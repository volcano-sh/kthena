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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	volcanoV1Beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// TestNetworkTopologyChangeTriggersRevision verifies that modifying networkTopology
// policy triggers a revision change, which should initiate a rolling update.
func TestNetworkTopologyChangeTriggersRevision(t *testing.T) {
	tests := []struct {
		name                 string
		baseMS               *workloadv1alpha1.ModelServing
		modifyFunc           func(*workloadv1alpha1.ModelServing)
		expectRevisionChange bool
		description          string
	}{
		{
			name: "networkTopology_groupPolicy_mode_change",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "prefill", Replicas: ptr.To[int32](1)},
						},
						NetworkTopology: &workloadv1alpha1.NetworkTopology{
							GroupPolicy: &volcanoV1Beta1.NetworkTopologySpec{
								Mode:               "hard",
								HighestTierAllowed: ptr.To[int](1),
							},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.NetworkTopology.GroupPolicy.Mode = "soft"
			},
			expectRevisionChange: true,
			description:          "Changing networkTopology.groupPolicy.mode should trigger revision change",
		},
		{
			name: "networkTopology_groupPolicy_tier_change",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "prefill", Replicas: ptr.To[int32](1)},
						},
						NetworkTopology: &workloadv1alpha1.NetworkTopology{
							GroupPolicy: &volcanoV1Beta1.NetworkTopologySpec{
								Mode:               "hard",
								HighestTierAllowed: ptr.To[int](1),
							},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.NetworkTopology.GroupPolicy.HighestTierAllowed = ptr.To[int](2)
			},
			expectRevisionChange: true,
			description:          "Changing networkTopology.groupPolicy.highestTierAllowed should trigger revision change",
		},
		{
			name: "networkTopology_rolePolicy_change",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "prefill", Replicas: ptr.To[int32](1)},
						},
						NetworkTopology: &workloadv1alpha1.NetworkTopology{
							RolePolicy: &volcanoV1Beta1.NetworkTopologySpec{
								Mode:               "hard",
								HighestTierAllowed: ptr.To[int](1),
							},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.NetworkTopology.RolePolicy.HighestTierAllowed = ptr.To[int](3)
			},
			expectRevisionChange: true,
			description:          "Changing networkTopology.rolePolicy should trigger revision change",
		},
		{
			name: "adding_networkTopology",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "prefill", Replicas: ptr.To[int32](1)},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{
					GroupPolicy: &volcanoV1Beta1.NetworkTopologySpec{
						Mode:               "hard",
						HighestTierAllowed: ptr.To[int](1),
					},
				}
			},
			expectRevisionChange: true,
			description:          "Adding networkTopology should trigger revision change",
		},
		{
			name: "gangPolicy_addition",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "prefill", Replicas: ptr.To[int32](1)},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{
					MinRoleReplicas: map[string]int32{
						"prefill": 1,
					},
				}
			},
			expectRevisionChange: true,
			description:          "Adding gangPolicy should trigger revision change",
		},
		{
			name: "role_replicas_only_change",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "prefill", Replicas: ptr.To[int32](1)},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
			},
			expectRevisionChange: false,
			description:          "Changing only role.replicas should NOT trigger revision change",
		},
		{
			name: "role_container_spec_change",
			baseMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Metadata: &workloadv1alpha1.Metadata{
										Labels: map[string]string{"app": "v1"},
									},
								},
							},
						},
					},
				},
			},
			modifyFunc: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Metadata.Labels["app"] = "v2"
			},
			expectRevisionChange: true,
			description:          "Changing role container specs should trigger revision change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Calculate initial revision
			copy1 := utils.RemoveRoleReplicasForRevision(tt.baseMS)
			revision1 := utils.Revision(copy1.Spec.Template)

			// Modify the ModelServing
			modifiedMS := tt.baseMS.DeepCopy()
			tt.modifyFunc(modifiedMS)

			// Calculate new revision
			copy2 := utils.RemoveRoleReplicasForRevision(modifiedMS)
			revision2 := utils.Revision(copy2.Spec.Template)

			// Verify expectations
			if tt.expectRevisionChange {
				if revision1 == revision2 {
					t.Errorf("%s: Expected revision to change, but got same revision: %s\nDescription: %s",
						tt.name, revision1, tt.description)
				}
			} else {
				if revision1 != revision2 {
					t.Errorf("%s: Expected revision to remain same, but got different revisions: %s vs %s\nDescription: %s",
						tt.name, revision1, revision2, tt.description)
				}
			}
		})
	}
}

// TestRevisionConsistency verifies that the same template produces the same revision hash
func TestRevisionConsistency(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{Name: "prefill", Replicas: ptr.To[int32](1)},
				},
				NetworkTopology: &workloadv1alpha1.NetworkTopology{
					GroupPolicy: &volcanoV1Beta1.NetworkTopologySpec{
						Mode:               "hard",
						HighestTierAllowed: ptr.To[int](1),
					},
				},
			},
		},
	}

	// Calculate revision multiple times
	copy1 := utils.RemoveRoleReplicasForRevision(ms)
	revision1 := utils.Revision(copy1.Spec.Template)

	copy2 := utils.RemoveRoleReplicasForRevision(ms)
	revision2 := utils.Revision(copy2.Spec.Template)

	copy3 := utils.RemoveRoleReplicasForRevision(ms)
	revision3 := utils.Revision(copy3.Spec.Template)

	if revision1 != revision2 || revision2 != revision3 {
		t.Errorf("Expected consistent revision hashes, got: %s, %s, %s", revision1, revision2, revision3)
	}
}
