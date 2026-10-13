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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// newUpdateMethodModelServing returns a ModelServing whose previous rollout is complete.
func newUpdateMethodModelServing(updateMethod workloadv1alpha1.UpdateMethod) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", Generation: 3},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](2),
			SchedulerName: "volcano",
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:         workloadv1alpha1.ServingGroupRollingUpdate,
				UpdateMethod: updateMethod,
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{{
					Name:     "decode",
					Replicas: ptr.To[int32](1),
					EntryTemplate: workloadv1alpha1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "engine",
								Image: "example.com/engine:v1",
								Env:   []corev1.EnvVar{{Name: "MODEL", Value: "m1"}},
							}},
						},
					},
				}},
			},
		},
		Status: workloadv1alpha1.ModelServingStatus{
			ObservedGeneration: 3,
			Replicas:           2,
			UpdatedReplicas:    2,
			CurrentRevision:    "rev-1",
			UpdateRevision:     "rev-1",
		},
	}
}

func TestValidateUpdateMethod(t *testing.T) {
	strategyPath := field.NewPath("spec", "rolloutStrategy")
	configPath := strategyPath.Child("rollingUpdateConfiguration")
	entryPath := field.NewPath("spec", "template", "roles").Index(0).Child("entryTemplate")
	notSupported := field.NotSupported(strategyPath.Child("updateMethod"), "", []string(nil))

	tests := []struct {
		name         string
		updateMethod workloadv1alpha1.UpdateMethod
		mutate       func(ms *workloadv1alpha1.ModelServing)
		want         field.ErrorList
	}{
		{
			name: "no rollout strategy",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy = nil
			},
		},
		{
			name: "empty updateMethod",
		},
		{
			name:         "progressDeadlineSeconds without InPlace",
			updateMethod: workloadv1alpha1.RecreateUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](600)
			},
			want: field.ErrorList{field.Forbidden(strategyPath.Child("progressDeadlineSeconds"), "")},
		},
		{
			name:         "InPlace with progressDeadlineSeconds is not supported yet",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.ProgressDeadlineSeconds = ptr.To[int32](600)
			},
			want: field.ErrorList{notSupported},
		},
		{
			name:         "InPlace with RoleRollingUpdate",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
			},
			want: field.ErrorList{notSupported, field.Forbidden(strategyPath.Child("type"), "")},
		},
		{
			name:         "InPlace with maxSurge",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt32(0)),
					MaxSurge:       ptr.To(intstr.FromInt32(1)),
				}
			},
			want: field.ErrorList{
				notSupported,
				field.Invalid(configPath.Child("maxSurge"), nil, ""),
				field.Invalid(configPath.Child("maxUnavailable"), nil, ""),
			},
		},
		{
			name:         "InPlace with maxUnavailable rounding down to 0",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](3)
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromString("25%")),
				}
			},
			want: field.ErrorList{notSupported, field.Invalid(configPath.Child("maxUnavailable"), nil, "")},
		},
		{
			name:         "InPlace with zero replicas and a percentage budget",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](0)
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromString("25%")),
				}
			},
			want: field.ErrorList{notSupported},
		},
		{
			name:         "InPlace with plugins",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "demo", Type: workloadv1alpha1.PluginTypeBuiltIn}}
			},
			want: field.ErrorList{notSupported, field.Forbidden(field.NewPath("spec", "plugins"), "")},
		},
		{
			name:         "InPlace with restart settings that skip restarts",
			updateMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				spec := &ms.Spec.Template.Roles[0].EntryTemplate.Spec
				spec.RestartPolicy = corev1.RestartPolicyOnFailure
				spec.Containers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
			},
			want: field.ErrorList{
				notSupported,
				field.NotSupported(entryPath.Child("spec", "restartPolicy"), "", []string(nil)),
				field.Forbidden(entryPath.Child("spec", "containers").Index(0), ""),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := newUpdateMethodModelServing(tt.updateMethod)
			if tt.mutate != nil {
				tt.mutate(ms)
			}
			assert.Equal(t, errorKeys(tt.want), errorKeys(validateUpdateMethod(ms)))
		})
	}
}

