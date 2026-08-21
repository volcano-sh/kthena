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

package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestBuildRevisionDataCanonicalSemantics(t *testing.T) {
	prefill := revisionTestRole("prefill", "prefill:v1")
	decode := revisionTestRole("decode", "decode:v1")
	base := revisionTestModelServing(prefill, decode)
	base.Spec.Plugins = []workloadv1alpha1.PluginSpec{
		{
			Name:   "injector",
			Config: &apiextensionsv1.JSON{Raw: []byte(`{"z":1,"a":2}`)},
			Scope: &workloadv1alpha1.PluginScope{
				Roles: []string{"prefill", "decode"},
			},
		},
	}

	equivalent := base.DeepCopy()
	equivalent.Spec.SchedulerName = defaultSchedulerName
	equivalent.Spec.Template.Roles[0], equivalent.Spec.Template.Roles[1] =
		equivalent.Spec.Template.Roles[1], equivalent.Spec.Template.Roles[0]
	equivalent.Spec.Plugins[0].Type = workloadv1alpha1.PluginTypeBuiltIn
	equivalent.Spec.Plugins[0].Config.Raw = []byte(`{"a":2,"z":1}`)
	equivalent.Spec.Plugins[0].Scope.Roles[0], equivalent.Spec.Plugins[0].Scope.Roles[1] =
		equivalent.Spec.Plugins[0].Scope.Roles[1], equivalent.Spec.Plugins[0].Scope.Roles[0]
	equivalent.Spec.Plugins[0].Scope.Target = workloadv1alpha1.PluginTargetAll
	equivalent.Spec.Replicas = ptr.To[int32](9)
	equivalent.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
	equivalent.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.ServingGroupRollingUpdate,
	}
	equivalent.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](30)
	equivalent.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{
		MinRoleReplicas: map[string]int32{"prefill": 1},
	}
	equivalent.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{}
	for i := range equivalent.Spec.Template.Roles {
		equivalent.Spec.Template.Roles[i].Replicas = ptr.To[int32](7)
		equivalent.Spec.Template.Roles[i].MaxUnavailable = ptr.To(intstr.FromInt32(2))
		equivalent.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromInt32(1))
	}

	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatalf("BuildRevisionData(base) error = %v", err)
	}
	equivalentData, err := BuildRevisionData(equivalent)
	if err != nil {
		t.Fatalf("BuildRevisionData(equivalent) error = %v", err)
	}
	if string(baseData) != string(equivalentData) {
		t.Fatalf("equivalent ModelServings produced different data:\nbase: %s\nother: %s", baseData, equivalentData)
	}

	if got := base.Spec.Plugins[0].Scope.Roles; got[0] != "prefill" || got[1] != "decode" {
		t.Fatalf("BuildRevisionData mutated plugin scope roles: %v", got)
	}
	if got := base.Spec.Template.Roles[0].Name; got != "prefill" {
		t.Fatalf("BuildRevisionData mutated role order: first role = %q", got)
	}
	if got := base.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy; got != "" {
		t.Fatalf("BuildRevisionData mutated input restart policy to %q", got)
	}
}

func TestBuildRevisionDataDeduplicatesPluginScopeRoles(t *testing.T) {
	base := revisionTestModelServing(revisionTestRole("prefill", "prefill:v1"))
	base.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
		Name:  "plugin",
		Scope: &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}},
	}}
	equivalent := base.DeepCopy()
	equivalent.Spec.Plugins[0].Scope.Roles = []string{"prefill", "prefill"}

	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatalf("BuildRevisionData(base) error = %v", err)
	}
	equivalentData, err := BuildRevisionData(equivalent)
	if err != nil {
		t.Fatalf("BuildRevisionData(equivalent) error = %v", err)
	}
	if string(baseData) != string(equivalentData) {
		t.Fatalf("duplicate plugin scope roles produced different data:\nbase: %s\nother: %s", baseData, equivalentData)
	}
	if got := len(equivalent.Spec.Plugins[0].Scope.Roles); got != 2 {
		t.Fatalf("BuildRevisionData mutated input plugin scope roles: %v", equivalent.Spec.Plugins[0].Scope.Roles)
	}
}

