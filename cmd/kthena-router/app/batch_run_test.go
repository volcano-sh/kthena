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

package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/batch"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	routerpkg "github.com/volcano-sh/kthena/pkg/kthena-router/router"
)

// fakeModelServer answers chat completions and remembers every prompt and request ID it was sent.
type fakeModelServer struct {
	server     *httptest.Server
	mu         sync.Mutex
	prompts    map[string]int
	requestIDs []string
}

func newFakeModelServer(t *testing.T) *fakeModelServer {
	t.Helper()
	m := &fakeModelServer{prompts: map[string]int{}}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &request)
		prompt := ""
		if len(request.Messages) > 0 {
			prompt = request.Messages[0].Content
		}
		m.mu.Lock()
		m.prompts[prompt]++
		m.requestIDs = append(m.requestIDs, r.Header.Get("X-Request-ID"))
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"re: %s"},"finish_reason":"stop"}]}`, prompt)
	}))
	t.Cleanup(m.server.Close)
	return m
}

// routedStore holds one model server with one pod, and a ModelRoute for model m that is bound to gateway when one is named.
func routedStore(t *testing.T, backend *httptest.Server, gateway string) datastore.Store {
	t.Helper()
	address, err := url.Parse(backend.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(address.Port())
	require.NoError(t, err)
	model := "base"
	server := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "ms-1", Namespace: "default"},
		Spec:       aiv1alpha1.ModelServerSpec{Model: &model, WorkloadPort: aiv1alpha1.WorkloadPort{Port: int32(port)}, InferenceEngine: "vLLM"},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"}, Status: corev1.PodStatus{PodIP: address.Hostname(), Phase: corev1.PodRunning}}
	route := &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "mr-1", Namespace: "default"},
		Spec:       aiv1alpha1.ModelRouteSpec{ModelName: "m", Rules: []*aiv1alpha1.Rule{{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-1"}}}}},
	}
	if gateway != "" {
		kind := gatewayv1.Kind("Gateway")
		route.Spec.ParentRefs = []gatewayv1.ParentReference{{Kind: &kind, Name: gatewayv1.ObjectName(gateway)}}
	}
	store := datastore.New()
	if gateway != "" {
		// A route bound to a Gateway only matches while that Gateway exists.
		listener := gatewayv1.Listener{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}
		require.NoError(t, store.AddOrUpdateGateway(&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: gateway},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "kthena-router", Listeners: []gatewayv1.Listener{listener}},
		}))
	}
	require.NoError(t, store.AddOrUpdateModelServer(server, sets.New(types.NamespacedName{Name: "pod-1", Namespace: "default"})))
	require.NoError(t, store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{server}))
	require.NoError(t, store.AddOrUpdateModelRoute(route))
	return store
}

func batchLine(prompt string) string {
	return `{"custom_id":"` + prompt + `","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"` + prompt + `"}]}}` + "\n"
}

