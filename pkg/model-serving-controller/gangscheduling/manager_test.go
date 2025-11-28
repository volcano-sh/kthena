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

package gangscheduling

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestCalculateRequirements(t *testing.T) {
	// Helper function to create a pod template
	createPodTemplate := func(name, cpu, memory string) *workloadv1alpha1.PodTemplateSpec {
		return &workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  name,
						Image: "test-image",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(cpu),
								corev1.ResourceMemory: resource.MustParse(memory),
							},
						},
					},
				},
			},
		}
	}

	// Helper function to create a basic ModelServing object
	createBasicModelServing := func() *workloadv1alpha1.ModelServing {
		return &workloadv1alpha1.ModelServing{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-model",
				Namespace: "default",
			},
			Spec: workloadv1alpha1.ModelServingSpec{
				Template: workloadv1alpha1.ServingGroup{
					Roles: []workloadv1alpha1.Role{
						{
							Name:           "prefill",
							Replicas:       ptr.To[int32](2),
							WorkerReplicas: 3,
							EntryTemplate:  *createPodTemplate("prefill-entry", "1", "2Gi"),
							WorkerTemplate: createPodTemplate("prefill-worker", "2", "4Gi"),
						},
						{
							Name:           "decode",
							Replicas:       ptr.To[int32](1),
							WorkerReplicas: 2,
							EntryTemplate:  *createPodTemplate("decode-entry", "1", "1Gi"),
							WorkerTemplate: createPodTemplate("decode-worker", "1", "2Gi"),
						},
					},
					GangPolicy: &workloadv1alpha1.GangPolicy{},
				},
			},
		}
	}

	t.Run("basic calculation", func(t *testing.T) {
		manager := NewManager(nil, nil)
		mi := createBasicModelServing()

		minMember, minTaskMember, minResources := manager.calculateRequirements(mi)

		// For 2 prefill roles (each with 1 entry + 3 workers) and 1 decode role (1 entry + 2 workers)
		// Total pods = (1+3)*2 + (1+2)*1 = 8 + 3 = 11
		assert.Equal(t, 11, minMember)

		// Check task members
		expectedTaskMembers := map[string]int32{
			"prefill-0": 4, // 1 entry + 3 workers
			"prefill-1": 4, // 1 entry + 3 workers
			"decode-0":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedTaskMembers, minTaskMember)

		// Check resources
		// Prefill roles: 2*(1cpu+2Gi) + 2*3*(2cpu+4Gi) = 2cpu+4Gi + 12cpu+24Gi = 14cpu+28Gi
		// Decode roles: 1*(1cpu+1Gi) + 1*2*(1cpu+2Gi) = 1cpu+1Gi + 2cpu+4Gi = 3cpu+5Gi
		// Total: 17cpu + 33Gi
		expectedCPU := resource.MustParse("17")
		expectedMemory := resource.MustParse("33Gi")

		assert.True(t, expectedCPU.Equal(minResources[corev1.ResourceCPU]),
			"Expected CPU %v, got %v", expectedCPU, minResources[corev1.ResourceCPU])
		assert.True(t, expectedMemory.Equal(minResources[corev1.ResourceMemory]),
			"Expected Memory %v, got %v", expectedMemory, minResources[corev1.ResourceMemory])
	})

	t.Run("with MinRoleReplicas constraint", func(t *testing.T) {
		manager := NewManager(nil, nil)
		mi := createBasicModelServing()

		// Set MinRoleReplicas to limit the number of roles considered
		minRoleReplicas := map[string]int32{
			"prefill": 1, // Only consider 1 prefill role instead of 2
			"decode":  1, // Consider all decode roles (1)
		}
		mi.Spec.Template.GangPolicy.MinRoleReplicas = minRoleReplicas

		minMember, minTaskMember, minResources := manager.calculateRequirements(mi)

		// For 1 prefill role (1 entry + 3 workers) and 1 decode role (1 entry + 2 workers)
		// Total pods = (1+3)*1 + (1+2)*1 = 4 + 3 = 7
		assert.Equal(t, 7, minMember)

		// Check task members - should only include prefill-0 and decode-0
		expectedTaskMembers := map[string]int32{
			"prefill-0": 4, // 1 entry + 3 workers
			"decode-0":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedTaskMembers, minTaskMember)

		// Check resources for limited roles
		// Prefill roles: 1*(1cpu+2Gi) + 1*3*(2cpu+4Gi) = 1cpu+2Gi + 6cpu+12Gi = 7cpu+14Gi
		// Decode roles: 1*(1cpu+1Gi) + 1*2*(1cpu+2Gi) = 1cpu+1Gi + 2cpu+4Gi = 3cpu+5Gi
		// Total: 10cpu + 19Gi
		expectedCPU := resource.MustParse("10")
		expectedMemory := resource.MustParse("19Gi")

		assert.True(t, expectedCPU.Equal(minResources[corev1.ResourceCPU]),
			"Expected CPU %v, got %v", expectedCPU, minResources[corev1.ResourceCPU])
		assert.True(t, expectedMemory.Equal(minResources[corev1.ResourceMemory]),
			"Expected Memory %v, got %v", expectedMemory, minResources[corev1.ResourceMemory])
	})

	t.Run("nil MinRoleReplicas", func(t *testing.T) {
		manager := NewManager(nil, nil)
		mi := createBasicModelServing()
		mi.Spec.Template.GangPolicy.MinRoleReplicas = nil

		minMember, _, _ := manager.calculateRequirements(mi)

		// Should consider all roles without constraint
		// Same as basic calculation: 11 pods
		assert.Equal(t, 11, minMember)
	})

	t.Run("empty roles", func(t *testing.T) {
		manager := NewManager(nil, nil)
		mi := createBasicModelServing()
		mi.Spec.Template.Roles = []workloadv1alpha1.Role{} // Empty roles

		minMember, minTaskMember, minResources := manager.calculateRequirements(mi)

		// Should have no requirements
		assert.Equal(t, 0, minMember)
		assert.Empty(t, minTaskMember)
		assert.Empty(t, minResources)
	})

	t.Run("role with no worker template", func(t *testing.T) {
		manager := NewManager(nil, nil)
		mi := createBasicModelServing()

		// Modify one role to have no worker template
		mi.Spec.Template.Roles[1].WorkerTemplate = nil
		mi.Spec.Template.Roles[1].WorkerReplicas = 0
		minMember, minTaskMember, _ := manager.calculateRequirements(mi)

		// For 2 prefill roles (each with 1 entry + 3 workers) and 1 decode role (1 entry only)
		// Total pods = (1+3)*2 + (1+0)*1 = 8 + 1 = 9
		assert.Equal(t, 9, minMember)

		// Check task members
		expectedTaskMembers := map[string]int32{
			"prefill-0": 4, // 1 entry + 3 workers
			"prefill-1": 4, // 1 entry + 3 workers
			"decode-0":  1, // 1 entry only (no workers)
		}
		assert.Equal(t, expectedTaskMembers, minTaskMember)
	})

	t.Run("zero worker replicas", func(t *testing.T) {
		manager := NewManager(nil, nil)
		mi := createBasicModelServing()

		// Set worker replicas to zero for one role
		mi.Spec.Template.Roles[0].WorkerReplicas = 0

		minMember, minTaskMember, _ := manager.calculateRequirements(mi)

		// For 2 prefill roles (each with 1 entry + 0 workers) and 1 decode role (1 entry + 2 workers)
		// Total pods = (1+0)*2 + (1+2)*1 = 2 + 3 = 5
		assert.Equal(t, 5, minMember)

		// Check task members
		expectedTaskMembers := map[string]int32{
			"prefill-0": 1, // 1 entry only (no workers)
			"prefill-1": 1, // 1 entry only (no workers)
			"decode-0":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedTaskMembers, minTaskMember)
	})
}

