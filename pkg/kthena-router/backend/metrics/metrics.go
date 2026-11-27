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
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"k8s.io/klog/v2"
)

var httpClient = &http.Client{
	Timeout: 5 * time.Second,
}

func HTTPClient() *http.Client {
	return httpClient
}

func PodEndpointURL(podIP string, port uint32, path string) string {
	return "http://" + net.JoinHostPort(podIP, strconv.FormatUint(uint64(port), 10)) + path
}

// This function refers to aibrix(https://github.com/vllm-project/aibrix/blob/main/pkg/metrics/utils.go)
func ParseMetricsURL(url string) (map[string]*dto.MetricFamily, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch metrics from %s: %v", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.Errorf("failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch metrics from %s: HTTP %d", url, resp.StatusCode)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	allMetrics, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error parsing metric families: %v", err)
	}
	return allMetrics, nil
}

func LastPeriodAvg(previous, current *dto.Histogram) float64 {
	previousSum := previous.GetSampleSum()
	previousCount := previous.GetSampleCount()

	currentSum := current.GetSampleSum()
	currentCount := current.GetSampleCount()
	if currentCount == 0 {
		return 0
	}

	// Prometheus histogram sums and counts are cumulative for non-negative
	// observations. If either value decreases, the backend process has likely
	// restarted and reset its metrics. In that case, use the samples collected
	// since the reset instead of subtracting values from the previous process.
	if currentCount < previousCount || currentSum < previousSum {
		return currentSum / float64(currentCount)
	}

	deltaSum := currentSum - previousSum
	deltaCount := currentCount - previousCount

	if deltaCount == 0 {
		return 0
	}

	return deltaSum / float64(deltaCount)
}

// MergeHistograms adds up the sample counts and sums across every series,
// so a pod that exports one series per engine is counted whole.
func MergeHistograms(series []*dto.Metric) *dto.Histogram {
	var count uint64
	var sum float64
	for _, metric := range series {
		count += metric.GetHistogram().GetSampleCount()
		sum += metric.GetHistogram().GetSampleSum()
	}
	return &dto.Histogram{SampleCount: &count, SampleSum: &sum}
}

// SeriesKey names one series of a histogram by its labels.
func SeriesKey(name string, metric *dto.Metric) string {
	pairs := make([]string, 0, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		pairs = append(pairs, label.GetName()+"="+strconv.Quote(label.GetValue()))
	}
	slices.Sort(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// SeriesPeriodAvg averages the samples each series gained since the previous scrape.
func SeriesPeriodAvg(name string, series []*dto.Metric, previous map[string]*dto.Histogram) float64 {
	var sum float64
	var count uint64
	for _, metric := range series {
		current := metric.GetHistogram()
		seriesSum, seriesCount := current.GetSampleSum(), current.GetSampleCount()
		// A series without a previous value, or with smaller totals, started or restarted since the last scrape.
		if prev := previous[SeriesKey(name, metric)]; prev != nil && seriesCount >= prev.GetSampleCount() && seriesSum >= prev.GetSampleSum() {
			seriesSum -= prev.GetSampleSum()
			seriesCount -= prev.GetSampleCount()
		}
		sum += seriesSum
		count += seriesCount
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}
