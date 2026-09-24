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

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestSchedulerNameImmutableAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, old, current string
		operation          admissionv1.Operation
		allow              bool
	}{
		{"create with custom scheduler", "", "custom", admissionv1.Create, true},
		{"unchanged volcano", "volcano", "volcano", admissionv1.Update, true},
		{"unchanged custom", "custom", "custom", admissionv1.Update, true},
		{"explicit empty to volcano", "", "volcano", admissionv1.Update, false},
		{"volcano to explicit empty", "volcano", "", admissionv1.Update, false},
		{"effective Pod default materialized", "", "default-scheduler", admissionv1.Update, true},
		{"effective Pod default omitted", "default-scheduler", "", admissionv1.Update, true},
		{"volcano to other", "volcano", "custom", admissionv1.Update, false},
		{"other to volcano", "custom", "volcano", admissionv1.Update, false},
		{"custom to custom", "custom-a", "custom-b", admissionv1.Update, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "scheduler-test"},
				Spec: workloadv1alpha1.ModelServingSpec{
					SchedulerName: tc.old, Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
						Name: "decode", Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "image"}},
						}},
					}}},
				},
			}
			current := old.DeepCopy()
			current.Spec.SchedulerName = tc.current
			oldRaw, err := json.Marshal(old)
			require.NoError(t, err)
			currentRaw, err := json.Marshal(current)
			require.NoError(t, err)
			review := admissionv1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
				Request: &admissionv1.AdmissionRequest{
					UID: "scheduler-update", Operation: tc.operation,
					Object: runtime.RawExtension{Raw: currentRaw}, OldObject: runtime.RawExtension{Raw: oldRaw},
				},
			}
			body, err := json.Marshal(review)
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewModelServingValidator().Handle(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			var result admissionv1.AdmissionReview
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.NotNil(t, result.Response)
			require.Equal(t, tc.allow, result.Response.Allowed, "%+v", result.Response.Result)
			require.Equal(t, review.Request.UID, result.Response.UID)
			if !tc.allow {
				require.Contains(t, result.Response.Result.Message, "spec.schedulerName")
				require.Contains(t, result.Response.Result.Message, "immutable")
			}
		})
	}
}
