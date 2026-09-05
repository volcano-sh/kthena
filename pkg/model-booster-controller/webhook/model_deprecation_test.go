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
	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestModelBoosterDeprecationWarnings(t *testing.T) {
	tests := []struct {
		name            string
		operation       admissionv1.Operation
		subresource     string
		includeOldModel bool
		mutate          func(*registryv1alpha1.ModelBooster)
		wantWarning     bool
	}{
		{
			name:        "create",
			operation:   admissionv1.Create,
			wantWarning: true,
		},
		{
			name:            "spec update",
			operation:       admissionv1.Update,
			includeOldModel: true,
			mutate: func(model *registryv1alpha1.ModelBooster) {
				model.Spec.Backend.SchedulerName = "volcano"
			},
			wantWarning: true,
		},
		{
			name:            "status-only update",
			operation:       admissionv1.Update,
			includeOldModel: true,
			mutate: func(model *registryv1alpha1.ModelBooster) {
				model.Status.ObservedGeneration++
			},
		},
		{
			name:            "metadata-only update",
			operation:       admissionv1.Update,
			includeOldModel: true,
			mutate: func(model *registryv1alpha1.ModelBooster) {
				model.Labels = map[string]string{"example": "value"}
			},
		},
		{
			name:            "status subresource update",
			operation:       admissionv1.Update,
			subresource:     "status",
			includeOldModel: true,
			mutate: func(model *registryv1alpha1.ModelBooster) {
				model.Status.ObservedGeneration++
			},
		},
		{
			name:        "update without old object",
			operation:   admissionv1.Update,
			wantWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldModel := validModelBoosterForDeprecationTest()
			newModel := oldModel.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(newModel)
			}

			var admissionOldModel *registryv1alpha1.ModelBooster
			if tt.includeOldModel {
				admissionOldModel = oldModel
			}
			response := handleModelBoosterAdmission(t, tt.operation, tt.subresource, newModel, admissionOldModel)
			require.True(t, response.Allowed)
			if tt.wantWarning {
				assert.Equal(t, []string{modelBoosterDeprecationWarning}, response.Warnings)
			} else {
				assert.Empty(t, response.Warnings)
			}
		})
	}
}

func validModelBoosterForDeprecationTest() *registryv1alpha1.ModelBooster {
	return &registryv1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: registryv1alpha1.ModelBoosterSpec{
			Backend: registryv1alpha1.ModelBackend{
				Name:     "backend",
				Type:     registryv1alpha1.ModelBackendTypeVLLM,
				ModelURI: "hf://test/model",
				Replicas: 1,
				Workers: []registryv1alpha1.ModelWorker{
					{
						Type:  registryv1alpha1.ModelWorkerTypeServer,
						Pods:  1,
						Image: "test-image:latest",
					},
				},
			},
		},
	}
}

func handleModelBoosterAdmission(
	t *testing.T,
	operation admissionv1.Operation,
	subresource string,
	model *registryv1alpha1.ModelBooster,
	oldModel *registryv1alpha1.ModelBooster,
) *admissionv1.AdmissionResponse {
	t.Helper()

	request := &admissionv1.AdmissionRequest{
		UID:         types.UID("test-request"),
		Operation:   operation,
		SubResource: subresource,
		Object: runtime.RawExtension{
			Raw: mustMarshalAdmissionObject(t, model),
		},
	}
	if oldModel != nil {
		request.OldObject.Raw = mustMarshalAdmissionObject(t, oldModel)
	}

	requestBody := mustMarshalAdmissionObject(t, admissionv1.AdmissionReview{Request: request})
	httpRequest := httptest.NewRequest(http.MethodPost, "/validate/modelbooster", bytes.NewReader(requestBody))
	httpRequest.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	NewModelValidator().Handle(recorder, httpRequest)
	require.Equal(t, http.StatusOK, recorder.Code)

	var responseReview admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &responseReview))
	require.NotNil(t, responseReview.Response)
	return responseReview.Response
}

func mustMarshalAdmissionObject(t *testing.T, object any) []byte {
	t.Helper()
	data, err := json.Marshal(object)
	require.NoError(t, err)
	return data
}
