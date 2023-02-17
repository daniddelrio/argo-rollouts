package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/utils/defaults"
	"github.com/argoproj/argo-rollouts/utils/evaluate"
	metricutil "github.com/argoproj/argo-rollouts/utils/metric"
	timeutil "github.com/argoproj/argo-rollouts/utils/time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var unixNow = func() int64 { return timeutil.Now().Unix() }

const (
	// ProviderType indicates the provider is datadog
	ProviderType            = "Datadog"
	DatadogTokensSecretName = "datadog"
	DatadogApiKey           = "api-key"
	DatadogAppKey           = "app-key"
	DatadogAddress          = "address"
)

// Provider contains all the required components to run a Datadog query
// Implements the Provider Interface
type Provider struct {
	logCtx log.Entry
	config datadogConfig
}

type datadogQueryAttributes struct {
	From    int64               `json:"from"`
	To      int64               `json:"to"`
	Queries []map[string]string `json:"queries"`
}

type datadogQuery struct {
	Attributes datadogQueryAttributes `json:"attributes"`
	QueryType  string                 `json:"type"`
}

type datadogResponse struct {
	Data struct {
		Attributes struct {
			Values [][]float64
			Times  []int64
		}
		Errors string
	}
}

type datadogConfig struct {
	Address string `yaml:"address,omitempty"`
	ApiKey  string `yaml:"api-key,omitempty"`
	AppKey  string `yaml:"app-key,omitempty"`
}

// Type incidates provider is a Datadog provider
func (p *Provider) Type() string {
	return ProviderType
}

// GetMetadata returns any additional metadata which needs to be stored & displayed as part of the metrics result.
func (p *Provider) GetMetadata(metric v1alpha1.Metric) map[string]string {
	return nil
}

func (p *Provider) Run(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric) v1alpha1.Measurement {
	startTime := timeutil.MetaNow()

	// Measurement to pass back
	measurement := v1alpha1.Measurement{
		StartedAt: &startTime,
	}

	endpoint := "https://api.datadoghq.com/api/v2/query/timeseries"
	if p.config.Address != "" {
		endpoint = p.config.Address + "/api/v2/query/timeseries"
	}

	url, err := url.Parse(endpoint)
	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	now := unixNow()
	var interval int64 = 300
	if metric.Provider.Datadog.Interval != "" {
		expDuration, err := metric.Provider.Datadog.Interval.Duration()
		if err != nil {
			return metricutil.MarkMeasurementError(measurement, err)
		}
		// Convert to seconds as DataDog expects unix timestamp
		interval = int64(expDuration.Seconds())
	}

	queryBody, _ := json.Marshal(datadogQuery{
		QueryType: "timeseries_request",
		Attributes: datadogQueryAttributes{
			From: now - interval,
			To:   now,
			Queries: []map[string]string{{
				"data_source": "metrics",
				"query":       metric.Provider.Datadog.Query,
			}},
		},
	})

	request := &http.Request{Method: "POST"}
	request.URL = url
	request.Body = io.NopCloser(bytes.NewReader(queryBody))
	request.Header = make(http.Header)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("DD-API-KEY", p.config.ApiKey)
	request.Header.Set("DD-APPLICATION-KEY", p.config.AppKey)

	// Send Request
	httpClient := &http.Client{
		Timeout: time.Duration(10) * time.Second,
	}
	response, err := httpClient.Do(request)

	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	value, status, err := p.parseResponse(metric, response)
	if err != nil {
		return metricutil.MarkMeasurementError(measurement, err)
	}

	measurement.Value = value
	measurement.Phase = status
	finishedTime := timeutil.MetaNow()
	measurement.FinishedAt = &finishedTime

	return measurement
}

