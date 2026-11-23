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

package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// TestRouter_HandlerFunc_CompletionsPromptForms checks that every prompt form of
// the Completions API is routed, and that the prompt reaches the backend as sent.
func TestRouter_HandlerFunc_CompletionsPromptForms(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		wantStatus int
	}{
		{name: "string", prompt: `"hello"`, wantStatus: http.StatusOK},
		{name: "list of strings", prompt: `["hello","world"]`, wantStatus: http.StatusOK},
		{name: "token IDs", prompt: `[15339,1917]`, wantStatus: http.StatusOK},
		{name: "list of token-ID lists", prompt: `[[15339],[1917]]`, wantStatus: http.StatusOK},
		{name: "number", prompt: `123`, wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu             sync.Mutex
				receivedPrompt json.RawMessage
			)
			// setupTestRouter's backend only forwards chat and responses paths, so
			// this test runs its own backend for /v1/completions.
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/completions" {
					// The router also polls the pod, e.g. /metrics and /v1/models.
					return
				}
				body, _ := io.ReadAll(r.Body)
				var req struct {
					Prompt json.RawMessage `json:"prompt"`
				}
				_ = json.Unmarshal(body, &req)
				mu.Lock()
				receivedPrompt = req.Prompt
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion"}`)
			}))
			defer backend.Close()
			gin.SetMode(gin.TestMode)
			store := datastore.New()
			router := NewRouter(store, "../scheduler/testdata/configmap.yaml", common.NewTransportRegistry())

			backendURL, err := url.Parse(backend.URL)
			require.NoError(t, err)
			backendPort, err := strconv.Atoi(backendURL.Port())
			require.NoError(t, err)
			modelServer := &aiv1alpha1.ModelServer{
				ObjectMeta: v1.ObjectMeta{Name: "ms-completions", Namespace: "default"},
				Spec: aiv1alpha1.ModelServerSpec{
					WorkloadPort:    aiv1alpha1.WorkloadPort{Port: int32(backendPort)},
					InferenceEngine: "vLLM",
				},
			}
			pod := &corev1.Pod{
				ObjectMeta: v1.ObjectMeta{Name: "pod-completions", Namespace: "default"},
				Status:     corev1.PodStatus{PodIP: backendURL.Hostname(), Phase: corev1.PodRunning},
			}
			modelRoute := &aiv1alpha1.ModelRoute{
				ObjectMeta: v1.ObjectMeta{Name: "mr-completions", Namespace: "default"},
				Spec: aiv1alpha1.ModelRouteSpec{
					ModelName: "completions-model",
					Rules:     []*aiv1alpha1.Rule{{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-completions"}}}},
				},
			}
			require.NoError(t, store.AddOrUpdateModelServer(modelServer, sets.New(types.NamespacedName{Name: "pod-completions", Namespace: "default"})))
			require.NoError(t, store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{modelServer}))
			require.NoError(t, store.AddOrUpdateModelRoute(modelRoute))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			body := fmt.Sprintf(`{"model":"completions-model","prompt":%s,"max_tokens":4}`, tt.prompt)
			c.Request, _ = http.NewRequest(http.MethodPost, "/v1/completions", bytes.NewBufferString(body))
			c.Request.Header.Set("Content-Type", "application/json")

			router.HandlerFunc()(c)

			assert.Equal(t, tt.wantStatus, w.Code, "body: %s", w.Body.String())
			mu.Lock()
			defer mu.Unlock()
			if tt.wantStatus == http.StatusOK {
				assert.JSONEq(t, tt.prompt, string(receivedPrompt), "the prompt must reach the backend unchanged")
			} else {
				assert.Nil(t, receivedPrompt, "a rejected request must not reach the backend")
			}
		})
	}
}