func TestBuildRevisionDataTracksRevisionedFields(t *testing.T) {
	base := revisionTestModelServing(revisionTestRole("prefill", "prefill:v1"))
	base.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "first"}, {Name: "second"}}
	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatalf("BuildRevisionData(base) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*workloadv1alpha1.ModelServing)
	}{
		{name: "scheduler name", mutate: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.SchedulerName = "custom" }},
		{name: "plugin order", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins[0], ms.Spec.Plugins[1] = ms.Spec.Plugins[1], ms.Spec.Plugins[0]
		}},
		{name: "role template", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v2"
		}},
		{name: "role environment", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = append(
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env,
				corev1.EnvVar{Name: "CUSTOM_SETTING", Value: "enabled"},
			)
		}},
		{name: "worker replicas", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].WorkerReplicas++
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base.DeepCopy()
			tt.mutate(changed)
			changedData, err := BuildRevisionData(changed)
			if err != nil {
				t.Fatalf("BuildRevisionData(changed) error = %v", err)
			}
			if string(baseData) == string(changedData) {
				t.Fatalf("revisioned field change did not change data: %s", changedData)
			}
		})
	}
}

func TestBuildRevisionDataNormalizesModelServingDefaultsAndEmptyValues(t *testing.T) {
	base := revisionTestModelServing(revisionTestRole("prefill", "prefill:v1"))
	base.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "plugin"}}

	equivalent := base.DeepCopy()
	equivalent.Spec.SchedulerName = defaultSchedulerName
	equivalent.Spec.Plugins[0].Type = workloadv1alpha1.PluginTypeBuiltIn
	equivalent.Spec.Plugins[0].Config = &apiextensionsv1.JSON{Raw: []byte("null")}
	equivalent.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{
		Roles:  []string{},
		Target: workloadv1alpha1.PluginTargetAll,
	}
	for i := range equivalent.Spec.Template.Roles {
		role := &equivalent.Spec.Template.Roles[i]
		role.EntryTemplate.Metadata = &workloadv1alpha1.Metadata{}
		role.EntryTemplate.Spec.SchedulerName = "ignored-template-scheduler"
		role.WorkerTemplate.Metadata = &workloadv1alpha1.Metadata{}
		role.WorkerTemplate.Spec.SchedulerName = "ignored-template-scheduler"
	}

	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatalf("BuildRevisionData(base) error = %v", err)
	}
	equivalentData, err := BuildRevisionData(equivalent)
	if err != nil {
		t.Fatalf("BuildRevisionData(equivalent) error = %v", err)
	}
	if string(baseData) != string(equivalentData) {
		t.Fatalf("defaults and empty values produced different data:\nbase: %s\nother: %s", baseData, equivalentData)
	}
}

func TestBuildRevisionDataPreservesPodAPIDefaultIntent(t *testing.T) {
	base := revisionTestModelServing(revisionTestRole("prefill", "prefill:v1"))
	explicit := base.DeepCopy()
	explicit.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways

	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatalf("BuildRevisionData(base) error = %v", err)
	}
	explicitData, err := BuildRevisionData(explicit)
	if err != nil {
		t.Fatalf("BuildRevisionData(explicit) error = %v", err)
	}
	if string(baseData) == string(explicitData) {
		t.Fatalf("explicit Pod API default did not change revision data: %s", explicitData)
	}
}

func TestBuildRevisionDataIgnoresControllerOwnedEnvValues(t *testing.T) {
	base := revisionTestModelServing(revisionTestRole("prefill", "prefill:v1"))
	role := &base.Spec.Template.Roles[0]
	role.EntryTemplate.Spec.InitContainers = []corev1.Container{{
		Name: "entry-init", Image: "init:v1", Env: []corev1.EnvVar{{Name: "KEEP", Value: "entry-init"}},
	}}
	role.EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KEEP", Value: "entry"}}
	role.WorkerTemplate.Spec.InitContainers = []corev1.Container{{
		Name: "worker-init", Image: "init:v1", Env: []corev1.EnvVar{{Name: "KEEP", Value: "worker-init"}},
	}}
	role.WorkerTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "KEEP", Value: "worker"}}

	equivalent := base.DeepCopy()
	for _, template := range []*workloadv1alpha1.PodTemplateSpec{
		&equivalent.Spec.Template.Roles[0].EntryTemplate,
		equivalent.Spec.Template.Roles[0].WorkerTemplate,
	} {
		for i := range template.Spec.InitContainers {
			addControllerOwnedEnvForTest(&template.Spec.InitContainers[i])
		}
		for i := range template.Spec.Containers {
			addControllerOwnedEnvForTest(&template.Spec.Containers[i])
		}
	}

	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatalf("BuildRevisionData(base) error = %v", err)
	}
	equivalentData, err := BuildRevisionData(equivalent)
	if err != nil {
		t.Fatalf("BuildRevisionData(equivalent) error = %v", err)
	}
	if string(baseData) != string(equivalentData) {
		t.Fatalf("controller-owned environment values produced different data:\nbase: %s\nother: %s", baseData, equivalentData)
	}
	if got := len(equivalent.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env); got != 4 {
		t.Fatalf("BuildRevisionData mutated input environment: %v", equivalent.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env)
	}
}

