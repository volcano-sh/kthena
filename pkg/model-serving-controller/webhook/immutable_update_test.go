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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func immutableAdmissionModelServing(strategy workloadv1alpha1.RolloutStrategyType) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "immutable-test"},
		Spec: workloadv1alpha1.ModelServingSpec{
			SchedulerName: "volcano", Replicas: ptr.To[int32](1),
			RecoveryPolicy:  workloadv1alpha1.NoneRestartPolicy,
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{Type: strategy},
			Plugins:         []workloadv1alpha1.PluginSpec{{Name: "demo-pod-tweaks"}, {Name: "lws"}},
		},
	}
	for _, name := range []string{"prefill", "decode"} {
		ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
			Name: name, Replicas: ptr.To[int32](1),
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main", Image: "image:v1"}},
			}},
		})
	}
	return ms
}

func immutableAdmissionResponse(t *testing.T, oldRaw []byte, current *workloadv1alpha1.ModelServing, operation admissionv1.Operation) *admissionv1.AdmissionResponse {
	t.Helper()
	request := map[string]interface{}{
		"uid": "immutable-update", "operation": operation, "object": current,
	}
	if oldRaw != nil {
		request["oldObject"] = json.RawMessage(oldRaw)
	}
	body, err := json.Marshal(map[string]interface{}{
		"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview", "request": request,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewModelServingValidator().Handle(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.NotNil(t, result.Response)
	require.EqualValues(t, "immutable-update", result.Response.UID)
	require.Empty(t, result.Response.Patch)
	return result.Response
}

func TestImmutableModelServingUpdateAdmission(t *testing.T) {
	type modelServing = workloadv1alpha1.ModelServing
	config := func(raw string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(raw)} }
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{
		workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate,
	} {
		t.Run(string(strategy), func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				prepare func(*modelServing)
				change  func(*modelServing)
				field   string
			}{
				{name: "unchanged"},
				{name: "metadata image and replicas remain mutable", change: func(ms *modelServing) {
					ms.Labels = map[string]string{"example.com/label": "value"}
					ms.Spec.Replicas = ptr.To[int32](2)
					ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
					ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
				}},
				{name: "Role reorder", change: func(ms *modelServing) {
					ms.Spec.Template.Roles[0], ms.Spec.Template.Roles[1] = ms.Spec.Template.Roles[1], ms.Spec.Template.Roles[0]
				}},
				{name: "existing Role templates may be exchanged", prepare: func(ms *modelServing) {
					ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v1"
					ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "decode:v1"
				}, change: func(ms *modelServing) {
					ms.Spec.Template.Roles[0].EntryTemplate, ms.Spec.Template.Roles[1].EntryTemplate = ms.Spec.Template.Roles[1].EntryTemplate, ms.Spec.Template.Roles[0].EntryTemplate
				}},
				{name: "nil plugins to empty", prepare: func(ms *modelServing) { ms.Spec.Plugins = nil }, change: func(ms *modelServing) { ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{} }},
				{name: "empty plugins to nil", prepare: func(ms *modelServing) { ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{} }, change: func(ms *modelServing) { ms.Spec.Plugins = nil }},
				{name: "default plugin type", change: func(ms *modelServing) { ms.Spec.Plugins[0].Type = workloadv1alpha1.PluginTypeBuiltIn }},
				{name: "empty scope", change: func(ms *modelServing) { ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{} }},
				{name: "explicit All scope", change: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetAll, Roles: []string{}}
				}},
				{name: "scope roles form a set", prepare: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"prefill", "decode"}}
				}, change: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope.Roles = []string{"decode", "prefill", "decode"}
				}},
				{name: "empty roles still means all for Entry target", prepare: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetEntry}
				}, change: func(ms *modelServing) { ms.Spec.Plugins[0].Scope.Roles = []string{} }},
				{name: "JSON object key order and whitespace", prepare: func(ms *modelServing) {
					ms.Spec.Plugins[0].Config = config(`{"a":1,"b":{"x":2,"y":3}}`)
				}, change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{ "b": {"y":3,"x":2}, "a":1 }`) }},
				{name: "omitted config to null", change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`null`) }},
				{name: "null config to omitted", prepare: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`null`) }, change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = nil }},
				{name: "add first plugin", prepare: func(ms *modelServing) { ms.Spec.Plugins = nil }, change: func(ms *modelServing) {
					ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "demo-pod-tweaks"}}
				}, field: "spec.plugins"},
				{name: "add plugin", change: func(ms *modelServing) {
					ms.Spec.Plugins = append(ms.Spec.Plugins, workloadv1alpha1.PluginSpec{Name: "third"})
				}, field: "spec.plugins"},
				{name: "delete one plugin", change: func(ms *modelServing) { ms.Spec.Plugins = ms.Spec.Plugins[:1] }, field: "spec.plugins"},
				{name: "delete all plugins", change: func(ms *modelServing) { ms.Spec.Plugins = nil }, field: "spec.plugins"},
				{name: "plugin name", change: func(ms *modelServing) { ms.Spec.Plugins[0].Name = "other" }, field: "spec.plugins"},
				{name: "plugin type", change: func(ms *modelServing) { ms.Spec.Plugins[0].Type = "Other" }, field: "spec.plugins"},
				{name: "plugin order", change: func(ms *modelServing) {
					ms.Spec.Plugins[0], ms.Spec.Plugins[1] = ms.Spec.Plugins[1], ms.Spec.Plugins[0]
				}, field: "spec.plugins"},
				{name: "plugin scope roles", change: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}}
				}, field: "spec.plugins"},
				{name: "plugin scope target", change: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetEntry}
				}, field: "spec.plugins"},
				{name: "inactive worker plugin still immutable", prepare: func(ms *modelServing) {
					ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetWorker}
				}, change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"enabled":true}`) }, field: "spec.plugins"},
				{name: "empty object is not omitted config", change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{}`) }, field: "spec.plugins"},
				{name: "JSON array order", prepare: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"list":[1,2]}`) }, change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"list":[2,1]}`) }, field: "spec.plugins"},
				{name: "large integers remain distinct", prepare: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"n":9007199254740992}`) }, change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"n":9007199254740993}`) }, field: "spec.plugins"},
				{name: "numeric representations are not automatically equivalent", prepare: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"n":1}`) }, change: func(ms *modelServing) { ms.Spec.Plugins[0].Config = config(`{"n":1.0}`) }, field: "spec.plugins"},
				{name: "plugin and image replicas change rejected together", change: func(ms *modelServing) {
					ms.Spec.Plugins = nil
					ms.Spec.Replicas = ptr.To[int32](2)
					ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
				}, field: "spec.plugins"},
				{name: "zero replicas do not allow plugin removal", prepare: func(ms *modelServing) {
					ms.Spec.Replicas = ptr.To[int32](0)
					for i := range ms.Spec.Template.Roles {
						ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](0)
					}
				}, change: func(ms *modelServing) { ms.Spec.Plugins = nil }, field: "spec.plugins"},
				{name: "rename Role", change: func(ms *modelServing) { ms.Spec.Template.Roles[0].Name = "renamed" }, field: "spec.template.roles"},
				{name: "add Role", change: func(ms *modelServing) {
					role := *ms.Spec.Template.Roles[0].DeepCopy()
					role.Name = "extra"
					ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, role)
				}, field: "spec.template.roles"},
				{name: "remove Role", change: func(ms *modelServing) { ms.Spec.Template.Roles = ms.Spec.Template.Roles[:1] }, field: "spec.template.roles"},
				{name: "zero replica Role identity immutable", prepare: func(ms *modelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0) }, change: func(ms *modelServing) { ms.Spec.Template.Roles[0].Name = "renamed" }, field: "spec.template.roles"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					old := immutableAdmissionModelServing(strategy)
					if tc.prepare != nil {
						tc.prepare(old)
					}
					current := old.DeepCopy()
					if tc.change != nil {
						tc.change(current)
					}
					oldRaw, err := json.Marshal(old)
					require.NoError(t, err)
					result := immutableAdmissionResponse(t, oldRaw, current, admissionv1.Update)
					require.Equal(t, tc.field == "", result.Allowed, "%+v", result.Result)
					if tc.field != "" {
						require.Contains(t, result.Result.Message, tc.field)
						require.Contains(t, result.Result.Message, "immutable")
					}
				})
			}
		})
	}
}

