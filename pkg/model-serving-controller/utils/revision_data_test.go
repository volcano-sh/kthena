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
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestBuildRevisionDataCanonicalProjection(t *testing.T) {
	base := revisionTestModelServing(
		revisionTestRole("prefill", "prefill:v1"),
		revisionTestRole("decode", "decode:v1"),
	)
	base.Spec.Plugins = []workloadv1alpha1.PluginSpec{
		{
			Name:   "first",
			Config: &apiextensionsv1.JSON{Raw: []byte(`{"z":1,"a":2,"value":9007199254740993}`)},
			Scope:  &workloadv1alpha1.PluginScope{Roles: []string{"decode", "prefill"}},
		},
		{Name: "second"},
	}
	base.OwnerReferences = []metav1.OwnerReference{{Kind: "LeaderWorkerSet", Name: "owner", UID: "owner"}}
	base.Labels = map[string]string{"live": "value"}
	base.Annotations = map[string]string{"live": "value"}
	baseData, err := BuildRevisionData(base)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*workloadv1alpha1.ModelServing)
		equal  bool
	}{
		{
			name: "canonical defaults and ordering",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.SchedulerName = defaultSchedulerName
				ms.Spec.Template.Roles[0], ms.Spec.Template.Roles[1] = ms.Spec.Template.Roles[1], ms.Spec.Template.Roles[0]
				ms.Spec.Plugins[0].Type = workloadv1alpha1.PluginTypeBuiltIn
				ms.Spec.Plugins[0].Config.Raw = []byte(`{"value":9007199254740993,"a":2,"z":1}`)
				ms.Spec.Plugins[0].Scope.Roles = []string{"prefill", "decode", "decode"}
				ms.Spec.Plugins[0].Scope.Target = workloadv1alpha1.PluginTargetAll
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].EntryTemplate.Metadata = &workloadv1alpha1.Metadata{}
					ms.Spec.Template.Roles[i].EntryTemplate.Spec.SchedulerName = "ignored"
				}
			},
			equal: true,
		},
		{
			name:   "scheduler name",
			mutate: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.SchedulerName = "custom" },
		},
		{
			name: "plugin order",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins[0], ms.Spec.Plugins[1] = ms.Spec.Plugins[1], ms.Spec.Plugins[0]
			},
		},
		{
			name: "plugin name, type, config, and scope",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins[0].Name = "renamed"
				ms.Spec.Plugins[0].Type = workloadv1alpha1.PluginType("custom")
				ms.Spec.Plugins[0].Config = &apiextensionsv1.JSON{Raw: []byte(`{"a":3}`)}
				ms.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}}
			},
		},
		{
			name: "role template and worker replicas",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v2"
				ms.Spec.Template.Roles[0].WorkerReplicas++
			},
		},
		{
			name: "pod environment",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = append(
					ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env,
					corev1.EnvVar{Name: "CUSTOM_SETTING", Value: "enabled"},
				)
			},
		},
		{
			name: "explicit pod restart policy",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
			},
		},
		{
			name: "operational and live metadata",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](9)
				ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate}
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](30)
				ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1}}
				ms.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{}
				ms.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "new", UID: "new"}}
				ms.Labels = map[string]string{"live": "changed"}
				ms.Annotations = map[string]string{"live": "changed"}
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](7)
					ms.Spec.Template.Roles[i].MaxUnavailable = ptr.To(intstr.FromInt(2))
					ms.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromInt(1))
					ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
						{Name: workloadv1alpha1.GroupSizeEnv, Value: "injected"},
						{Name: workloadv1alpha1.EntryAddressEnv, Value: "injected"},
						{Name: workloadv1alpha1.WorkerIndexEnv, Value: "injected"},
					}
				}
			},
			equal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base.DeepCopy()
			tt.mutate(changed)
			changedData, err := BuildRevisionData(changed)
			if err != nil {
				t.Fatal(err)
			}
			if equal := bytes.Equal(baseData, changedData); equal != tt.equal {
				t.Fatalf("canonical data equality = %t, want %t:\nbase: %s\nchanged: %s", equal, tt.equal, baseData, changedData)
			}
		})
	}
}

