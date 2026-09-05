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

package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestCreateLWSPatch(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		expected string
	}{
		{
			name:     "adds missing rollout strategy",
			raw:      `{"spec":{}}`,
			expected: `[{"op":"add","path":"/spec/rolloutStrategy","value":{"type":"RollingUpdate"}}]`,
		},
		{
			name:     "adds missing rollout strategy type",
			raw:      `{"spec":{"rolloutStrategy":{}}}`,
			expected: `[{"op":"add","path":"/spec/rolloutStrategy/type","value":"RollingUpdate"}]`,
		},
		{
			name:     "replaces empty rollout strategy type",
			raw:      `{"spec":{"rolloutStrategy":{"type":""}}}`,
			expected: `[{"op":"replace","path":"/spec/rolloutStrategy/type","value":"RollingUpdate"}]`,
		},
		{
			name:     "preserves explicit rollout strategy type",
			raw:      `{"spec":{"rolloutStrategy":{"type":"RollingUpdate"}}}`,
			expected: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lws lwsv1.LeaderWorkerSet
			require.NoError(t, json.Unmarshal([]byte(tt.raw), &lws))
			patch, err := createLWSPatch([]byte(tt.raw), &lws)
			require.NoError(t, err)
			assert.JSONEq(t, tt.expected, string(patch))
		})
	}
}

func TestLWSMutatorDefaultsRolloutStrategy(t *testing.T) {
	raw := []byte(`{"apiVersion":"leaderworkerset.x-k8s.io/v1","kind":"LeaderWorkerSet","metadata":{"name":"sample"},"spec":{"rolloutStrategy":{"type":""}}}`)
	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID("test-uid"),
			Object: runtime.RawExtension{Raw: raw},
		},
	}
	body, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/mutate/leaderworkerset", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	NewLWSMutator().Handle(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotNil(t, response.Response)
	assert.True(t, response.Response.Allowed)
	assert.Equal(t, types.UID("test-uid"), response.Response.UID)
	assert.JSONEq(t, `[{"op":"replace","path":"/spec/rolloutStrategy/type","value":"RollingUpdate"}]`, string(response.Response.Patch))
}

func TestLWSMutatorPreservesExplicitRolloutStrategy(t *testing.T) {
	raw := []byte(`{"apiVersion":"leaderworkerset.x-k8s.io/v1","kind":"LeaderWorkerSet","metadata":{"name":"sample"},"spec":{"rolloutStrategy":{"type":"RollingUpdate"}}}`)
	requestReview := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}},
	}
	body, err := json.Marshal(requestReview)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/mutate/leaderworkerset", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	NewLWSMutator().Handle(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.JSONEq(t, `[]`, string(response.Response.Patch))
}
