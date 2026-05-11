/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"fmt"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

var httpClient = &http.Client{
	Timeout: 5 * time.Second,
}

func HTTPClient() *http.Client {
	return httpClient
}

// This function refer to aibrix(https://github.com/vllm-project/aibrix/blob/main/pkg/metrics/utils.go)
func ParseMetricsURL(url string) (map[string]*dto.MetricFamily, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch metrics from %s: %v", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("failed to close response body: %v", err)
		}
	}()

	parser := expfmt.NewTextParser(model.UTF8Validation)
	allMetrics, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error parsing metric families: %v\n", err)
	}
	return allMetrics, nil
}

func LastPeriodAvg(previous, current *dto.Histogram) float64 {
	previousSum := previous.GetSampleSum()
	previousCount := previous.GetSampleCount()

	currentSum := current.GetSampleSum()
	currentCount := current.GetSampleCount()

	deltaSum := currentSum - previousSum
	deltaCount := currentCount - previousCount

	if deltaCount == 0 {
		return 0
	}

	return deltaSum / float64(deltaCount)
}