func TestAggregateResources(t *testing.T) {
	t.Run("basic aggregation", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
				{
					Name: "container2",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		}

		manager.aggregateResources(&total, podSpec)

		expectedCPU := resource.MustParse("3")
		expectedMemory := resource.MustParse("3Gi")

		assert.True(t, expectedCPU.Equal(total[corev1.ResourceCPU]))
		assert.True(t, expectedMemory.Equal(total[corev1.ResourceMemory]))
	})

	t.Run("nil total resource list", func(t *testing.T) {
		manager := NewManager(nil, nil)
		var total corev1.ResourceList = nil

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		}

		manager.aggregateResources(&total, podSpec)

		assert.NotNil(t, total)
		assert.Len(t, total, 1)
		assert.True(t, resource.MustParse("1").Equal(total[corev1.ResourceCPU]))
	})

	t.Run("empty containers", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{}, // Empty containers
		}

		manager.aggregateResources(&total, podSpec)

		assert.Empty(t, total)
	})

	t.Run("nil containers", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: nil, // Nil containers
		}

		manager.aggregateResources(&total, podSpec)

		assert.Empty(t, total)
	})

	t.Run("container with no resources", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					// No Resources field
				},
			},
		}

		manager.aggregateResources(&total, podSpec)

		assert.Empty(t, total)
	})

	t.Run("container with empty resources", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{}, // Empty requests
					},
				},
			},
		}

		manager.aggregateResources(&total, podSpec)

		assert.Empty(t, total)
	})

	t.Run("multiple calls to aggregate resources", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec1 := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		}

		podSpec2 := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container2",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		}

		// First call
		manager.aggregateResources(&total, podSpec1)
		assert.True(t, resource.MustParse("1").Equal(total[corev1.ResourceCPU]))

		// Second call
		manager.aggregateResources(&total, podSpec2)
		assert.True(t, resource.MustParse("3").Equal(total[corev1.ResourceCPU]))
	})

	t.Run("different resource types", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:              resource.MustParse("1"),
							corev1.ResourceMemory:           resource.MustParse("2Gi"),
							corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
						},
					},
				},
			},
		}

		manager.aggregateResources(&total, podSpec)

		assert.Len(t, total, 3)
		assert.True(t, resource.MustParse("1").Equal(total[corev1.ResourceCPU]))
		assert.True(t, resource.MustParse("2Gi").Equal(total[corev1.ResourceMemory]))
		assert.True(t, resource.MustParse("10Gi").Equal(total[corev1.ResourceEphemeralStorage]))
	})

	t.Run("existing resources get updated", func(t *testing.T) {
		manager := NewManager(nil, nil)
		total := corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"),
		}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		}

		manager.aggregateResources(&total, podSpec)

		// Should have 1+2=3 CPUs
		assert.True(t, resource.MustParse("3").Equal(total[corev1.ResourceCPU]))
	})
}

