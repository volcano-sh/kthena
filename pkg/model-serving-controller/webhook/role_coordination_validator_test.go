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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestValidateRoleCoordination(t *testing.T) {
	validSkew := intstr.FromString("10%")
	integerSkew := intstr.FromInt(10)
	zeroSkew := intstr.FromString("0%")

	tests := []struct {
		name              string
		mutate            func(*workloadv1alpha1.ModelServing)
		expectedSubstring string
	}{
		{name: "valid dependency DAG"},
		{
			name: "coordination requires RoleRollingUpdate",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.Type = workloadv1alpha1.ServingGroupRollingUpdate
			},
			expectedSubstring: "only valid when rolloutStrategy.type is RoleRollingUpdate",
		},
		{
			name: "maxSkew is required",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.MaxSkew = nil
			},
			expectedSubstring: "maxSkew is required",
		},
		{
			name: "integer maxSkew is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.MaxSkew = &integerSkew
			},
			expectedSubstring: "must be a percentage string",
		},
		{
			name: "zero maxSkew is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.MaxSkew = &zeroSkew
			},
			expectedSubstring: "must be greater than 0%",
		},
		{
			name: "unknown selected Role is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"a", "unknown"}
			},
			expectedSubstring: "Not found",
		},
		{
			name: "dependency endpoint must be coordinated",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				coordination := ms.Spec.RolloutStrategy.RoleCoordination
				coordination.Roles = []string{"a", "b"}
			},
			expectedSubstring: "must belong to the coordinated Role set",
		},
		{
			name: "self dependency is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
					{Role: "a", DependsOn: []string{"a"}},
				}
			},
			expectedSubstring: "cannot depend on itself",
		},
		{
			name: "dependency cycle is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
					{Role: "a", DependsOn: []string{"b"}},
					{Role: "b", DependsOn: []string{"a"}},
				}
			},
			expectedSubstring: "dependency graph must be acyclic",
		},
		{
			name: "at least two Roles are required",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"a"}
				ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = nil
			},
			expectedSubstring: "at least two Roles",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.RoleRollingUpdate,
						RoleCoordination: &workloadv1alpha1.RoleCoordination{
							MaxSkew: &validSkew,
							Dependencies: []workloadv1alpha1.RoleRolloutDependency{
								{Role: "a", DependsOn: []string{"b"}},
								{Role: "b", DependsOn: []string{"c"}},
							},
						},
					},
					Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{
						{Name: "a", Replicas: ptr.To[int32](10)},
						{Name: "b", Replicas: ptr.To[int32](10)},
						{Name: "c", Replicas: ptr.To[int32](10)},
					}},
				},
			}
			if tt.mutate != nil {
				tt.mutate(ms)
			}

			errs := validateRoleCoordination(ms)
			if tt.expectedSubstring == "" {
				assert.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tt.expectedSubstring)
		})
	}
}

