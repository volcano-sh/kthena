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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
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
