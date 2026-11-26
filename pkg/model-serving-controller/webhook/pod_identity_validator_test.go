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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func baseManagedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms-0-prefill-0-0",
			Namespace: "default",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "ms",
				workloadv1alpha1.GroupNameLabelKey:        "ms-0",
				workloadv1alpha1.RoleLabelKey:             "prefill",
				workloadv1alpha1.RoleIDKey:                "prefill-0",
				workloadv1alpha1.EntryLabelKey:            "true",
				workloadv1alpha1.GroupInstanceIDLabelKey:  "gid-1",
				workloadv1alpha1.RoleInstanceIDLabelKey:   "rid-1",
				workloadv1alpha1.RevisionLabelKey:         "rev-1",
				workloadv1alpha1.RoleTemplateHashLabelKey: "hash-1",
				"modelserving.volcano.sh/rolename":        "P-instance",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
				Kind:       workloadv1alpha1.ModelServingKind.Kind,
				Name:       "ms",
				UID:        "ms-uid",
				Controller: ptr.To(true),
			}},
		},
	}
}

func sendPodAdmissionUpdate(t *testing.T, validator *PodIdentityValidator, oldPod, newPod *corev1.Pod) *admissionv1.AdmissionResponse {
	t.Helper()
	var oldRaw []byte
	if oldPod != nil {
		var err error
		oldRaw, err = json.Marshal(oldPod)
		require.NoError(t, err)
	}
	newRaw, err := json.Marshal(newPod)
	require.NoError(t, err)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "req-uid",
			Operation: admissionv1.Update,
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	}
	body, err := json.Marshal(review)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/validate-pod-identity", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	validator.Handle(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var out admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.NotNil(t, out.Response)
	return out.Response
}

func TestPodIdentityValidatorRejectsImmutableLabelMutations(t *testing.T) {
	validator := NewPodIdentityValidator()

	for _, key := range immutablePodIdentityLabels {
		t.Run("modify "+key, func(t *testing.T) {
			oldPod := baseManagedPod()
			newPod := oldPod.DeepCopy()
			newPod.Labels[key] = "tampered"
			resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
			assert.False(t, resp.Allowed)
			require.NotNil(t, resp.Result)
			assert.Contains(t, resp.Result.Message, key)
		})
		t.Run("delete "+key, func(t *testing.T) {
			oldPod := baseManagedPod()
			newPod := oldPod.DeepCopy()
			delete(newPod.Labels, key)
			resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
			assert.False(t, resp.Allowed)
		})
	}

	t.Run("missing oldObject is rejected", func(t *testing.T) {
		resp := sendPodAdmissionUpdate(t, validator, nil, baseManagedPod())
		assert.False(t, resp.Allowed)
	})

	t.Run("in-place revision and user label updates are allowed", func(t *testing.T) {
		oldPod := baseManagedPod()
		newPod := oldPod.DeepCopy()
		newPod.Labels[workloadv1alpha1.RevisionLabelKey] = "rev-2"
		newPod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-2"
		newPod.Labels["modelserving.volcano.sh/rolename"] = "disabled"
		resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
		assert.True(t, resp.Allowed)
	})

	t.Run("unmanaged Pod label updates are allowed", func(t *testing.T) {
		oldPod := baseManagedPod()
		oldPod.OwnerReferences = nil
		newPod := oldPod.DeepCopy()
		newPod.Labels[workloadv1alpha1.RoleIDKey] = "other-0"
		resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
		assert.True(t, resp.Allowed)
	})

	t.Run("removing controlling ModelServing ownerReference is rejected", func(t *testing.T) {
		oldPod := baseManagedPod()
		newPod := oldPod.DeepCopy()
		newPod.OwnerReferences = nil
		resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
		assert.False(t, resp.Allowed)
		require.NotNil(t, resp.Result)
		assert.Contains(t, resp.Result.Message, "ownerReferences")
	})

	t.Run("replacing controlling ModelServing ownerReference UID is rejected", func(t *testing.T) {
		oldPod := baseManagedPod()
		newPod := oldPod.DeepCopy()
		newPod.OwnerReferences[0].UID = "other-ms-uid"
		resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
		assert.False(t, resp.Allowed)
	})

	t.Run("attaching ModelServing controller ownerReference to unmanaged Pod is rejected", func(t *testing.T) {
		oldPod := baseManagedPod()
		oldPod.OwnerReferences = nil
		newPod := baseManagedPod()
		resp := sendPodAdmissionUpdate(t, validator, oldPod, newPod)
		assert.False(t, resp.Allowed)
	})
}