func TestGetExistingPodGroups(t *testing.T) {
	// Setup test objects
	modelServing := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
	}

	podGroup1 := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-0",
			Namespace: "default",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "test-model",
			},
		},
	}

	podGroup2 := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-1",
			Namespace: "default",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "test-model",
			},
		},
	}

	podGroup3 := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-model-0",
			Namespace: "default",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "other-model",
			},
		},
	}

	podGroupDifferentNamespace := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-0",
			Namespace: "other-namespace",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "test-model",
			},
		},
	}

	t.Run("successful retrieval of existing pod groups", func(t *testing.T) {
		// Create fake volcano client with test data
		fakeVolcanoClient := volcanofake.NewSimpleClientset(podGroup1, podGroup2, podGroup3, podGroupDifferentNamespace)
		manager := NewManager(nil, fakeVolcanoClient)

		result, err := manager.getExistingPodGroups(context.Background(), modelServing)

		// Assertions
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 2) // Should only contain pod groups for test-model in default namespace

		// Check if the correct pod groups are returned
		assert.Contains(t, result, "test-model-0")
		assert.Contains(t, result, "test-model-1")
		assert.NotContains(t, result, "other-model-0")

		// Check if the returned pod groups have correct data
		assert.Equal(t, "test-model-0", result["test-model-0"].Name)
		assert.Equal(t, "default", result["test-model-0"].Namespace)
		assert.Equal(t, "test-model-1", result["test-model-1"].Name)
		assert.Equal(t, "default", result["test-model-1"].Namespace)
	})

	t.Run("no existing pod groups", func(t *testing.T) {
		// Create fake volcano client with only unrelated pod groups
		fakeVolcanoClient := volcanofake.NewSimpleClientset(podGroup3)
		manager := NewManager(nil, fakeVolcanoClient)

		result, err := manager.getExistingPodGroups(context.Background(), modelServing)

		// Assertions
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 0) // Should be empty
	})

	t.Run("empty pod group list", func(t *testing.T) {
		// Create fake volcano client with no pod groups
		fakeVolcanoClient := volcanofake.NewSimpleClientset()
		manager := NewManager(nil, fakeVolcanoClient)

		result, err := manager.getExistingPodGroups(context.Background(), modelServing)

		// Assertions
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 0) // Should be empty
	})

	t.Run("pod group with same name in different namespace", func(t *testing.T) {
		// Create fake volcano client with pod groups
		fakeVolcanoClient := volcanofake.NewSimpleClientset(podGroup1, podGroupDifferentNamespace)
		manager := NewManager(nil, fakeVolcanoClient)

		result, err := manager.getExistingPodGroups(context.Background(), modelServing)

		// Should only get pod groups from the same namespace
		assert.NoError(t, err)
		assert.Len(t, result, 1)
		assert.Contains(t, result, "test-model-0")
		assert.Equal(t, "default", result["test-model-0"].Namespace)
		assert.NotEqual(t, "other-namespace", result["test-model-0"].Namespace)
	})

	t.Run("nil model Serving parameter", func(t *testing.T) {
		fakeVolcanoClient := volcanofake.NewSimpleClientset(podGroup1)
		manager := NewManager(nil, fakeVolcanoClient)

		// Test with nil ModelServing - this would cause a panic in the real code
		// but we're checking that our test handles it gracefully
		assert.Panics(t, func() {
			_, _ = manager.getExistingPodGroups(context.Background(), nil)
		})
	})
}

