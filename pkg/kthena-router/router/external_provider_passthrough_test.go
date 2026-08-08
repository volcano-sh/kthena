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
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/accesslog"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestExternalProviderForwardsOpaqueRequestContent(t *testing.T) {
	tests := []struct {
		name         string
		providerType aiv1alpha1.ExternalProviderType
		path         string
		requestBody  string
		responseBody string
		wantFields   []string
	}{
		{
			name:         "openai chat tools and image",
			providerType: aiv1alpha1.OpenAI,
			path:         "/v1/chat/completions",
			requestBody:  `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]},{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
			responseBody: `{"id":"chat-pass","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			wantFields:   []string{"messages", "tools"},
		},
		{
			name:         "openai responses tools image and file",
			providerType: aiv1alpha1.OpenAI,
			path:         "/v1/responses",
			requestBody:  `{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/cat.png"},{"type":"input_file","file_id":"file-1"}]},{"type":"function_call_output","call_id":"call-1","output":{"ok":true}},{"type":"additional_tools","tools":[{"type":"custom","name":"shell"}]},{"type":"custom_tool_call_output","call_id":"call-2","output":"ok"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			responseBody: `{"id":"resp-pass","object":"response","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			wantFields:   []string{"input", "tools"},
		},
		{
			name:         "anthropic tools image and document",
			providerType: aiv1alpha1.Anthropic,
			path:         "/v1/messages",
			requestBody:  `{"max_tokens":8,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},{"type":"document","source":{"type":"text","media_type":"text/plain","data":"hello"}}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"q":"x"}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`,
			responseBody: `{"id":"msg-pass","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantFields:   []string{"messages", "tools"},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls int
			var got map[string]interface{}
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.responseBody)
			}))
			defer upstream.Close()

			fixture := newExternalPassthroughFixture(t, fmt.Sprintf("passthrough-%d", i), tt.providerType, upstream.URL)
			body := addPassthroughModel(tt.requestBody, fixture.clientModel)
			var original map[string]interface{}
			require.NoError(t, json.Unmarshal([]byte(body), &original))

			w, accessCtx := executeExternalPassthroughRequest(t, fixture, tt.path, body)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, 1, upstreamCalls)
			assert.Equal(t, fixture.providerModel, got["model"])
			for _, field := range tt.wantFields {
				assert.Equal(t, original[field], got[field], field)
			}
			assert.Nil(t, accessCtx.Error)
		})
	}
}

type externalPassthroughFixture struct {
	router        *Router
	clientModel   string
	providerModel string
}

func newExternalPassthroughFixture(t *testing.T, suffix string, providerType aiv1alpha1.ExternalProviderType, baseURL string) externalPassthroughFixture {
	t.Helper()
	store := datastore.New()
	router := NewRouter(store, "../scheduler/testdata/configmap.yaml")
	clientModel := "passthrough-client-" + suffix
	providerModel := "passthrough-upstream-" + suffix
	providerName := "passthrough-provider-" + suffix
	secretName := "passthrough-secret-" + suffix
	routeName := "passthrough-route-" + suffix
	headers := map[string]string{}
	if providerType == aiv1alpha1.Anthropic {
		headers["anthropic-version"] = "2023-06-01"
	}

	require.NoError(t, store.AddOrUpdateExternalModelProvider(&aiv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: "default"},
		Spec: aiv1alpha1.ExternalModelProviderSpec{
			ProviderType:       providerType,
			BaseURL:            baseURL,
			Model:              &providerModel,
			InsecureSkipVerify: true,
			Auth: &aiv1alpha1.ProviderAuth{SecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "api-key",
			}},
			Headers: headers,
		},
	}))
	require.NoError(t, store.AddOrUpdateSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("provider-key")},
	}))
	require.NoError(t, store.AddOrUpdateModelRoute(&aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: "default"},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: clientModel,
			Rules: []*aiv1alpha1.Rule{{TargetModels: []*aiv1alpha1.TargetModel{{
				ExternalModelProviderName: providerName,
			}}}},
		},
	}))

	return externalPassthroughFixture{router: router, clientModel: clientModel, providerModel: providerModel}
}

func addPassthroughModel(body, model string) string {
	return fmt.Sprintf(`{"model":%q,%s`, model, body[1:])
}

func executeExternalPassthroughRequest(t *testing.T, fixture externalPassthroughFixture, path, body string) (*httptest.ResponseRecorder, *accesslog.AccessLogContext) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	accessCtx := accesslog.NewAccessLogContext("external-passthrough", http.MethodPost, path, c.Request.Proto, fixture.clientModel)
	c.Set(accesslog.AccessLogContextKey, accessCtx)
	fixture.router.HandlerFunc()(c)
	return w, accessCtx
}
