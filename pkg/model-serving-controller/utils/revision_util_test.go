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
	"hash/fnv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

var (
	nginxPodTemplate = workloadv1alpha1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}
)
var replicas int32 = 1

func TestRevision(t *testing.T) {
	role1 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}
	role2 := workloadv1alpha1.Role{
		Name:           "decode",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 2,
		WorkerTemplate: &nginxPodTemplate,
	}
	role3 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}

	hash1 := Revision(role1)
	hash2 := Revision(role2)
	hash3 := Revision(role3)

	if hash1 == hash2 {
		t.Errorf("Hash should be different for different objects, got %s and %s", hash1, hash3)
	}
	if hash1 != hash3 {
		t.Errorf("Hash should be equal for identical objects, got %s and %s", hash1, hash2)
	}
}

func TestDeepHashObject(t *testing.T) {
	hasher := fnv.New32()
	role1 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}
	DeepHashObject(hasher, role1)
	firstHash := hasher.Sum32()

	hasher.Reset()
	DeepHashObject(hasher, role1)
	secondHash := hasher.Sum32()

	if firstHash != secondHash {
		t.Errorf("DeepHashObject should produce the same hash for the same object, got %v and %v", firstHash, secondHash)
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

func newRole(name string, replicas *int32, workerReplicas int32) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:           name,
		Replicas:       replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: workerReplicas,
		WorkerTemplate: nil,
	}
}

func newModelServing(roles []workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: roles,
			},
		},
	}
}

func TestModelServingRevision(t *testing.T) {
	tests := []struct {
		name      string
		a         *workloadv1alpha1.ModelServing
		b         *workloadv1alpha1.ModelServing
		wantEqual bool
	}{
		{
			name:      "identical roles produce equal revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			wantEqual: true,
		},
		{
			name:      "different replicas produce equal revision (replicas ignored)",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(3), 0)}),
			wantEqual: true,
		},
		{
			name:      "nil replicas equals non-nil replicas (replicas ignored)",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", nil, 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(5), 0)}),
			wantEqual: true,
		},
		{
			name:      "different role name produces different revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("decode", int32Ptr(1), 0)}),
			wantEqual: false,
		},
		{
			name:      "different worker replicas produces different revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 2)}),
			wantEqual: false,
		},
		{
			name:      "different number of roles produces different revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0), newRole("decode", int32Ptr(1), 0)}),
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := ModelServingRevision(tt.a)
			gotB := ModelServingRevision(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Errorf("ModelServingRevision() equality = %v, want %v (a=%s, b=%s)", gotA == gotB, tt.wantEqual, gotA, gotB)
			}
		})
	}
}

func newModelServingWithGroupPolicies(topology *workloadv1alpha1.NetworkTopology, gangPolicy *workloadv1alpha1.GangPolicy) *workloadv1alpha1.ModelServing {
	ms := newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)})
	ms.Spec.Template.NetworkTopology = topology
	ms.Spec.Template.GangPolicy = gangPolicy
	return ms
}

func TestModelServingRevisionWithGroupPolicies(t *testing.T) {
	groupPolicy := func(tier int) *workloadv1alpha1.NetworkTopology {
		return &workloadv1alpha1.NetworkTopology{
			GroupPolicy: &schedulingv1beta1.NetworkTopologySpec{
				Mode:               schedulingv1beta1.HardNetworkTopologyMode,
				HighestTierAllowed: ptr.To(tier),
			},
		}
	}
	rolePolicy := &workloadv1alpha1.NetworkTopology{
		RolePolicy: &schedulingv1beta1.NetworkTopologySpec{
			Mode:               schedulingv1beta1.HardNetworkTopologyMode,
			HighestTierAllowed: ptr.To(1),
		},
	}
	gangPolicy := &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1}}

	tests := []struct {
		name      string
		a         *workloadv1alpha1.ModelServing
		b         *workloadv1alpha1.ModelServing
		wantEqual bool
	}{
		{
			name:      "empty network topology equals unset network topology",
			a:         newModelServingWithGroupPolicies(nil, nil),
			b:         newModelServingWithGroupPolicies(&workloadv1alpha1.NetworkTopology{}, nil),
			wantEqual: true,
		},
		{
			name:      "adding network topology produces different revision",
			a:         newModelServingWithGroupPolicies(nil, nil),
			b:         newModelServingWithGroupPolicies(groupPolicy(1), nil),
			wantEqual: false,
		},
		{
			name:      "changing group policy produces different revision",
			a:         newModelServingWithGroupPolicies(groupPolicy(1), nil),
			b:         newModelServingWithGroupPolicies(groupPolicy(2), nil),
			wantEqual: false,
		},
		{
			name:      "group policy and role policy produce different revisions",
			a:         newModelServingWithGroupPolicies(groupPolicy(1), nil),
			b:         newModelServingWithGroupPolicies(rolePolicy, nil),
			wantEqual: false,
		},
		{
			name:      "identical network topology produces equal revision",
			a:         newModelServingWithGroupPolicies(groupPolicy(1), nil),
			b:         newModelServingWithGroupPolicies(groupPolicy(1), nil),
			wantEqual: true,
		},
		{
			name:      "empty gang policy equals unset gang policy",
			a:         newModelServingWithGroupPolicies(nil, nil),
			b:         newModelServingWithGroupPolicies(nil, &workloadv1alpha1.GangPolicy{}),
			wantEqual: true,
		},
		{
			name:      "empty minRoleReplicas equals unset gang policy",
			a:         newModelServingWithGroupPolicies(nil, nil),
			b:         newModelServingWithGroupPolicies(nil, &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{}}),
			wantEqual: true,
		},
		{
			name:      "adding gang policy produces different revision",
			a:         newModelServingWithGroupPolicies(nil, nil),
			b:         newModelServingWithGroupPolicies(nil, gangPolicy),
			wantEqual: false,
		},
		{
			name:      "removing gang policy produces different revision",
			a:         newModelServingWithGroupPolicies(nil, gangPolicy),
			b:         newModelServingWithGroupPolicies(nil, &workloadv1alpha1.GangPolicy{}),
			wantEqual: false,
		},
		{
			name:      "adding gang policy alongside network topology produces different revision",
			a:         newModelServingWithGroupPolicies(groupPolicy(1), nil),
			b:         newModelServingWithGroupPolicies(groupPolicy(1), gangPolicy),
			wantEqual: false,
		},
		{
			name:      "identical gang policy and network topology produce equal revision",
			a:         newModelServingWithGroupPolicies(groupPolicy(1), gangPolicy),
			b:         newModelServingWithGroupPolicies(groupPolicy(1), gangPolicy),
			wantEqual: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := ModelServingRevision(tt.a)
			gotB := ModelServingRevision(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Errorf("ModelServingRevision() equality = %v, want %v (a=%s, b=%s)", gotA == gotB, tt.wantEqual, gotA, gotB)
			}
		})
	}
}

