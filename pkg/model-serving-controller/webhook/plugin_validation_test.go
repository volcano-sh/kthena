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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func pluginImmutableFixture() *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		TypeMeta:   metav1.TypeMeta{APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: "ModelServing"},
		ObjectMeta: metav1.ObjectMeta{Name: "immutable-plugins"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1), SchedulerName: "volcano",
			Plugins: []workloadv1alpha1.PluginSpec{
				{Name: "demo-pod-tweaks", Type: workloadv1alpha1.PluginTypeBuiltIn,
					Config: &apiextensionsv1.JSON{Raw: []byte(`{"annotations":{"plugin-value":"old","other":"value"}}`)}},
				{Name: "lws-standard-labels", Type: workloadv1alpha1.PluginTypeBuiltIn},
			},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name: "predictor", Replicas: ptr.To[int32](1),
				EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:1.36"}},
				}},
			}}},
		},
	}
}

// Exercise the real AdmissionReview entry point, including UPDATE oldObject
// dispatch. All changes must obey the same rule in both rollout modes.
func TestModelServingValidatorPluginsImmutable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(old, next *workloadv1alpha1.ModelServing)
		allowed bool
	}{
		{name: "unchanged", allowed: true},
		{name: "add first plugin", mutate: func(old, _ *workloadv1alpha1.ModelServing) { old.Spec.Plugins = nil }},
		{name: "add plugin", mutate: func(old, _ *workloadv1alpha1.ModelServing) { old.Spec.Plugins = old.Spec.Plugins[:1] }},
		{name: "delete one", mutate: func(_, next *workloadv1alpha1.ModelServing) { next.Spec.Plugins = next.Spec.Plugins[:1] }},
		{name: "delete all empty", mutate: func(_, next *workloadv1alpha1.ModelServing) { next.Spec.Plugins = []workloadv1alpha1.PluginSpec{} }},
		{name: "delete all omitted", mutate: func(_, next *workloadv1alpha1.ModelServing) { next.Spec.Plugins = nil }},
		{name: "rename", mutate: func(_, next *workloadv1alpha1.ModelServing) { next.Spec.Plugins[0].Name = "other-plugin" }},
		{name: "change type", mutate: func(_, next *workloadv1alpha1.ModelServing) { next.Spec.Plugins[0].Type = "Webhook" }},
		{name: "change config", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0].Config.Raw = []byte(`{"annotations":{"plugin-value":"new","other":"value"}}`)
		}},
		{name: "remove config", mutate: func(_, next *workloadv1alpha1.ModelServing) { next.Spec.Plugins[0].Config = nil }},
		{name: "add config", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[1].Config = &apiextensionsv1.JSON{Raw: []byte(`{}`)}
		}},
		{name: "scope target", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetEntry}
		}},
		{name: "scope roles even if currently all roles", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"predictor"}}
		}},
		{name: "remove scope", mutate: func(old, _ *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"predictor"}}
		}},
		{name: "plugin order", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0], next.Spec.Plugins[1] = next.Spec.Plugins[1], next.Spec.Plugins[0]
		}},
		{name: "config array order", mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins[0].Config.Raw = []byte(`{"values":[1,2]}`)
			next.Spec.Plugins[0].Config.Raw = []byte(`{"values":[2,1]}`)
		}},
		{name: "large integer difference", mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins[0].Config.Raw = []byte(`{"value":9007199254740992}`)
			next.Spec.Plugins[0].Config.Raw = []byte(`{"value":9007199254740993}`)
		}},
		{name: "numeric spelling is not equivalent", mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins[0].Config.Raw = []byte(`{"value":1}`)
			next.Spec.Plugins[0].Config.Raw = []byte(`{"value":1.0}`)
		}},
		{name: "delete at zero replicas", mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Replicas, next.Spec.Replicas = ptr.To[int32](0), ptr.To[int32](0)
			next.Spec.Plugins = nil
		}},
		{name: "delete together with scale", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Replicas, next.Spec.Plugins = ptr.To[int32](2), nil
		}},
		{name: "delete together with image", mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "busybox:1.37"
			next.Spec.Plugins = nil
		}},
		{name: "parent controller cannot add plugin", mutate: func(old, next *workloadv1alpha1.ModelServing) {
			owner := metav1.OwnerReference{APIVersion: "leaderworkerset.x-k8s.io/v1", Kind: "LeaderWorkerSet", Name: "parent", UID: "parent-uid", Controller: ptr.To(true)}
			old.OwnerReferences, next.OwnerReferences = []metav1.OwnerReference{owner}, []metav1.OwnerReference{owner}
			old.Spec.Plugins = nil
		}},
		{name: "metadata and scale", allowed: true, mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Labels = map[string]string{"owner": "test"}
			next.Spec.Replicas = ptr.To[int32](2)
			next.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
		}},
		{name: "template image", allowed: true, mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "busybox:1.37"
		}},
		{name: "JSON object keys and whitespace", allowed: true, mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0].Config.Raw = []byte(`{ "annotations": { "other": "value", "plugin-value": "old" } }`)
		}},
		{name: "empty and omitted plugins", allowed: true, mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins, next.Spec.Plugins = nil, []workloadv1alpha1.PluginSpec{}
		}},
		{name: "default type", allowed: true, mutate: func(old, _ *workloadv1alpha1.ModelServing) { old.Spec.Plugins[0].Type = "" }},
		{name: "default scope", allowed: true, mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetAll}
		}},
		{name: "empty scope", allowed: true, mutate: func(_, next *workloadv1alpha1.ModelServing) {
			next.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{}
		}},
		{name: "roles set order and duplicates", allowed: true, mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"b", "a", "b"}}
			next.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"a", "b"}}
		}},
		{name: "null and omitted config", allowed: true, mutate: func(old, next *workloadv1alpha1.ModelServing) {
			old.Spec.Plugins[0].Config = &apiextensionsv1.JSON{Raw: []byte(`null`)}
			next.Spec.Plugins[0].Config = nil
		}},
	}
	for _, mode := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, tt := range tests {
			t.Run(string(mode)+"/"+tt.name, func(t *testing.T) {
				old := pluginImmutableFixture()
				old.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: mode}
				next := old.DeepCopy()
				if tt.mutate != nil {
					tt.mutate(old, next)
				}
				response := admitPlugins(t, admissionv1.Update, old, next)
				require.Equal(t, tt.allowed, response.Allowed, "%+v", response.Result)
				if !tt.allowed {
					require.NotNil(t, response.Result)
					require.Contains(t, response.Result.Message, "spec.plugins: Forbidden: field is immutable after creation")
					require.Contains(t, response.Result.Message, "create a new ModelServing")
				}
			})
		}
	}
}