func TestRoleRevisionHashUsesApplicableCanonicalInputs(t *testing.T) {
	ms := revisionTestModelServing(
		revisionTestRole("decode", "decode:v1"),
		revisionTestRole("prefill", "prefill:v1"),
	)
	ms.Spec.Template.Roles[0].WorkerReplicas = 0
	ms.Spec.Template.Roles[0].WorkerTemplate = nil
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{
		{Name: "global", Type: workloadv1alpha1.PluginTypeBuiltIn},
		{Name: "worker", Scope: &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetWorker}},
		{Name: "decode", Scope: &workloadv1alpha1.PluginScope{Roles: []string{"decode"}}, Config: &apiextensionsv1.JSON{Raw: []byte(`{"z":1,"a":2}`)}},
	}
	base, err := RoleRevisionHash(ms, "decode")
	if err != nil {
		t.Fatal(err)
	}
	data, err := BuildRevisionData(ms)
	if err != nil {
		t.Fatal(err)
	}
	fromData, err := RoleRevisionHashFromRevisionData(data, "decode")
	if err != nil {
		t.Fatal(err)
	}
	if fromData != base {
		t.Fatalf("Role identity from revision data = %q, want %q", fromData, base)
	}

	tests := []struct {
		name   string
		mutate func(*workloadv1alpha1.ModelServing)
	}{
		{name: "scheduler", mutate: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.SchedulerName = "custom" }},
		{name: "applicable plugin config", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins[2].Config = &apiextensionsv1.JSON{Raw: []byte(`{"a":3}`)}
		}},
		{name: "applicable plugin order", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins[0], ms.Spec.Plugins[2] = ms.Spec.Plugins[2], ms.Spec.Plugins[0]
		}},
		{name: "applicable plugin scope", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins[2].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := ms.DeepCopy()
			tt.mutate(changed)
			got, err := RoleRevisionHash(changed, "decode")
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Fatalf("RoleRevisionHash did not change for %s", tt.name)
			}
		})
	}
}

func TestApplyRevisionPreservesOperationalFields(t *testing.T) {
	current := revisionTestModelServing(
		revisionTestRole("decode", "decode:current"),
		revisionTestRole("prefill", "prefill:current"),
		revisionTestRole("removed", "removed:current"),
	)
	current.ObjectMeta = metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: "test-uid"}
	current.OwnerReferences = []metav1.OwnerReference{{Kind: "LeaderWorkerSet", Name: "current-owner", UID: "current-owner"}}
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
			Name:      "test-revision",
			Namespace: current.Namespace,
			Annotations: map[string]string{
				ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1,
			},
			OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(current)},
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
	if !reflect.DeepEqual(applied.OwnerReferences, current.OwnerReferences) {
		t.Fatal("ApplyRevision changed current ModelServing ownership")
	}
	recovered, err := ModelServingForControllerRevision(current, revision)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recovered.OwnerReferences, current.OwnerReferences) {
		t.Fatal("ModelServingForControllerRevision changed current ModelServing ownership")
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
	if applied.Spec.Template.Roles[0].MaxUnavailable == nil || applied.Spec.Template.Roles[1].MaxUnavailable == nil {
		t.Error("rolling update configuration was not preserved for existing roles")
	}
	if applied.Spec.Template.Roles[2].Replicas == nil || *applied.Spec.Template.Roles[2].Replicas != 1 {
		t.Errorf("historical-only v1 Role replicas = %v, want API default 1", applied.Spec.Template.Roles[2].Replicas)
	}
	if len(applied.Spec.Template.Roles) != 3 {
		t.Fatalf("roles = %d, want 3", len(applied.Spec.Template.Roles))
	}
	if got := current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image; got != "decode:current" {
		t.Errorf("ApplyRevision mutated input image to %q", got)
	}
}

