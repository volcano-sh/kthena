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

package autoscaler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
)

// promStub starts an httptest.Server that mimics the Prometheus instant-query
// endpoint. It always responds to /api/v1/query with the provided payload after
// an optional delay, regardless of query string.
func promStub(t *testing.T, payload string, delay time.Duration, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/query") {
			http.NotFound(w, r)
			return
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestCollector() *MetricCollector {
	return &MetricCollector{
		Target: &workload.Target{
			TargetRef: corev1.ObjectReference{Name: "ut-target"},
		},
	}
}

func ptr[T any](value T) *T {
	return &value
}

func TestFetchPrometheusMetric(t *testing.T) {
	type want struct {
		err          bool
		errSubstring string
		value        float64
		// maxDuration bounds the total call duration; zero means unchecked.
		maxDuration time.Duration
	}

	cases := []struct {
		name string
		// serverURL overrides the stub URL; used to exercise unreachable hosts
		// without standing up a server.
		serverURL string
		payload   string
		status    int
		delay     time.Duration
		query     string
		want      want
	}{
		{
			name:    "scalar result",
			payload: `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"42.5"]}}`,
			query:   "up",
			want:    want{value: 42.5},
		},
		{
			name: "single sample vector",
			payload: `{
                "status":"success",
                "data":{
                    "resultType":"vector",
                    "result":[{"metric":{"job":"prom"},"value":[1700000000,"7"]}]
                }
            }`,
			query: `up{job="prom"}`,
			want:  want{value: 7.0},
		},
		{
			name:    "empty vector is rejected",
			payload: `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			query:   "missing_metric",
			want:    want{err: true, errSubstring: "single sample vector"},
		},
		{
			name: "multi sample vector is rejected",
			payload: `{
                "status":"success",
                "data":{
                    "resultType":"vector",
                    "result":[
                        {"metric":{"i":"a"},"value":[1700000000,"1"]},
                        {"metric":{"i":"b"},"value":[1700000000,"2"]}
                    ]
                }
            }`,
			query: "noisy",
			want:  want{err: true, errSubstring: "single sample vector"},
		},
		{
			name: "unsupported matrix result",
			payload: `{
                "status":"success",
                "data":{
                    "resultType":"matrix",
                    "result":[{"metric":{},"values":[[1700000000,"1"]]}]
                }
            }`,
			query: "range_query",
			want:  want{err: true, errSubstring: "unsupported prometheus query result type"},
		},
		{
			name:    "server returns http error",
			payload: `{"status":"error","errorType":"bad_data","error":"parse error"}`,
			status:  http.StatusBadRequest,
			query:   "!!!",
			want:    want{err: true},
		},
		{
			name:    "hanging server is bounded by ctx timeout",
			payload: `{"status":"success","data":{"resultType":"scalar","result":[0,"0"]}}`,
			delay:   (util.AutoscaleCtxTimeoutSeconds + 2) * time.Second,
			query:   "slow",
			want: want{
				err:         true,
				maxDuration: time.Duration(util.AutoscaleCtxTimeoutSeconds+2) * time.Second,
			},
		},
		{
			name:      "invalid server url",
			serverURL: "http://127.0.0.1:1",
			query:     "up",
			want:      want{err: true},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			serverURL := tc.serverURL
			if serverURL == "" {
				srv := promStub(t, tc.payload, tc.delay, tc.status)
				serverURL = srv.URL
			}

			collector := newTestCollector()
			start := time.Now()
			got, err := collector.fetchPrometheusMetric(context.Background(), &workload.PrometheusMetricSource{
				ServerURL: serverURL,
				Query:     tc.query,
			})
			elapsed := time.Since(start)

			if tc.want.err {
				require.Error(t, err)
				if tc.want.errSubstring != "" {
					assert.Contains(t, err.Error(), tc.want.errSubstring)
				}
			} else {
				require.NoError(t, err)
				assert.InDelta(t, tc.want.value, got, 1e-9)
			}

			if tc.want.maxDuration > 0 {
				assert.Less(t, elapsed, tc.want.maxDuration, "fetch should be bounded by ctx timeout")
			}
		})
	}
}

func TestExtractMetricFromFamilyAggregatesLabeledCounterAndGaugeSamples(t *testing.T) {
	cases := []struct {
		name        string
		metricName  string
		metricsText string
		want        float64
	}{
		{
			name:       "counter",
			metricName: "requests_total",
			metricsText: `# HELP requests_total Total requests.
# TYPE requests_total counter
requests_total{model="llama",status="success"} 2
requests_total{model="llama",status="error"} 3
`,
			want: 5,
		},
		{
			name:       "gauge",
			metricName: "queue_depth",
			metricsText: `# HELP queue_depth Current queue depth.
# TYPE queue_depth gauge
queue_depth{model="llama",role="prefill"} 4
queue_depth{model="llama",role="decode"} 6
`,
			want: 10,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			families, err := parsePrometheusFamilies(tc.metricsText, map[string][]string{
				tc.metricName: {tc.metricName},
			})
			require.NoError(t, err)

			mf, ok := families[tc.metricName]
			require.True(t, ok)

			got, snapshot, found, err := extractMetricFromFamily(mf, nil)
			require.NoError(t, err)
			require.True(t, found)
			require.Nil(t, snapshot)
			assert.InDelta(t, tc.want, got, 1e-9)
		})
	}
}

func TestExtractMetricFromFamilySkipsNilCounterAndGaugeSamples(t *testing.T) {
	cases := []struct {
		name   string
		family *io_prometheus_client.MetricFamily
		want   float64
	}{
		{
			name: "counter",
			family: &io_prometheus_client.MetricFamily{
				Name: ptr("requests_total"),
				Type: ptr(io_prometheus_client.MetricType_COUNTER),
				Metric: []*io_prometheus_client.Metric{
					nil,
					{},
					{Counter: &io_prometheus_client.Counter{Value: ptr(2.5)}},
				},
			},
			want: 2.5,
		},
		{
			name: "gauge",
			family: &io_prometheus_client.MetricFamily{
				Name: ptr("queue_depth"),
				Type: ptr(io_prometheus_client.MetricType_GAUGE),
				Metric: []*io_prometheus_client.Metric{
					nil,
					{},
					{Gauge: &io_prometheus_client.Gauge{Value: ptr(4.5)}},
				},
			},
			want: 4.5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, snapshot, found, err := extractMetricFromFamily(tc.family, nil)
			require.NoError(t, err)
			require.True(t, found)
			require.Nil(t, snapshot)
			assert.InDelta(t, tc.want, got, 1e-9)
		})
	}
}

func TestExtractMetricFromFamilyProcessesHistogramSample(t *testing.T) {
	const metricName = "request_duration_seconds"

	families, err := parsePrometheusFamilies(`# HELP request_duration_seconds Request duration.
# TYPE request_duration_seconds histogram
request_duration_seconds_bucket{le="1"} 1
request_duration_seconds_bucket{le="+Inf"} 1
request_duration_seconds_sum 1
request_duration_seconds_count 1
`, map[string][]string{
		metricName: {metricName},
	})
	require.NoError(t, err)

	mf, ok := families[metricName]
	require.True(t, ok)

	got, snapshot, found, err := extractMetricFromFamily(mf, nil)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, snapshot)

	// All observations are in the le="1" bucket, so the SLO percentile
	// extracted from the histogram diff resolves to that upper bound.
	assert.InDelta(t, 1.0, got, 1e-9)
}

func TestParsePrometheusFamiliesReturnsErrorOnMalformedPayload(t *testing.T) {
	_, err := parsePrometheusFamilies(`# HELP requests_total Total requests.
# TYPE requests_total counter
requests_total 2
not a valid prometheus metric line
`, map[string][]string{
		"requests_total": {"requests_total"},
	})

	require.Error(t, err)
}

func TestUpdateMetricsKeepsReadyPodMetricsWhenAnotherPodIsUnready(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "# TYPE queue_depth gauge\nqueue_depth 40\n# TYPE queue_depth_alt gauge\nqueue_depth_alt 40\n")
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err)

	labels := map[string]string{
		workload.ModelServingNameLabelKey: "model",
		workload.EntryLabelKey:            "true",
	}
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "default", Labels: labels},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "127.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	unreadyPod := readyPod.DeepCopy()
	unreadyPod.Name = "unready"
	unreadyPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, indexer.Add(readyPod))
	require.NoError(t, indexer.Add(unreadyPod))
	podLister := corelister.NewPodLister(indexer)

	policy := &workload.AutoscalingPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
	collector := NewMetricCollector(
		&workload.Target{TargetRef: corev1.ObjectReference{Namespace: "default", Name: "model"}},
		policy,
		algorithm.Metrics{"queue_depth": 10, "queue_depth_alt": 10},
	)

	unreadyCount, readyMetrics, _, err := collector.UpdateMetrics(context.Background(), podLister, map[string]workload.MetricSource{
		"queue_depth": {Pod: &workload.PodMetricSource{Name: "queue_depth", Port: int32(port)}},
		// This creates a second metric group selecting the same pods.
		"queue_depth_alt": {Pod: &workload.PodMetricSource{Name: "queue_depth_alt", Uri: "/metrics-alt", Port: int32(port)}},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), unreadyCount)
	// Expected behavior: scrape the ready pod even though another matching pod is unready.
	require.Equal(t, float64(40), readyMetrics["queue_depth"])
	require.Equal(t, float64(40), readyMetrics["queue_depth_alt"])
}