func TestValidateRoleCoordinationUpdateCapacity(t *testing.T) {
	validSkew := intstr.FromString("10%")
	role := func(name, image string, replicas int32) workloadv1alpha1.Role {
		return workloadv1alpha1.Role{
			Name:     name,
			Replicas: ptr.To(replicas),
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: image}}},
			},
		}
	}
	modelServing := func(image string, replicas int32) *workloadv1alpha1.ModelServing {
		return &workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{
				RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
					Type: workloadv1alpha1.RoleRollingUpdate,
					RoleCoordination: &workloadv1alpha1.RoleCoordination{
						MaxSkew: &validSkew,
						Dependencies: []workloadv1alpha1.RoleRolloutDependency{
							{Role: "a", DependsOn: []string{"b"}},
							{Role: "b", DependsOn: []string{"c"}},
						},
					},
				},
				Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{
					role("a", image, replicas), role("b", image, replicas), role("c", image, replicas),
				}},
			},
		}
	}

	t.Run("multi-replica dependencies have replacement bootstrap capacity", func(t *testing.T) {
		oldMS := modelServing("old", 2)
		newMS := modelServing("new", 2)
		assert.Empty(t, validateRoleCoordinationUpdate(oldMS, newMS))
	})

	t.Run("fully partitioned changed dependency is rejected", func(t *testing.T) {
		oldMS := modelServing("old", 2)
		newMS := modelServing("new", 2)
		partition := intstr.FromString("100%")
		newMS.Spec.Template.Roles[1].Partition = &partition

		errs := validateRoleCoordinationUpdate(oldMS, newMS)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "must leave target-version capacity outside partition")
	})

	t.Run("single replica dependency without concurrent capacity is rejected", func(t *testing.T) {
		oldMS := modelServing("old", 1)
		newMS := modelServing("new", 1)

		errs := validateRoleCoordinationUpdate(oldMS, newMS)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "cannot create target-version capacity while retaining the old request path")
		assert.Contains(t, errs.ToAggregate().Error(), "stage a scale-up before changing the Role template")
	})

	t.Run("ordinary scale up supplies dependency bootstrap capacity", func(t *testing.T) {
		oldMS := modelServing("old", 1)
		newMS := modelServing("new", 2)
		assert.Empty(t, validateRoleCoordinationUpdate(oldMS, newMS))
	})

	activeRollout := func(ms *workloadv1alpha1.ModelServing) {
		ms.Generation = 2
		ms.Status.ObservedGeneration = 2
		ms.Status.Conditions = []metav1.Condition{{
			Type:               string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked),
			Status:             metav1.ConditionTrue,
			Reason:             "DependencyNotReady",
			ObservedGeneration: 2,
		}}
	}

	t.Run("in-progress dependency scale down below bootstrap capacity is rejected", func(t *testing.T) {
		oldMS := modelServing("new", 2)
		activeRollout(oldMS)
		newMS := oldMS.DeepCopy()
		newMS.Spec.Template.Roles[1].Replicas = ptr.To[int32](1)

		errs := validateRoleCoordinationUpdate(oldMS, newMS)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "must retain capacity for one old and one target replica")
	})

	t.Run("stale status conservatively rejects dependency scale down", func(t *testing.T) {
		oldMS := modelServing("new", 2)
		oldMS.Generation = 2
		oldMS.Status.ObservedGeneration = 1
		oldMS.Status.Conditions = []metav1.Condition{{
			Type:               string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked),
			Status:             metav1.ConditionFalse,
			Reason:             "RolloutComplete",
			ObservedGeneration: 1,
		}}
		newMS := oldMS.DeepCopy()
		newMS.Spec.Template.Roles[2].Replicas = ptr.To[int32](1)

		errs := validateRoleCoordinationUpdate(oldMS, newMS)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "must retain capacity for one old and one target replica")
	})

	t.Run("in-progress fully partitioned dependency is rejected", func(t *testing.T) {
		oldMS := modelServing("new", 2)
		activeRollout(oldMS)
		newMS := oldMS.DeepCopy()
		partition := intstr.FromString("100%")
		newMS.Spec.Template.Roles[2].Partition = &partition

		errs := validateRoleCoordinationUpdate(oldMS, newMS)
		require.NotEmpty(t, errs)
		assert.Contains(t, errs.ToAggregate().Error(), "must retain capacity for one old and one target replica")
	})

	t.Run("in-progress dependency keeps one target slot outside partition", func(t *testing.T) {
		oldMS := modelServing("new", 2)
		activeRollout(oldMS)
		newMS := oldMS.DeepCopy()
		newMS.Spec.Template.Roles[2].Replicas = ptr.To[int32](3)
		partition := intstr.FromInt(2)
		newMS.Spec.Template.Roles[2].Partition = &partition

		assert.Empty(t, validateRoleCoordinationUpdate(oldMS, newMS))
	})

	t.Run("dependency can scale down after coordinated rollout completes", func(t *testing.T) {
		oldMS := modelServing("new", 2)
		oldMS.Generation = 2
		oldMS.Status.ObservedGeneration = 2
		oldMS.Status.Conditions = []metav1.Condition{{
			Type:               string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked),
			Status:             metav1.ConditionFalse,
			Reason:             "RolloutComplete",
			ObservedGeneration: 2,
		}}
		newMS := oldMS.DeepCopy()
		newMS.Spec.Template.Roles[1].Replicas = ptr.To[int32](1)

		assert.Empty(t, validateRoleCoordinationUpdate(oldMS, newMS))
	})
}