func TestApplyRevisionRejectsRevisionNotControlledByModelServing(t *testing.T) {
	ms := revisionTestModelServing(revisionTestRole("role", "current"))
	ms.ObjectMeta = metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: "test-uid"}
	data, err := BuildRevisionData(ms)
	if err != nil {
		t.Fatalf("BuildRevisionData() error = %v", err)
	}

	tests := []struct {
		name       string
		ownerUID   string
		includeRef bool
	}{
		{name: "no owner"},
		{name: "different owner", ownerUID: "other-uid", includeRef: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			revision := &appsv1.ControllerRevision{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-revision",
					Namespace:   ms.Namespace,
					Annotations: map[string]string{ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1},
				},
				Data: runtime.RawExtension{Raw: data},
			}
			if tt.includeRef {
				owner := newModelServingOwnerRef(ms)
				owner.UID = types.UID(tt.ownerUID)
				revision.OwnerReferences = []metav1.OwnerReference{owner}
			}

			if _, err := ApplyRevision(ms, revision); err == nil {
				t.Fatal("ApplyRevision() error = nil")
			}
		})
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

func TestModelServingForControllerRevisionPreservesLegacyOperationalFields(t *testing.T) {
	legacyRoles := []workloadv1alpha1.Role{
		revisionTestRole("prefill", "prefill:old"),
		revisionTestRole("restored", "restored:old"),
	}
	legacyRoles[0].Replicas = ptr.To[int32](2)
	legacyRoles[1].Replicas = ptr.To[int32](4)
	legacyRoles[0].RollingUpdateConfiguration.MaxUnavailable = ptr.To(intstr.FromInt(2))
	data, err := json.Marshal(map[string]interface{}{"data": legacyRoles})
	if err != nil {
		t.Fatal(err)
	}
	current := revisionTestModelServing(revisionTestRole("prefill", "prefill:new"))
	current.ObjectMeta = metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: "test-uid"}
	current.Spec.SchedulerName = "current-scheduler"
	current.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "current-plugin", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	current.Spec.Template.Roles[0].Replicas = ptr.To[int32](5)
	current.Spec.Template.Roles[0].RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(current)}},
		Data:       runtime.RawExtension{Raw: data},
	}

	got, err := ModelServingForControllerRevision(current, cr)
	if err != nil {
		t.Fatalf("ModelServingForControllerRevision() error = %v", err)
	}
	if got.Spec.SchedulerName != "current-scheduler" || len(got.Spec.Plugins) != 1 || got.Spec.Plugins[0].Name != "current-plugin" {
		t.Fatal("legacy revision changed fields that it never recorded")
	}
	if got.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image != "prefill:old" {
		t.Fatal("legacy Role template was not restored")
	}
	if got.Spec.Template.Roles[0].Replicas == nil || *got.Spec.Template.Roles[0].Replicas != 5 {
		t.Fatal("current Role replicas were not preserved")
	}
	if got.Spec.Template.Roles[0].Partition == nil || got.Spec.Template.Roles[0].Partition.IntValue() != 1 {
		t.Fatal("current Role rollout configuration was not preserved")
	}
	if len(got.Spec.Template.Roles) != 2 {
		t.Fatalf("legacy revision restored %d roles, want the historical-only role", len(got.Spec.Template.Roles))
	}
	if got.Spec.Template.Roles[1].Replicas == nil || *got.Spec.Template.Roles[1].Replicas != 1 {
		t.Fatal("historical-only legacy Role did not use the API default replica count")
	}
	if got.Spec.Template.Roles[1].RollingUpdateConfiguration != (workloadv1alpha1.RollingUpdateConfiguration{}) {
		t.Fatal("historical-only legacy Role restored operational rollout settings")
	}
}

func TestModelServingForControllerRevisionRejectsForeignLegacyRevision(t *testing.T) {
	current := revisionTestModelServing(revisionTestRole("prefill", "prefill:new"))
	current.ObjectMeta = metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: "test-uid"}
	data, err := json.Marshal(map[string]interface{}{
		"data": []workloadv1alpha1.Role{revisionTestRole("prefill", "prefill:old")},
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign := current.DeepCopy()
	foreign.UID = "foreign-uid"
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "foreign-legacy-revision",
			OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(foreign)},
		},
		Data: runtime.RawExtension{Raw: data},
	}

	if _, err := ModelServingForControllerRevision(current, cr); err == nil {
		t.Fatal("ModelServingForControllerRevision() error = nil")
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