func TestModelServingValidatorPluginsCreate(t *testing.T) {
	for _, configured := range []bool{false, true} {
		ms := pluginImmutableFixture()
		if !configured {
			ms.Spec.Plugins = nil
		}
		response := admitPlugins(t, admissionv1.Create, nil, ms)
		require.True(t, response.Allowed, "%+v", response.Result)
	}
}

func TestValidatePluginsImmutableDoesNotMutateInput(t *testing.T) {
	old := pluginImmutableFixture()
	old.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"b", "a", "b"}}
	next := old.DeepCopy()
	oldBefore, nextBefore := old.DeepCopy(), next.DeepCopy()
	require.Empty(t, validatePluginsImmutable(old, next))
	require.Equal(t, oldBefore, old)
	require.Equal(t, nextBefore, next)
}

func TestModelServingValidatorPluginsRejectMalformedOldObject(t *testing.T) {
	for _, oldRaw := range []string{"", "null", "{}", "[]", `"not an object"`, `{"metadata":{"name":"old"},"spec":{"plugins":true}}`} {
		t.Run(oldRaw, func(t *testing.T) {
			// Keep the surrounding AdmissionReview valid to exercise UPDATE denial,
			// rather than the generic invalid-request HTTP response.
			response := admitPluginsRaw(t, admissionv1.Update, []byte(oldRaw), pluginImmutableFixture())
			require.False(t, response.Allowed)
			require.NotNil(t, response.Result)
			require.Contains(t, response.Result.Message, "old ModelServing")
		})
	}
	t.Run("invalid JSON", func(t *testing.T) {
		review := &admissionv1.AdmissionReview{Request: &admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			OldObject: runtime.RawExtension{Raw: []byte(`{"spec":`)},
		}}
		old, err := parseOldModelServingForUpdate(review)
		require.ErrorContains(t, err, "failed to decode old ModelServing")
		require.Nil(t, old)
	})
}

func TestValidatePluginsImmutableRejectsUncomparableConfig(t *testing.T) {
	for _, malformedOld := range []bool{false, true} {
		old := pluginImmutableFixture()
		next := old.DeepCopy()
		if malformedOld {
			old.Spec.Plugins[0].Config.Raw = []byte(`{"value":`)
		} else {
			next.Spec.Plugins[0].Config.Raw = []byte(`{"value":`)
		}
		errs := validatePluginsImmutable(old, next)
		require.Len(t, errs, 1)
		require.Contains(t, errs[0].Error(), "cannot compare")
	}
}

func admitPlugins(t *testing.T, operation admissionv1.Operation, old, next *workloadv1alpha1.ModelServing) *admissionv1.AdmissionResponse {
	t.Helper()
	var oldRaw []byte
	if old != nil {
		var err error
		oldRaw, err = json.Marshal(old)
		require.NoError(t, err)
	}
	return admitPluginsRaw(t, operation, oldRaw, next)
}

func admitPluginsRaw(t *testing.T, operation admissionv1.Operation, oldRaw []byte, next *workloadv1alpha1.ModelServing) *admissionv1.AdmissionResponse {
	t.Helper()
	newRaw, err := json.Marshal(next)
	require.NoError(t, err)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request:  &admissionv1.AdmissionRequest{UID: "plugins-immutable", Operation: operation, Object: runtime.RawExtension{Raw: newRaw}},
	}
	if len(oldRaw) > 0 {
		review.Request.OldObject = runtime.RawExtension{Raw: oldRaw}
	}
	body, err := json.Marshal(review)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/validate-workload-ai-v1alpha1-modelserving", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	NewModelServingValidator().Handle(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var result admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	require.NotNil(t, result.Response)
	require.Equal(t, review.Request.UID, result.Response.UID)
	return result.Response
}
