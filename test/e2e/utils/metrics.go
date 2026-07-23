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

package utils

import dto "github.com/prometheus/client_model/go"

// MatchMetricLabels reports whether all want labels match the metric label pairs.
func MatchMetricLabels(metricLabels []*dto.LabelPair, wantLabels map[string]string) bool {
	labelMap := make(map[string]string)
	for _, lp := range metricLabels {
		labelMap[lp.GetName()] = lp.GetValue()
	}
	for k, v := range wantLabels {
		if labelMap[k] != v {
			return false
		}
	}
	return true
}

// GetCounterValue returns the sum of counter values matching the named metric and labels.
func GetCounterValue(metrics map[string]*dto.MetricFamily, metricName string, labels map[string]string) float64 {
	mf, ok := metrics[metricName]
	if !ok {
		return 0
	}
	var value float64
	for _, m := range mf.GetMetric() {
		if MatchMetricLabels(m.GetLabel(), labels) {
			value += m.GetCounter().GetValue()
		}
	}
	return value
}

// GetGaugeValue returns the sum of gauge values matching the named metric and labels.
func GetGaugeValue(metrics map[string]*dto.MetricFamily, metricName string, labels map[string]string) float64 {
	mf, ok := metrics[metricName]
	if !ok {
		return 0
	}
	var value float64
	for _, m := range mf.GetMetric() {
		if MatchMetricLabels(m.GetLabel(), labels) {
			value += m.GetGauge().GetValue()
		}
	}
	return value
}

// GetHistogramCount returns the sum of histogram sample counts matching the named metric and labels.
func GetHistogramCount(metrics map[string]*dto.MetricFamily, metricName string, labels map[string]string) uint64 {
	mf, ok := metrics[metricName]
	if !ok {
		return 0
	}
	var count uint64
	for _, m := range mf.GetMetric() {
		if MatchMetricLabels(m.GetLabel(), labels) {
			count += m.GetHistogram().GetSampleCount()
		}
	}
	return count
}