func TestModelServingRevisionWithoutGroupPoliciesHashesOnlyRoles(t *testing.T) {
	ms := newModelServingWithGroupPolicies(&workloadv1alpha1.NetworkTopology{}, &workloadv1alpha1.GangPolicy{})
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](30)

	want := Revision(removeRoleReplicasForRevision(ms).Spec.Template.Roles)
	if got := ModelServingRevision(ms); got != want {
		t.Errorf("ModelServingRevision() = %s, want roles-only revision %s", got, want)
	}
}

func TestMaxSurgeDoesNotChangeRevisionOrRoleTemplateHash(t *testing.T) {
	withoutSurge := newRole("decode", int32Ptr(2), 0)
	withSurge := *withoutSurge.DeepCopy()
	withSurge.MaxUnavailable = func() *intstr.IntOrString { value := intstr.FromInt(0); return &value }()
	withSurge.MaxSurge = func() *intstr.IntOrString { value := intstr.FromString("25%"); return &value }()
	withSurge.Partition = func() *intstr.IntOrString { value := intstr.FromInt(1); return &value }()

	if CalRoleTemplateHash(withoutSurge) != CalRoleTemplateHash(withSurge) {
		t.Fatal("rolling update policy must not change Role template hash")
	}
	if ModelServingRevision(newModelServing([]workloadv1alpha1.Role{withoutSurge})) !=
		ModelServingRevision(newModelServing([]workloadv1alpha1.Role{withSurge})) {
		t.Fatal("rolling update policy must not change ModelServing revision")
	}
}

func TestModelServingRevisionDoesNotMutateInput(t *testing.T) {
	replicaVal := int32(3)
	ms := newModelServing([]workloadv1alpha1.Role{
		newRole("prefill", &replicaVal, 0),
		newRole("decode", &replicaVal, 2),
	})

	ModelServingRevision(ms)

	for i, role := range ms.Spec.Template.Roles {
		if role.Replicas == nil {
			t.Errorf("role[%d].Replicas was mutated to nil, expected it to be preserved", i)
			continue
		}
		if *role.Replicas != replicaVal {
			t.Errorf("role[%d].Replicas = %d, want %d", i, *role.Replicas, replicaVal)
		}
	}
}

func TestCalRoleTemplateHash(t *testing.T) {
	tests := []struct {
		name      string
		a         workloadv1alpha1.Role
		b         workloadv1alpha1.Role
		wantEqual bool
	}{
		{
			name:      "identical roles produce equal hash",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("prefill", int32Ptr(1), 0),
			wantEqual: true,
		},
		{
			name:      "different replicas produce equal hash (replicas ignored)",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("prefill", int32Ptr(4), 0),
			wantEqual: true,
		},
		{
			name:      "nil replicas equals non-nil replicas (replicas ignored)",
			a:         newRole("prefill", nil, 0),
			b:         newRole("prefill", int32Ptr(2), 0),
			wantEqual: true,
		},
		{
			name:      "different role name produces different hash",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("decode", int32Ptr(1), 0),
			wantEqual: false,
		},
		{
			name:      "different worker replicas produces different hash",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("prefill", int32Ptr(1), 3),
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := CalRoleTemplateHash(tt.a)
			gotB := CalRoleTemplateHash(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Errorf("CalRoleTemplateHash() equality = %v, want %v (a=%s, b=%s)", gotA == gotB, tt.wantEqual, gotA, gotB)
			}
		})
	}
}

func TestCalRoleTemplateHashDoesNotMutateInput(t *testing.T) {
	replicaVal := int32(7)
	role := newRole("prefill", &replicaVal, 0)

	CalRoleTemplateHash(role)

	if role.Replicas == nil {
		t.Fatal("role.Replicas was mutated to nil, expected it to be preserved")
	}
	if *role.Replicas != replicaVal {
		t.Errorf("role.Replicas = %d, want %d", *role.Replicas, replicaVal)
	}
}