func addControllerOwnedEnvForTest(container *corev1.Container) {
	container.Env = append(container.Env,
		corev1.EnvVar{Name: workloadv1alpha1.GroupSizeEnv, Value: "invalid-group-size"},
		corev1.EnvVar{Name: workloadv1alpha1.EntryAddressEnv, Value: "invalid-entry-address"},
		corev1.EnvVar{Name: workloadv1alpha1.WorkerIndexEnv, Value: "invalid-worker-index"},
	)
}

func TestBuildRevisionDataPreservesPluginConfigNumberPrecision(t *testing.T) {
	ms := revisionTestModelServing(revisionTestRole("prefill", "prefill:v1"))
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
		Name:   "plugin",
		Config: &apiextensionsv1.JSON{Raw: []byte(`{"value":9007199254740993}`)},
	}}

	data, err := BuildRevisionData(ms)
	if err != nil {
		t.Fatalf("BuildRevisionData() error = %v", err)
	}
	if !bytes.Contains(data, []byte(`9007199254740993`)) {
		t.Fatalf("revision data changed plugin config number precision: %s", data)
	}
}

func TestBuildRevisionDataIsStrategicMergePatchWithReplaceMembership(t *testing.T) {
	current := revisionTestModelServing(
		revisionTestRole("kept", "kept:current"),
		revisionTestRole("removed", "removed:current"),
	)
	current.Spec.Replicas = ptr.To[int32](4)
	current.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "removed-plugin"}}
	target := revisionTestModelServing(revisionTestRole("kept", "kept:target"))
	target.Spec.Plugins = []workloadv1alpha1.PluginSpec{}

	patch, err := BuildRevisionData(target)
	if err != nil {
		t.Fatalf("BuildRevisionData() error = %v", err)
	}
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal current ModelServing: %v", err)
	}
	patchedData, err := strategicpatch.StrategicMergePatch(currentData, patch, workloadv1alpha1.ModelServing{})
	if err != nil {
		t.Fatalf("StrategicMergePatch() error = %v", err)
	}
	var patched workloadv1alpha1.ModelServing
	if err := json.Unmarshal(patchedData, &patched); err != nil {
		t.Fatalf("unmarshal patched ModelServing: %v", err)
	}
	if len(patched.Spec.Template.Roles) != 1 || patched.Spec.Template.Roles[0].Name != "kept" {
		t.Fatalf("patched roles = %#v, want only kept", patched.Spec.Template.Roles)
	}
	if got := patched.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image; got != "kept:target" {
		t.Fatalf("patched image = %q, want kept:target", got)
	}
	if len(patched.Spec.Plugins) != 0 {
		t.Fatalf("patched plugins = %#v, want empty", patched.Spec.Plugins)
	}
	if patched.Spec.Replicas == nil || *patched.Spec.Replicas != 4 {
		t.Fatalf("patched replicas = %v, want preserved value 4", patched.Spec.Replicas)
	}
}