func TestImmutableAdmissionOldObjectAndCreate(t *testing.T) {
	current := immutableAdmissionModelServing(workloadv1alpha1.ServingGroupRollingUpdate)
	result := immutableAdmissionResponse(t, nil, current, admissionv1.Create)
	require.True(t, result.Allowed, "%+v", result.Result)
	for _, tc := range []struct {
		name    string
		raw     []byte
		message string
	}{
		{"missing", nil, "old ModelServing is required"},
		{"null", []byte(`null`), "old ModelServing is required"},
		{"wrong object type", []byte(`"invalid"`), "decode old ModelServing"},
		{"wrong spec type", []byte(`{"spec":"invalid"}`), "decode old ModelServing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := immutableAdmissionResponse(t, tc.raw, current, admissionv1.Update)
			require.False(t, result.Allowed)
			require.Contains(t, result.Result.Message, tc.message)
		})
	}
}

func TestImmutableAdmissionRetainsPodAndEvictionValidation(t *testing.T) {
	old := immutableAdmissionModelServing(workloadv1alpha1.ServingGroupRollingUpdate)
	oldRaw, err := json.Marshal(old)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func(*workloadv1alpha1.ModelServing)
		field  string
	}{
		{"worker requires template", func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].WorkerReplicas = 1 }, "workerTemplate"},
		{"eviction cannot exceed replicas", func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.RolloutStrategy.EvictionStrategy = &workloadv1alpha1.EvictionStrategySpec{
				ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
				MinAvailable:    ptr.To(intstr.FromInt32(2)),
			}
		}, "minAvailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := old.DeepCopy()
			tc.change(current)
			result := immutableAdmissionResponse(t, oldRaw, current, admissionv1.Update)
			require.False(t, result.Allowed)
			require.Contains(t, result.Result.Message, tc.field)
		})
	}
}