func TestValidateInPlaceUpdate(t *testing.T) {
	updateMethodPath := field.NewPath("spec", "rolloutStrategy", "updateMethod")
	entryPath := field.NewPath("spec", "template", "roles").Index(0).Child("entryTemplate")
	changeImage := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v2"
	}
	changeEnv := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env[0].Value = "m2"
	}

	tests := []struct {
		name      string
		oldMethod workloadv1alpha1.UpdateMethod
		newMethod workloadv1alpha1.UpdateMethod
		mutateOld func(ms *workloadv1alpha1.ModelServing)
		mutateNew func(ms *workloadv1alpha1.ModelServing)
		want      field.ErrorList
	}{
		{
			name:      "Recreate allows any template change",
			oldMethod: workloadv1alpha1.InPlaceUpdateMethod,
			newMethod: workloadv1alpha1.RecreateUpdateMethod,
			mutateNew: changeEnv,
		},
		{
			name:      "InPlace image change",
			oldMethod: workloadv1alpha1.InPlaceUpdateMethod,
			newMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutateOld: func(ms *workloadv1alpha1.ModelServing) {
				ms.Status.UpdatedReplicas = 1
			},
			mutateNew: changeImage,
		},
		{
			name:      "InPlace non-image change",
			oldMethod: workloadv1alpha1.InPlaceUpdateMethod,
			newMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutateNew: changeEnv,
			want:      field.ErrorList{field.Forbidden(entryPath, "")},
		},
		{
			name:      "switch to InPlace after a complete rollout with an image change",
			newMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutateNew: changeImage,
		},
		{
			name:      "switch to InPlace during a rollout",
			newMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutateOld: func(ms *workloadv1alpha1.ModelServing) {
				ms.Status.UpdatedReplicas = 1
			},
			want: field.ErrorList{field.Forbidden(updateMethodPath, "")},
		},
		{
			name:      "switch to InPlace with revisions behind",
			newMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutateOld: func(ms *workloadv1alpha1.ModelServing) {
				ms.Status.CurrentRevision = "rev-0"
			},
			want: field.ErrorList{field.Forbidden(updateMethodPath, "")},
		},
		{
			name:      "switch to InPlace before the last spec change is observed",
			oldMethod: workloadv1alpha1.RecreateUpdateMethod,
			newMethod: workloadv1alpha1.InPlaceUpdateMethod,
			mutateOld: func(ms *workloadv1alpha1.ModelServing) {
				ms.Status.ObservedGeneration = 2
			},
			mutateNew: changeEnv,
			want:      field.ErrorList{field.Forbidden(updateMethodPath, ""), field.Forbidden(entryPath, "")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldMS := newUpdateMethodModelServing(tt.oldMethod)
			if tt.mutateOld != nil {
				tt.mutateOld(oldMS)
			}
			newMS := oldMS.DeepCopy()
			newMS.Spec.RolloutStrategy.UpdateMethod = tt.newMethod
			if tt.mutateNew != nil {
				tt.mutateNew(newMS)
			}
			assert.Equal(t, errorKeys(tt.want), errorKeys(validateInPlaceUpdate(oldMS, newMS)))
		})
	}
}

func TestModelServingValidatorHandleDecodesOldObject(t *testing.T) {
	oldMS := newUpdateMethodModelServing(workloadv1alpha1.RecreateUpdateMethod)
	oldMS.Status.UpdatedReplicas = 1
	newMS := oldMS.DeepCopy()
	newMS.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
	oldRaw, err := json.Marshal(oldMS)
	require.NoError(t, err)
	newRaw, err := json.Marshal(newMS)
	require.NoError(t, err)

	tests := []struct {
		name        string
		oldRaw      []byte
		wantCode    int
		wantMessage string
	}{
		{
			name:        "update is checked against the old object",
			oldRaw:      oldRaw,
			wantCode:    http.StatusOK,
			wantMessage: "switching to InPlace requires the previous rollout to be complete",
		},
		{
			name:     "malformed old object",
			oldRaw:   []byte(`{"spec":{"replicas":"two"}}`),
			wantCode: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review := admissionv1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
				Request: &admissionv1.AdmissionRequest{
					UID:       "uid",
					Operation: admissionv1.Update,
					Object:    runtime.RawExtension{Raw: newRaw},
					OldObject: runtime.RawExtension{Raw: tt.oldRaw},
				},
			}
			body, err := json.Marshal(review)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/validate/modelserving", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			NewModelServingValidator().Handle(rec, req)

			require.Equal(t, tt.wantCode, rec.Code)
			if tt.wantCode != http.StatusOK {
				return
			}
			var response admissionv1.AdmissionReview
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			assert.False(t, response.Response.Allowed)
			assert.Contains(t, response.Response.Result.Message, tt.wantMessage)
		})
	}
}

// errorKeys drops error details, so tests assert only the error type and field.
func errorKeys(errs field.ErrorList) []string {
	var keys []string
	for _, err := range errs {
		keys = append(keys, string(err.Type)+" "+err.Field)
	}
	return keys
}