// A Gateway-bound ModelRoute only matches a request that carries its Gateway, so in Gateway mode the lines reach the model only if the batch's listener is replayed.
func TestABatchRunsThroughTheRouter(t *testing.T) {
	tests := []struct {
		name     string
		gateway  string
		listener func(*testing.T, *routerpkg.Router) http.Handler
	}{
		{name: "default listener", listener: defaultListener},
		{name: "gateway listener with a gateway-bound route", gateway: "gateway", listener: gatewayListener},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enableBatch(t)
			t.Setenv("ACCESS_LOG_ENABLED", "false")
			model := newFakeModelServer(t)
			r := NewRouter(routedStore(t, model.server, tt.gateway), testRouterConfig, common.NewTransportRegistry())
			done := startBatch(t.Context(), r)
			require.NotNil(t, done)
			t.Cleanup(func() { waitForBatch(done, 15*time.Second) })
			handler := tt.listener(t, r)

			uploaded := serve(handler, uploadRequest(t, batchLine("q1")+batchLine("q2"), ""))
			require.Equal(t, http.StatusOK, uploaded.Code, uploaded.Body.String())
			input, _ := decodeBody(t, uploaded)["id"].(string)
			create := httptest.NewRequest(http.MethodPost, "/v1/batches", bytes.NewBufferString(`{"input_file_id":"`+input+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`))
			create.Header.Set("Content-Type", "application/json")
			created := serve(handler, create)
			require.Equal(t, http.StatusOK, created.Code, created.Body.String())
			id, _ := decodeBody(t, created)["id"].(string)

			var job map[string]any
			require.Eventually(t, func() bool {
				job = decodeBody(t, serve(handler, request(http.MethodGet, "/v1/batches/"+id, "")))
				return job["status"] == "completed"
			}, 30*time.Second, 20*time.Millisecond, "the worker runs the batch to the end")
			if errors, ok := job["error_file_id"].(string); ok {
				t.Fatalf("lines failed: %s", serve(handler, request(http.MethodGet, "/v1/files/"+errors+"/content", "")).Body.String())
			}
			assert.Equal(t, map[string]any{"total": float64(2), "completed": float64(2), "failed": float64(0)}, job["request_counts"])
			assert.Nil(t, job["error_file_id"])

			content := serve(handler, request(http.MethodGet, "/v1/files/"+job["output_file_id"].(string)+"/content", ""))
			require.Equal(t, http.StatusOK, content.Code, content.Body.String())
			answers := map[string]string{}
			scanner := bufio.NewScanner(strings.NewReader(content.Body.String()))
			for scanner.Scan() {
				var line struct {
					CustomID string `json:"custom_id"`
					Response struct {
						StatusCode int `json:"status_code"`
						Body       struct {
							Choices []struct {
								Message struct {
									Content string `json:"content"`
								} `json:"message"`
							} `json:"choices"`
						} `json:"body"`
					} `json:"response"`
				}
				require.NoError(t, json.Unmarshal(scanner.Bytes(), &line), scanner.Text())
				require.Equal(t, http.StatusOK, line.Response.StatusCode)
				require.Len(t, line.Response.Body.Choices, 1)
				answers[line.CustomID] = line.Response.Body.Choices[0].Message.Content
			}
			assert.Equal(t, map[string]string{"q1": "re: q1", "q2": "re: q2"}, answers, "every line came back once, with the model's own answer")

			model.mu.Lock()
			defer model.mu.Unlock()
			assert.Equal(t, map[string]int{"q1": 1, "q2": 1}, model.prompts, "the model was asked each line once")
			for _, requestID := range model.requestIDs {
				assert.True(t, strings.HasPrefix(requestID, "batch_req_"), "the model sees the line's request ID, got %q", requestID)
			}
		})
	}
}

func TestBatchIdentityStandsInForAuthentication(t *testing.T) {
	quietGin(t)
	engine := gin.New()
	engine.Use(batchIdentity())
	engine.Any("/*path", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"user":     c.GetString(common.UserIdKey),
			"gateway":  c.GetString(routerpkg.GatewayKey),
			"listener": c.GetString(routerpkg.GatewayListenerNameKey),
			"port":     c.GetInt(routerpkg.GatewayListenerPortKey),
		})
	})

	refused := serve(engine, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	assert.Equal(t, http.StatusForbidden, refused.Code, "a request without a batch identity is refused")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	identity := batch.Identity{Tenant: "t", Subject: "alice", Origin: batch.Origin{Gateway: "default/gateway", Listener: "http", Port: 80}}
	accepted := serve(engine, req.WithContext(batch.WithIdentity(req.Context(), identity)))
	require.Equal(t, http.StatusOK, accepted.Code)
	assert.Equal(t, map[string]any{"user": "alice", "gateway": "default/gateway", "listener": "http", "port": float64(80)}, decodeBody(t, accepted))
}

func TestWaitForBatchIsBounded(t *testing.T) {
	waitForBatch(nil, time.Hour)
	closed := make(chan struct{})
	close(closed)
	waitForBatch(closed, time.Hour)
	start := time.Now()
	waitForBatch(make(chan struct{}), 20*time.Millisecond)
	assert.Less(t, time.Since(start), 5*time.Second, "a worker that never stops does not hold the router")
}