func TestApplyRevisionPreservesOperationalFields(t *testing.T) {
	current := revisionTestModelServing(
		revisionTestRole("decode", "decode:current"),
		revisionTestRole("prefill", "prefill:current"),
		revisionTestRole("removed", "removed:current"),
	)
	current.Spec.Replicas = ptr.To[int32](4)
	current.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
	current.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](20)
	current.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{}
	current.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{}
	for i := range current.Spec.Template.Roles {
		current.Spec.Template.Roles[i].Replicas = ptr.To(int32(i + 2))
		current.Spec.Template.Roles[i].MaxUnavailable = ptr.To(intstr.FromInt32(1))
	}

	target := revisionTestModelServing(
		revisionTestRole("restored", "restored:old"),
		revisionTestRole("prefill", "prefill:old"),
		revisionTestRole("decode", "decode:old"),
	)
	target.Spec.SchedulerName = "historical-scheduler"
	target.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "historical-plugin"}}
	data, err := BuildRevisionData(target)
	if err != nil {
		t.Fatalf("BuildRevisionData(target) error = %v", err)
	}
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-revision",
			Annotations: map[string]string{
				ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1,
			},
		},
		Data: runtime.RawExtension{Raw: data},
	}

	applied, err := ApplyRevision(current, revision)
	if err != nil {
		t.Fatalf("ApplyRevision() error = %v", err)
	}
	if got := applied.Spec.SchedulerName; got != "historical-scheduler" {
		t.Errorf("schedulerName = %q, want historical-scheduler", got)
	}
	if got := applied.Spec.Plugins[0].Name; got != "historical-plugin" {
		t.Errorf("plugin = %q, want historical-plugin", got)
	}
	if got := *applied.Spec.Replicas; got != 4 {
		t.Errorf("replicas = %d, want 4", got)
	}
	if got := applied.Spec.RecoveryPolicy; got != workloadv1alpha1.NoneRestartPolicy {
		t.Errorf("recoveryPolicy = %q, want %q", got, workloadv1alpha1.NoneRestartPolicy)
	}
	if got := *applied.Spec.Template.RestartGracePeriodSeconds; got != 20 {
		t.Errorf("restartGracePeriodSeconds = %d, want 20", got)
	}
	if applied.Spec.Template.GangPolicy == nil || applied.Spec.Template.NetworkTopology == nil {
		t.Error("operational ServingGroup fields were not preserved")
	}

	wantNames := []string{"decode", "prefill", "restored"}
	for i, want := range wantNames {
		if got := applied.Spec.Template.Roles[i].Name; got != want {
			t.Errorf("role[%d].name = %q, want %q", i, got, want)
		}
		if got := applied.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image; got != want+":old" {
			t.Errorf("role[%d].image = %q, want %q", i, got, want+":old")
		}
	}
	if got := *applied.Spec.Template.Roles[0].Replicas; got != 2 {
		t.Errorf("decode replicas = %d, want 2", got)
	}
	if got := *applied.Spec.Template.Roles[1].Replicas; got != 3 {
		t.Errorf("prefill replicas = %d, want 3", got)
	}
	if got := *applied.Spec.Template.Roles[2].Replicas; got != 1 {
		t.Errorf("restored replicas = %d, want 1", got)
	}
	if applied.Spec.Template.Roles[0].MaxUnavailable == nil || applied.Spec.Template.Roles[1].MaxUnavailable == nil {
		t.Error("rolling update configuration was not preserved for existing roles")
	}
	if applied.Spec.Template.Roles[2].MaxUnavailable != nil {
		t.Error("restored role inherited rolling update configuration")
	}
	if len(applied.Spec.Template.Roles) != 3 {
		t.Fatalf("roles = %d, want 3", len(applied.Spec.Template.Roles))
	}
	if got := current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image; got != "decode:current" {
		t.Errorf("ApplyRevision mutated input image to %q", got)
	}
}

func TestRevisionDataHashUsesCollisionCount(t *testing.T) {
	data := []byte(`{"spec":{}}`)
	unsalted := RevisionDataHash(data, nil)
	if repeated := RevisionDataHash(append([]byte(nil), data...), nil); unsalted != repeated {
		t.Fatal("RevisionDataHash is not deterministic")
	}
	if unsalted == RevisionDataHash(data, ptr.To[int32](1)) {
		t.Fatal("collision count did not salt revision hash")
	}
	wantHasher := fnv.New32()
	_, _ = wantHasher.Write(data)
	_, _ = wantHasher.Write([]byte("1"))
	want := rand.SafeEncodeString(fmt.Sprint(wantHasher.Sum32()))
	if got := RevisionDataHash(data, ptr.To[int32](1)); got != want {
		t.Fatalf("RevisionDataHash() = %q, want Kubernetes-compatible hash %q", got, want)
	}
}

func TestGenerateControllerRevisionNameBoundsLongPrefix(t *testing.T) {
	prefix := strings.Repeat("a", 240)
	hash := "1234567890"
	got := GenerateControllerRevisionName(prefix, hash)
	if want := strings.Repeat("a", 223) + "-" + hash; got != want {
		t.Fatalf("GenerateControllerRevisionName() = %q, want %q", got, want)
	}
}

func revisionTestModelServing(roles ...workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{Roles: roles},
		},
	}
}

func revisionTestRole(name, image string) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name: name,
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: name, Image: image}},
			},
		},
		WorkerReplicas: 1,
		WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: name + "-worker", Image: image}},
			},
		},
	}
}