func (p *Provider) parseResponse(metric v1alpha1.Metric, response *http.Response) (string, v1alpha1.AnalysisPhase, error) {

	bodyBytes, err := io.ReadAll(response.Body)

	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Received no bytes in response: %v", err)
	}

	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("received authentication error response code: %v %s", response.StatusCode, string(bodyBytes))
	} else if response.StatusCode != http.StatusOK {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("received non 2xx response code: %v %s", response.StatusCode, string(bodyBytes))
	}

	var res datadogResponse
	err = json.Unmarshal(bodyBytes, &res)
	if err != nil {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Could not parse JSON body: %v", err)
	}

	// Handle an error returned by Datadog
	if res.Data.Errors != "" {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("There were errors in your query: %v", res.Data.Errors)
	}

	// Handle an empty query result
	if reflect.ValueOf(res.Data.Attributes).IsZero() || len(res.Data.Attributes.Values) == 0 || len(res.Data.Attributes.Times) == 0 {
		var nilFloat64 *float64
		status, err := evaluate.EvaluateResult(nilFloat64, metric, p.logCtx)
		attributesBytes, jsonErr := json.Marshal(res.Data.Attributes)
		if jsonErr != nil {
			return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Failed to marshall JSON empty series: %v", jsonErr)
		}

		return string(attributesBytes), status, err
	}

	// Handle a populated query result
	attributes := res.Data.Attributes
	datapoint := attributes.Values[0]
	timepoint := attributes.Times[len(attributes.Times)-1]
	if timepoint == 0 {
		return "", v1alpha1.AnalysisPhaseError, fmt.Errorf("Datapoint does not have a corresponding time value")
	}

	value := datapoint[len(datapoint)-1]
	status, err := evaluate.EvaluateResult(value, metric, p.logCtx)
	return strconv.FormatFloat(value, 'f', -1, 64), status, err
}

// Resume should not be used the Datadog provider since all the work should occur in the Run method
func (p *Provider) Resume(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	p.logCtx.Warn("Datadog provider should not execute the Resume method")
	return measurement
}

// Terminate should not be used the Datadog provider since all the work should occur in the Run method
func (p *Provider) Terminate(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, measurement v1alpha1.Measurement) v1alpha1.Measurement {
	p.logCtx.Warn("Datadog provider should not execute the Terminate method")
	return measurement
}

// GarbageCollect is a no-op for the Datadog provider
func (p *Provider) GarbageCollect(run *v1alpha1.AnalysisRun, metric v1alpha1.Metric, limit int) error {
	return nil
}

func lookupKeysInEnv(keys []string) map[string]string {
	valuesByKey := make(map[string]string)
	for i := range keys {
		key := keys[i]
		formattedKey := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if value, ok := os.LookupEnv(fmt.Sprintf("DD_%s", formattedKey)); ok {
			valuesByKey[key] = value
		}
	}
	return valuesByKey
}

func NewDatadogProvider(logCtx log.Entry, kubeclientset kubernetes.Interface) (*Provider, error) {
	ns := defaults.Namespace()

	apiKey := ""
	appKey := ""
	address := ""
	secretKeys := []string{DatadogApiKey, DatadogAppKey, DatadogAddress}
	envValuesByKey := lookupKeysInEnv(secretKeys)
	if len(envValuesByKey) == len(secretKeys) {
		apiKey = envValuesByKey[DatadogApiKey]
		appKey = envValuesByKey[DatadogAppKey]
		address = envValuesByKey[DatadogAddress]
	} else {
		secret, err := kubeclientset.CoreV1().Secrets(ns).Get(context.TODO(), DatadogTokensSecretName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		apiKey = string(secret.Data[DatadogApiKey])
		appKey = string(secret.Data[DatadogAppKey])
		if _, hasAddress := secret.Data[DatadogAddress]; hasAddress {
			address = string(secret.Data[DatadogAddress])
		}
	}

	if apiKey != "" && appKey != "" {
		return &Provider{
			logCtx: logCtx,
			config: datadogConfig{
				Address: address,
				ApiKey:  apiKey,
				AppKey:  appKey,
			},
		}, nil
	} else {
		return nil, errors.New("API or App token not found")
	}

}