func TestEqualMinTaskMember(t *testing.T) {
	t.Run("equal maps", func(t *testing.T) {
		a := map[string]int32{
			"task1": 1,
			"task2": 2,
			"task3": 3,
		}
		b := map[string]int32{
			"task1": 1,
			"task2": 2,
			"task3": 3,
		}

		assert.True(t, equalMinTaskMember(a, b))
	})

	t.Run("unequal maps - different values", func(t *testing.T) {
		a := map[string]int32{
			"task1": 1,
			"task2": 2,
		}
		b := map[string]int32{
			"task1": 1,
			"task2": 3, // Different value
		}

		assert.False(t, equalMinTaskMember(a, b))
	})

	t.Run("unequal maps - different keys", func(t *testing.T) {
		a := map[string]int32{
			"task1": 1,
			"task2": 2,
		}
		b := map[string]int32{
			"task1": 1,
			"task3": 2, // Different key
		}

		assert.False(t, equalMinTaskMember(a, b))
	})

	t.Run("unequal maps - different lengths", func(t *testing.T) {
		a := map[string]int32{
			"task1": 1,
			"task2": 2,
		}
		b := map[string]int32{
			"task1": 1,
			"task2": 2,
			"task3": 3, // Extra key
		}

		assert.False(t, equalMinTaskMember(a, b))
	})

	t.Run("empty maps", func(t *testing.T) {
		a := map[string]int32{}
		b := map[string]int32{}

		assert.True(t, equalMinTaskMember(a, b))
	})

	t.Run("one empty map", func(t *testing.T) {
		a := map[string]int32{
			"task1": 1,
		}
		b := map[string]int32{}

		assert.False(t, equalMinTaskMember(a, b))
	})

	t.Run("both nil maps", func(t *testing.T) {
		var a map[string]int32 = nil
		var b map[string]int32 = nil

		assert.True(t, equalMinTaskMember(a, b))
	})

	t.Run("one nil map", func(t *testing.T) {
		var a map[string]int32 = nil
		b := map[string]int32{
			"task1": 1,
		}

		assert.False(t, equalMinTaskMember(a, b))
	})
}

func TestEqualResourceList(t *testing.T) {
	t.Run("equal resource lists", func(t *testing.T) {
		a := &corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
		b := &corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}

		assert.True(t, equalResourceList(a, b))
	})

	t.Run("unequal resource lists - different values", func(t *testing.T) {
		a := &corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
		b := &corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"), // Different CPU
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}

		assert.False(t, equalResourceList(a, b))
	})

	t.Run("unequal resource lists - different keys", func(t *testing.T) {
		a := &corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
		b := &corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("1"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"), // Different resource type
		}

		assert.False(t, equalResourceList(a, b))
	})

	t.Run("unequal resource lists - different lengths", func(t *testing.T) {
		a := &corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
		b := &corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("1"),
			corev1.ResourceMemory:           resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"), // Extra resource
		}

		assert.False(t, equalResourceList(a, b))
	})

	t.Run("empty resource lists", func(t *testing.T) {
		a := &corev1.ResourceList{}
		b := &corev1.ResourceList{}

		assert.True(t, equalResourceList(a, b))
	})

	t.Run("one empty resource list", func(t *testing.T) {
		a := &corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"),
		}
		b := &corev1.ResourceList{}

		assert.False(t, equalResourceList(a, b))
	})

	t.Run("both nil resource lists", func(t *testing.T) {
		var a *corev1.ResourceList = nil
		var b *corev1.ResourceList = nil

		assert.True(t, equalResourceList(a, b))
	})

	t.Run("one nil resource list", func(t *testing.T) {
		var a *corev1.ResourceList = nil
		b := &corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1"),
		}

		assert.False(t, equalResourceList(a, b))
	})

	t.Run("one nil, one empty resource list", func(t *testing.T) {
		var a *corev1.ResourceList = nil
		b := &corev1.ResourceList{}

		assert.False(t, equalResourceList(a, b))
	})
}

