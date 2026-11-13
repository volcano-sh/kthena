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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler"
)

func TestRouter_DefaultLoraAffinity(t *testing.T) {
	tests := []struct {
		name              string
		model             string
		loadedModels      []string
		wantStatus        int
		wantUpstreamModel string
	}{
		{"loaded adapter", "adapter-a", []string{"fair-model-base", "adapter-a"}, http.StatusOK, "adapter-a"},
		{"unloaded adapter", "adapter-a", []string{"fair-model-base", "adapter-b"}, http.StatusBadRequest, ""},
		{"adapter before model discovery", "adapter-a", nil, http.StatusBadRequest, ""},
		{"base alias before model discovery", "fair-model", nil, http.StatusOK, "fair-model-base"},
		{"base alias with loaded models", "fair-model", []string{"fair-model-base", "adapter-a"}, http.StatusOK, "fair-model-base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			router, store, backend := setupFairnessTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body ModelRequest
				if assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					assert.Equal(t, tt.wantUpstreamModel, body["model"])
				}
				fmt.Fprint(w, `{"id":"lora-response"}`)
			}))
			defer backend.Close()
			router.scheduler = scheduler.NewScheduler(store, nil)
			route := &aiv1alpha1.ModelRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "mr-fair", Namespace: "default"},
				Spec: aiv1alpha1.ModelRouteSpec{
					ModelName:    "fair-model",
					LoraAdapters: []string{"adapter-a"},
					Rules:        []*aiv1alpha1.Rule{{TargetModels: []*aiv1alpha1.TargetModel{{ModelServerName: "ms-fair"}}}},
				},
			}
			require.NoError(t, store.AddOrUpdateModelRoute(route))
			pods, err := store.GetPodsByModelServer(types.NamespacedName{Name: "ms-fair", Namespace: "default"})
			require.NoError(t, err)
			require.Len(t, pods, 1)
			pods[0].UpdateModels(tt.loadedModels)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"prompt":"hello"}`, tt.model)))
			c.Request.Header.Set("Content-Type", "application/json")
			router.HandlerFunc()(c)

			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.wantStatus == http.StatusOK {
				assert.EqualValues(t, 1, calls.Load())
				assert.Contains(t, w.Body.String(), "lora-response")
			} else {
				assert.Zero(t, calls.Load(), "an unavailable adapter must never reach the backend")
				assert.Contains(t, w.Body.String(), "lora-affinity")
			}
		})
	}
}
