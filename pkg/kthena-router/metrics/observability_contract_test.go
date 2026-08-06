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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
)

var (
	descriptorMetricNamePattern = regexp.MustCompile(`fqName: "([^"]+)"`)
	dashboardMetricNamePattern  = regexp.MustCompile(`\bkthena_router_[a-zA-Z0-9_:]*\b`)
	documentedMetricNamePattern = regexp.MustCompile("`(kthena_router_[a-zA-Z_:][a-zA-Z0-9_:]*)`")
)

func TestGrafanaDashboardMetricNamesAreRegistered(t *testing.T) {
	data, err := os.ReadFile(repositoryFile(t, "examples", "observability", "grafana-dashboard-score-plugins.json"))
	require.NoError(t, err)

	var dashboard interface{}
	require.NoError(t, json.Unmarshal(data, &dashboard), "dashboard must be valid JSON")

	references := make(map[string]struct{})
	collectDashboardMetricNames(dashboard, references)
	require.NotEmpty(t, references, "dashboard must contain router metric references")
	requireMetricReferencesRegistered(t, references, registeredRouterMetricNames(t))
}

func TestDocumentedRouterMetricNamesAreRegistered(t *testing.T) {
	data, err := os.ReadFile(repositoryFile(t, "docs", "kthena", "docs", "user-guide", "router-observability.md"))
	require.NoError(t, err)

	references := make(map[string]struct{})
	for _, match := range documentedMetricNamePattern.FindAllSubmatch(data, -1) {
		references[string(match[1])] = struct{}{}
	}
	require.NotEmpty(t, references, "router observability documentation must name router metrics")
	requireMetricReferencesRegistered(t, references, registeredRouterMetricNames(t))
}

func registeredRouterMetricNames(t *testing.T) map[string]struct{} {
	t.Helper()

	names := make(map[string]struct{})
	metricsValue := reflect.ValueOf(DefaultMetrics).Elem()
	for i := 0; i < metricsValue.NumField(); i++ {
		collector, ok := prometheusCollector(metricsValue.Field(i))
		if !ok {
			continue
		}
		_, alreadyRegistered := prometheus.DefaultRegisterer.Register(collector).(prometheus.AlreadyRegisteredError)
		require.True(t, alreadyRegistered, "router metric collector is not registered with the default registry")

		descriptors := make(chan *prometheus.Desc, 16)
		collector.Describe(descriptors)
		close(descriptors)
		for descriptor := range descriptors {
			match := descriptorMetricNamePattern.FindStringSubmatch(descriptor.String())
			require.Len(t, match, 2, "extract metric name from descriptor %s", descriptor)
			if strings.HasPrefix(match[1], "kthena_router_") {
				names[match[1]] = struct{}{}
			}
		}
	}

	require.NotEmpty(t, names, "router metrics registry must expose descriptors")
	return names
}

func prometheusCollector(field reflect.Value) (prometheus.Collector, bool) {
	if field.CanInterface() {
		if collector, ok := field.Interface().(prometheus.Collector); ok {
			return collector, true
		}
	}
	if field.CanAddr() && field.Addr().CanInterface() {
		if collector, ok := field.Addr().Interface().(prometheus.Collector); ok {
			return collector, true
		}
	}
	return nil, false
}

func collectDashboardMetricNames(value interface{}, names map[string]struct{}) {
	switch value := value.(type) {
	case string:
		for _, name := range dashboardMetricNamePattern.FindAllString(value, -1) {
			names[name] = struct{}{}
		}
	case []interface{}:
		for _, item := range value {
			collectDashboardMetricNames(item, names)
		}
	case map[string]interface{}:
		for _, item := range value {
			collectDashboardMetricNames(item, names)
		}
	}
}

func requireMetricReferencesRegistered(t *testing.T, references, registered map[string]struct{}) {
	t.Helper()

	var unknown []string
	for reference := range references {
		require.True(t, model.MetricNameRE.MatchString(reference), "%q is not a valid Prometheus metric name", reference)
		if !isRegisteredMetricReference(reference, registered) {
			unknown = append(unknown, reference)
		}
	}
	sort.Strings(unknown)
	require.Empty(t, unknown, "metric references do not correspond to registered router metrics")
}

func isRegisteredMetricReference(reference string, registered map[string]struct{}) bool {
	if _, ok := registered[reference]; ok {
		return true
	}
	for _, suffix := range []string{"_bucket", "_count", "_sum"} {
		if strings.HasSuffix(reference, suffix) {
			_, ok := registered[strings.TrimSuffix(reference, suffix)]
			return ok
		}
	}
	return false
}

func repositoryFile(t *testing.T, elements ...string) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve repository root")
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
	return filepath.Join(append([]string{root}, elements...)...)
}