func TestEquslSubGroupNetworkTopology(t *testing.T) {
	// test case 1: both parameters are nil or empty
	t.Run("both nil or empty", func(t *testing.T) {
		assert.True(t, equalSubGroupNetworkTopology(nil, nil))
		assert.True(t, equalSubGroupNetworkTopology([]schedulingv1beta1.SubGroupPolicySpec{}, nil))
	})

	// test case 2: one parameter is nil or empty, the other is not
	t.Run("one nil or empty, other not", func(t *testing.T) {
		highestTierAllowed := 1
		subGroupPolicy := &schedulingv1beta1.NetworkTopologySpec{
			Mode:               "hard",
			HighestTierAllowed: &highestTierAllowed,
		}
		assert.False(t, equalSubGroupNetworkTopology(nil, subGroupPolicy))
		assert.False(t, equalSubGroupNetworkTopology([]schedulingv1beta1.SubGroupPolicySpec{}, subGroupPolicy))
	})

	// test case 3: MatchPolicy is nil
	t.Run("match policy is nil", func(t *testing.T) {
		highestTierAllowed := 1
		subGroupPolicy := []schedulingv1beta1.SubGroupPolicySpec{
			{
				NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
					Mode:               "hard",
					HighestTierAllowed: &highestTierAllowed,
				},
				MatchPolicy: nil,
			},
		}
		networkTopology := &schedulingv1beta1.NetworkTopologySpec{
			Mode:               "hard",
			HighestTierAllowed: &highestTierAllowed,
		}
		assert.False(t, equalSubGroupNetworkTopology(subGroupPolicy, networkTopology))
	})

	// test case 4: MatchPolicy labels mismatch
	t.Run("match policy labels mismatch", func(t *testing.T) {
		highestTierAllowed := 1
		subGroupPolicy := []schedulingv1beta1.SubGroupPolicySpec{
			{
				NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
					Mode:               "hard",
					HighestTierAllowed: &highestTierAllowed,
				},
				MatchPolicy: []schedulingv1beta1.MatchPolicySpec{
					{
						LabelKey: "wrong-label-key-1",
					},
					{
						LabelKey: "wrong-label-key-2",
					},
				},
			},
		}
		networkTopology := &schedulingv1beta1.NetworkTopologySpec{
			Mode:               "hard",
			HighestTierAllowed: &highestTierAllowed,
		}
		assert.False(t, equalSubGroupNetworkTopology(subGroupPolicy, networkTopology))
	})

	// test case 5: NetworkTopology mismatch
	t.Run("network topology mismatch", func(t *testing.T) {
		highestTierAllowed1 := 1
		highestTierAllowed2 := 2
		subGroupPolicy := []schedulingv1beta1.SubGroupPolicySpec{
			{
				NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
					Mode:               "soft",
					HighestTierAllowed: &highestTierAllowed2,
				},
				MatchPolicy: []schedulingv1beta1.MatchPolicySpec{
					{
						LabelKey: workloadv1alpha1.RoleLabelKey,
					},
					{
						LabelKey: workloadv1alpha1.RoleIDKey,
					},
				},
			},
		}
		networkTopology := &schedulingv1beta1.NetworkTopologySpec{
			Mode:               "hard",
			HighestTierAllowed: &highestTierAllowed1,
		}
		assert.False(t, equalSubGroupNetworkTopology(subGroupPolicy, networkTopology))
	})

	// test case 6: complete match
	t.Run("complete match", func(t *testing.T) {
		highestTierAllowed := 1
		subGroupPolicy := []schedulingv1beta1.SubGroupPolicySpec{
			{
				NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
					Mode:               "hard",
					HighestTierAllowed: &highestTierAllowed,
				},
				MatchPolicy: []schedulingv1beta1.MatchPolicySpec{
					{
						LabelKey: workloadv1alpha1.RoleLabelKey,
					},
					{
						LabelKey: workloadv1alpha1.RoleIDKey,
					},
				},
			},
		}
		networkTopology := &schedulingv1beta1.NetworkTopologySpec{
			Mode:               "hard",
			HighestTierAllowed: &highestTierAllowed,
		}
		assert.True(t, equalSubGroupNetworkTopology(subGroupPolicy, networkTopology))
	})
}
