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

package backend

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/sglang"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend/vllm"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

func TestGetMetricsProvider(t *testing.T) {
	tests := []struct {
		name    string
		engine  string
		wantErr bool
	}{
		{name: "vLLM is registered", engine: string(networkingv1alpha1.VLLM)},
		{name: "SGLang is registered", engine: string(networkingv1alpha1.SGLang)},
		{name: "engine names are case sensitive", engine: "vllm", wantErr: true},
		{name: "unknown engine", engine: "TGI", wantErr: true},
		{name: "empty engine", engine: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := GetMetricsProvider(tt.engine)
			if tt.wantErr {
				assert.EqualError(t, err, "unsupported engine: "+tt.engine)
				assert.Nil(t, provider)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, provider)
		})
	}
}

// metricsText renders a Prometheus text exposition with two gauges and a
// time-to-first-token histogram, using the metric names of one engine.
func metricsText(kvCacheUsage, waitingRequests, ttft string, ttftSum float64, ttftCount uint64) string {
	return fmt.Sprintf(`# TYPE %[1]s gauge
%[1]s 0.25
# TYPE %[2]s gauge
%[2]s 3
# TYPE %[3]s histogram
%[3]s_bucket{le="+Inf"} %[5]d
%[3]s_sum %[4]g
%[3]s_count %[5]d
`, kvCacheUsage, waitingRequests, ttft, ttftSum, ttftCount)
}

// newModelServerPod starts an HTTP server that answers /metrics with the given
// status and body, and returns a pod and port that point at it.
func newModelServerPod(t *testing.T, status int, body string) (*corev1.Pod, uint32) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.ParseUint(portStr, 10, 32)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "model-server-0"},
		Status:     corev1.PodStatus{PodIP: host},
	}
	return pod, uint32(port)
}

func histogram(sum float64, count uint64) *dto.Histogram {
	return &dto.Histogram{SampleSum: &sum, SampleCount: &count}
}

func TestGetPodMetrics(t *testing.T) {
	vllmMetrics := metricsText(vllm.KVCacheUsage, vllm.RequestWaitingNum, vllm.TTFT, 6, 8)
	sglangMetrics := metricsText(sglang.KVCacheUsage, sglang.RequestWaitingNum, sglang.TTFT, 6, 8)

	tests := []struct {
		name              string
		engine            string
		status            int
		body              string
		previousHistogram map[string]*dto.Histogram
		wantMetrics       map[string]float64
		wantHistogram     map[string]*dto.Histogram
	}{
		{
			name:   "merges vLLM gauges and histogram averages on the first scrape",
			engine: string(networkingv1alpha1.VLLM),
			status: http.StatusOK,
			body:   vllmMetrics,
			// Without a previous scrape the histogram average starts at zero.
			wantMetrics: map[string]float64{
				utils.KVCacheUsage:      0.25,
				utils.RequestWaitingNum: 3,
				utils.TTFT:              0,
			},
			wantHistogram: map[string]*dto.Histogram{utils.TTFT: histogram(6, 8)},
		},
		{
			name:              "averages the vLLM histogram over the period since the previous scrape",
			engine:            string(networkingv1alpha1.VLLM),
			status:            http.StatusOK,
			body:              vllmMetrics,
			previousHistogram: map[string]*dto.Histogram{utils.TTFT: histogram(2, 4)},
			// (6-2) seconds over (8-4) requests.
			wantMetrics: map[string]float64{
				utils.KVCacheUsage:      0.25,
				utils.RequestWaitingNum: 3,
				utils.TTFT:              1,
			},
			wantHistogram: map[string]*dto.Histogram{utils.TTFT: histogram(6, 8)},
		},
		{
			name:   "reads SGLang metric names for the SGLang engine",
			engine: string(networkingv1alpha1.SGLang),
			status: http.StatusOK,
			body:   sglangMetrics,
			wantMetrics: map[string]float64{
				utils.KVCacheUsage:      0.25,
				utils.RequestWaitingNum: 3,
				utils.TTFT:              0,
			},
			wantHistogram: map[string]*dto.Histogram{utils.TTFT: histogram(6, 8)},
		},
		{
			name:          "ignores metrics named for a different engine",
			engine:        string(networkingv1alpha1.SGLang),
			status:        http.StatusOK,
			body:          vllmMetrics,
			wantMetrics:   map[string]float64{},
			wantHistogram: map[string]*dto.Histogram{},
		},
		{
			name:   "returns nil for an unsupported engine",
			engine: "TGI",
			status: http.StatusOK,
			body:   vllmMetrics,
		},
		{
			name:   "returns nil when the pod's metrics endpoint fails",
			engine: string(networkingv1alpha1.VLLM),
			status: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, port := newModelServerPod(t, tt.status, tt.body)

			gotMetrics, gotHistogram := GetPodMetrics(tt.engine, pod, port, tt.previousHistogram)

			if tt.wantMetrics == nil {
				assert.Nil(t, gotMetrics)
				assert.Nil(t, gotHistogram)
				return
			}
			require.Len(t, gotMetrics, len(tt.wantMetrics))
			for name, want := range tt.wantMetrics {
				require.Contains(t, gotMetrics, name)
				assert.InDelta(t, want, gotMetrics[name], 1e-9, name)
			}
			require.Len(t, gotHistogram, len(tt.wantHistogram))
			for name, want := range tt.wantHistogram {
				require.Contains(t, gotHistogram, name)
				assert.Equal(t, want.GetSampleSum(), gotHistogram[name].GetSampleSum(), name)
				assert.Equal(t, want.GetSampleCount(), gotHistogram[name].GetSampleCount(), name)
			}
		})
	}
}
