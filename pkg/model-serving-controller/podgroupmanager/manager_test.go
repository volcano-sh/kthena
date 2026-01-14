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

package podgroupmanager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
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
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		ms := createBasicModelServing()
		manager.hasPodGroupCRD.Store(true)
		manager.hasSubGroupPolicy.Store(true)

		// servingGroupName is used to find roleList.
		// It will not affect the calculation of minTaskMember.
		minMember, minRoleMember, minTaskMember, _ := manager.calculateRequirements(ms, "test-serving-group")

		// For 2 prefill roles (each with 1 entry + 3 workers) and 1 decode role (1 entry + 2 workers)
		// Total pods = (1+3)*2 + (1+2)*1 = 8 + 3 = 11
		assert.Equal(t, 11, minMember)

		// Check task members
		expectedRoleMembers := map[string]int32{
			"prefill": 4, // 1 entry + 3 workers
			"decode":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedRoleMembers, minRoleMember)
		expectedTaskMembers := map[string]int32{}
		assert.Equal(t, expectedTaskMembers, minTaskMember)

		manager.hasSubGroupPolicy.Store(false)
		_, _, minTaskMember, minResources := manager.calculateRequirements(ms, "test-serving-group")
		expectedTaskMembers = map[string]int32{
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
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		ms := createBasicModelServing()
		manager.hasPodGroupCRD.Store(true)
		manager.hasSubGroupPolicy.Store(true)

		// Set MinRoleReplicas to limit the number of roles considered
		minRoleReplicas := map[string]int32{
			"prefill": 1, // Only consider 1 prefill role instead of 2
			"decode":  1, // Consider all decode roles (1)
		}
		ms.Spec.Template.GangPolicy.MinRoleReplicas = minRoleReplicas

		// servingGroupName is used to find roleList.
		// It will not affect the calculation of minTaskMember.
		minMember, minRoleMember, minTaskMember, _ := manager.calculateRequirements(ms, "test-serving-group")

		// For 1 prefill role (1 entry + 3 workers) and 1 decode role (1 entry + 2 workers)
		// Total pods = (1+3)*1 + (1+2)*1 = 4 + 3 = 7
		assert.Equal(t, 7, minMember)

		// Check task members - should only include prefill-0 and decode-0
		expectedRoleMembers := map[string]int32{
			"prefill": 4, // 1 entry + 3 workers
			"decode":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedRoleMembers, minRoleMember)
		expectedTaskMembers := map[string]int32{}
		assert.Equal(t, expectedTaskMembers, minTaskMember)

		manager.hasSubGroupPolicy.Store(false)
		_, _, minTaskMember, minResources := manager.calculateRequirements(ms, "test-serving-group")
		expectedTaskMembers = map[string]int32{
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
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		ms := createBasicModelServing()
		ms.Spec.Template.GangPolicy.MinRoleReplicas = nil
		manager.hasPodGroupCRD.Store(true)
		manager.hasSubGroupPolicy.Store(true)

		// servingGroupName is used to find roleList.
		// It will not affect the calculation of minTaskMember.
		minMember, _, _, _ := manager.calculateRequirements(ms, "test-serving-group")

		// Should consider all roles without constraint
		// Same as basic calculation: 11 pods
		assert.Equal(t, 11, minMember)
	})

	t.Run("empty roles", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		ms := createBasicModelServing()
		ms.Spec.Template.Roles = []workloadv1alpha1.Role{} // Empty roles
		manager.hasPodGroupCRD.Store(true)
		manager.hasSubGroupPolicy.Store(true)

		// servingGroupName is used to find roleList.
		// It will not affect the calculation of minTaskMember.
		minMember, minRoleMember, minTaskMember, minResources := manager.calculateRequirements(ms, "test-serving-group")

		// Should have no requirements
		assert.Equal(t, 0, minMember)
		assert.Empty(t, minRoleMember)
		assert.Empty(t, minTaskMember)
		assert.Empty(t, minResources)
	})

	t.Run("role with no worker template", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		ms := createBasicModelServing()
		manager.hasPodGroupCRD.Store(true)
		manager.hasSubGroupPolicy.Store(true)

		// Modify one role to have no worker template
		ms.Spec.Template.Roles[1].WorkerTemplate = nil
		ms.Spec.Template.Roles[1].WorkerReplicas = 0
		// servingGroupName is used to find roleList.
		// It will not affect the calculation of minTaskMember.
		minMember, minRoleMember, minTaskMember, _ := manager.calculateRequirements(ms, "test-serving-group")

		// For 2 prefill roles (each with 1 entry + 3 workers) and 1 decode role (1 entry only)
		// Total pods = (1+3)*2 + (1+0)*1 = 8 + 1 = 9
		assert.Equal(t, 9, minMember)

		// Check task members
		expectedRoleMembers := map[string]int32{
			"prefill": 4, // 1 entry + 3 workers
			"decode":  1, // 1 entry only (no workers)
		}
		assert.Equal(t, expectedRoleMembers, minRoleMember)
		expectedTaskMembers := map[string]int32{}
		assert.Equal(t, expectedTaskMembers, minTaskMember)

		manager.hasSubGroupPolicy.Store(false)
		_, _, minTaskMember, _ = manager.calculateRequirements(ms, "test-serving-group")
		expectedTaskMembers = map[string]int32{
			"prefill-0": 4, // 1 entry + 3 workers
			"prefill-1": 4, // 1 entry + 3 workers
			"decode-0":  1, // 1 entry only (no workers)
		}
		assert.Equal(t, expectedTaskMembers, minTaskMember)
	})

	t.Run("zero worker replicas", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		ms := createBasicModelServing()
		manager.hasPodGroupCRD.Store(true)
		manager.hasSubGroupPolicy.Store(true)

		// Set worker replicas to zero for one role
		ms.Spec.Template.Roles[0].WorkerReplicas = 0

		// servingGroupName is used to find roleList.
		// It will not affect the calculation of minTaskMember.
		minMember, minRoleMember, minTaskMember, _ := manager.calculateRequirements(ms, "test-serving-group")

		// For 2 prefill roles (each with 1 entry + 0 workers) and 1 decode role (1 entry + 2 workers)
		// Total pods = (1+0)*2 + (1+2)*1 = 2 + 3 = 5
		assert.Equal(t, 5, minMember)

		// Check task members
		expectedRoleMembers := map[string]int32{
			"prefill": 1, // 1 entry only (no workers)
			"decode":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedRoleMembers, minRoleMember)
		expectedTaskMembers := map[string]int32{}
		assert.Equal(t, expectedTaskMembers, minTaskMember)

		manager.hasSubGroupPolicy.Store(false)
		_, _, minTaskMember, _ = manager.calculateRequirements(ms, "test-serving-group")
		expectedTaskMembers = map[string]int32{
			"prefill-0": 1, // 1 entry only (no workers)
			"prefill-1": 1, // 1 entry only (no workers)
			"decode-0":  3, // 1 entry + 2 workers
		}
		assert.Equal(t, expectedTaskMembers, minTaskMember)
	})
}

func TestAggregateResources(t *testing.T) {
	t.Run("basic aggregation", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
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

		total = manager.aggregateResources(total, podSpec, 1)

		expectedCPU := resource.MustParse("3")
		expectedMemory := resource.MustParse("3Gi")

		assert.True(t, expectedCPU.Equal(total[corev1.ResourceCPU]))
		assert.True(t, expectedMemory.Equal(total[corev1.ResourceMemory]))
	})

	t.Run("nil total resource list", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
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

		total = manager.aggregateResources(total, podSpec, 1)

		assert.NotNil(t, total)
		assert.Len(t, total, 1)
		assert.True(t, resource.MustParse("1").Equal(total[corev1.ResourceCPU]))
	})

	t.Run("empty containers", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{}, // Empty containers
		}

		total = manager.aggregateResources(total, podSpec, 1)

		assert.Empty(t, total)
	})

	t.Run("nil containers", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: nil, // Nil containers
		}

		total = manager.aggregateResources(total, podSpec, 1)

		assert.Empty(t, total)
	})

	t.Run("container with no resources", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
		total := corev1.ResourceList{}

		podSpec := &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "container1",
					// No Resources field
				},
			},
		}

		total = manager.aggregateResources(total, podSpec, 1)

		assert.Empty(t, total)
	})

	t.Run("container with empty resources", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
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

		total = manager.aggregateResources(total, podSpec, 1)

		assert.Empty(t, total)
	})

	t.Run("multiple calls to aggregate resources", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
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
		total = manager.aggregateResources(total, podSpec1, 1)
		assert.True(t, resource.MustParse("1").Equal(total[corev1.ResourceCPU]))

		// Second call
		total = manager.aggregateResources(total, podSpec2, 1)
		assert.True(t, resource.MustParse("3").Equal(total[corev1.ResourceCPU]))
	})

	t.Run("different resource types", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
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

		total = manager.aggregateResources(total, podSpec, 1)

		assert.Len(t, total, 3)
		assert.True(t, resource.MustParse("1").Equal(total[corev1.ResourceCPU]))
		assert.True(t, resource.MustParse("2Gi").Equal(total[corev1.ResourceMemory]))
		assert.True(t, resource.MustParse("10Gi").Equal(total[corev1.ResourceEphemeralStorage]))
	})

	t.Run("existing resources get updated", func(t *testing.T) {
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, nil, apiextfake, store)
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

		total = manager.aggregateResources(total, podSpec, 1)

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
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, fakeVolcanoClient, apiextfake, store)

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
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, fakeVolcanoClient, apiextfake, store)

		result, err := manager.getExistingPodGroups(context.Background(), modelServing)

		// Assertions
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 0) // Should be empty
	})

	t.Run("empty pod group list", func(t *testing.T) {
		// Create fake volcano client with no pod groups
		fakeVolcanoClient := volcanofake.NewSimpleClientset()
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, fakeVolcanoClient, apiextfake, store)

		result, err := manager.getExistingPodGroups(context.Background(), modelServing)

		// Assertions
		assert.NoError(t, err)
		assert.NotNil(t, result)
		assert.Len(t, result, 0) // Should be empty
	})

	t.Run("pod group with same name in different namespace", func(t *testing.T) {
		// Create fake volcano client with pod groups
		fakeVolcanoClient := volcanofake.NewSimpleClientset(podGroup1, podGroupDifferentNamespace)
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, fakeVolcanoClient, apiextfake, store)

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
		store := datastore.New()
		apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
		manager := NewManager(nil, fakeVolcanoClient, apiextfake, store)

		// Test with nil ModelServing - this would cause a panic in the real code
		// but we're checking that our test handles it gracefully
		assert.Panics(t, func() {
			_, _ = manager.getExistingPodGroups(context.Background(), nil)
		})
	})
}

func TestHasPodGroupChanged(t *testing.T) {
	groupHighestTierAllowed := 3
	subgroupHighestTierAllowed := 2
	// Helper function to create basic PodGroup spec
	basePodGroup := func() *schedulingv1beta1.PodGroup {
		return &schedulingv1beta1.PodGroup{
			Spec: schedulingv1beta1.PodGroupSpec{
				MinMember:     2,
				MinTaskMember: map[string]int32{"task1": 1, "task2": 1},
				MinResources: &corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
				NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
					Mode:               "test-mode",
					HighestTierAllowed: &groupHighestTierAllowed,
				},
				SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
					{
						Name: "test-subgroup",
						NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
							Mode:               "sub-test-mode",
							HighestTierAllowed: &subgroupHighestTierAllowed,
						},
						MatchLabelKeys: []string{
							workloadv1alpha1.RoleLabelKey,
							workloadv1alpha1.RoleIDKey,
						},
					},
				},
			},
		}
	}

	t.Run("NoChange", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()

		result := hasPodGroupChanged(current, updated)
		assert.False(t, result, "Expected no change when objects are identical")
	})

	t.Run("MinMemberChanged", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()
		updated.Spec.MinMember = 3

		result := hasPodGroupChanged(current, updated)
		assert.True(t, result, "Expected change when MinMember differs")
	})

	t.Run("MinResourcesChanged", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()
		(*updated.Spec.MinResources)[corev1.ResourceCPU] = resource.MustParse("3")

		result := hasPodGroupChanged(current, updated)
		assert.True(t, result, "Expected change when MinResources differs")
	})

	t.Run("NetworkTopologyNilVsNotNil", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()
		updated.Spec.NetworkTopology = nil

		result := hasPodGroupChanged(current, updated)
		assert.True(t, result, "Expected change when NetworkTopology changes from nil to not nil")
	})

	t.Run("NetworkTopologyFieldChanged", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()
		updated.Spec.NetworkTopology.Mode = "different-mode"

		result := hasPodGroupChanged(current, updated)
		assert.True(t, result, "Expected change when NetworkTopology field differs")
	})

	t.Run("SubGroupPolicyLengthDiffers", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()
		updated.Spec.SubGroupPolicy = append(updated.Spec.SubGroupPolicy, schedulingv1beta1.SubGroupPolicySpec{})

		result := hasPodGroupChanged(current, updated)
		assert.True(t, result, "Expected change when SubGroupPolicy length differs")
	})

	t.Run("SubGroupPolicyContentChanged", func(t *testing.T) {
		current := basePodGroup()
		updated := basePodGroup()
		updated.Spec.SubGroupPolicy[0].Name = "different-name"

		result := hasPodGroupChanged(current, updated)
		assert.True(t, result, "Expected change when SubGroupPolicy content differs")
	})

	t.Run("AllFieldsNil", func(t *testing.T) {
		current := &schedulingv1beta1.PodGroup{
			Spec: schedulingv1beta1.PodGroupSpec{
				MinMember:       0,
				MinTaskMember:   nil,
				MinResources:    nil,
				NetworkTopology: nil,
				SubGroupPolicy:  nil,
			},
		}
		updated := &schedulingv1beta1.PodGroup{
			Spec: schedulingv1beta1.PodGroupSpec{
				MinMember:       0,
				MinTaskMember:   nil,
				MinResources:    nil,
				NetworkTopology: nil,
				SubGroupPolicy:  nil,
			},
		}

		result := hasPodGroupChanged(current, updated)
		assert.False(t, result, "Expected no change when all fields are nil/empty")
	})
}

func TestCalculateRequiredRoleNames(t *testing.T) {
	tests := []struct {
		name             string
		expectedReplicas int
		existRoleList    []datastore.Role
		roleName         string
		expectedResult   []datastore.Role
	}{
		{
			name:             "scale up from zero",
			expectedReplicas: 3,
			existRoleList:    []datastore.Role{},
			roleName:         "test-role",
			expectedResult: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 0)},
				{Name: utils.GenerateRoleID("test-role", 1)},
				{Name: utils.GenerateRoleID("test-role", 2)},
			},
		},
		{
			name:             "scale up from existing roles",
			expectedReplicas: 5,
			existRoleList: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 0)},
				{Name: utils.GenerateRoleID("test-role", 1)},
			},
			roleName: "test-role",
			expectedResult: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 0)},
				{Name: utils.GenerateRoleID("test-role", 1)},
				{Name: utils.GenerateRoleID("test-role", 2)},
				{Name: utils.GenerateRoleID("test-role", 3)},
				{Name: utils.GenerateRoleID("test-role", 4)},
			},
		},
		{
			name:             "scale up with gap in indices",
			expectedReplicas: 4,
			existRoleList: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 0)},
				{Name: utils.GenerateRoleID("test-role", 2)},
			},
			roleName: "test-role",
			expectedResult: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 0)},
				{Name: utils.GenerateRoleID("test-role", 2)},
				{Name: utils.GenerateRoleID("test-role", 3)},
				{Name: utils.GenerateRoleID("test-role", 4)},
			},
		},
		{
			name:             "scale up, exist role index is larger than expectedReplicas",
			expectedReplicas: 3,
			existRoleList: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 10)},
				{Name: utils.GenerateRoleID("test-role", 11)},
			},
			roleName: "test-role",
			expectedResult: []datastore.Role{
				{Name: utils.GenerateRoleID("test-role", 10)},
				{Name: utils.GenerateRoleID("test-role", 11)},
				{Name: utils.GenerateRoleID("test-role", 12)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateRequiredRoleNames(tt.expectedReplicas, tt.existRoleList, tt.roleName)
			assert.Equal(t, tt.expectedResult, result)
		})
	}
}

func TestAppendSubGroupPolicy(t *testing.T) {
	// Test cases for appendSubGroupPolicy function
	tests := []struct {
		name           string
		modelServing   *workloadv1alpha1.ModelServing
		podGroup       *schedulingv1beta1.PodGroup
		minRoleMember  map[string]int32
		expectedResult *schedulingv1beta1.PodGroup
	}{
		{
			name: "Basic sub group policy creation",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-model-serving",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "role1",
								Replicas: ptr.To(int32(3)),
							},
						},
						GangPolicy: &workloadv1alpha1.GangPolicy{
							MinRoleReplicas: map[string]int32{
								"role1": 2,
							},
						},
					},
				},
			},
			podGroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{},
			},
			minRoleMember: map[string]int32{
				"role1": 2,
			},
			expectedResult: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "role1",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									workloadv1alpha1.ModelServingNameLabelKey: "test-model-serving",
									workloadv1alpha1.RoleLabelKey:             "role1",
								},
							},
							MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
							SubGroupSize:   ptr.To(int32(2)),
							MinSubGroups:   ptr.To(int32(2)),
						},
					},
				},
			},
		},
		{
			name: "Multiple roles with network topology",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-model-serving",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "role1",
								Replicas: ptr.To(int32(3)),
							},
							{
								Name:     "role2",
								Replicas: ptr.To(int32(2)),
							},
						},
						GangPolicy: &workloadv1alpha1.GangPolicy{
							MinRoleReplicas: map[string]int32{
								"role1": 2,
								"role2": 1,
							},
						},
						NetworkTopology: &workloadv1alpha1.NetworkTopology{
							RolePolicy: &schedulingv1beta1.NetworkTopologySpec{
								Mode:               "soft",
								HighestTierAllowed: ptr.To(2),
							},
						},
					},
				},
			},
			podGroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{},
			},
			minRoleMember: map[string]int32{
				"role1": 2,
				"role2": 1,
			},
			expectedResult: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "role1",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									workloadv1alpha1.ModelServingNameLabelKey: "test-model-serving",
									workloadv1alpha1.RoleLabelKey:             "role1",
								},
							},
							MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
							SubGroupSize:   ptr.To(int32(2)),
							MinSubGroups:   ptr.To(int32(2)),
							NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
								Mode:               "soft",
								HighestTierAllowed: ptr.To(2),
							},
						},
						{
							Name: "role2",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									workloadv1alpha1.ModelServingNameLabelKey: "test-model-serving",
									workloadv1alpha1.RoleLabelKey:             "role2",
								},
							},
							MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
							SubGroupSize:   ptr.To(int32(1)),
							MinSubGroups:   ptr.To(int32(1)),
							NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{
								Mode:               "soft",
								HighestTierAllowed: ptr.To(2),
							},
						},
					},
				},
			},
		},
		{
			name: "No network topology",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-model-serving",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "role1",
								Replicas: ptr.To(int32(3)),
							},
						},
						GangPolicy: &workloadv1alpha1.GangPolicy{
							MinRoleReplicas: map[string]int32{
								"role1": 2,
							},
						},
					},
				},
			},
			podGroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{},
			},
			minRoleMember: map[string]int32{
				"role1": 2,
			},
			expectedResult: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "role1",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									workloadv1alpha1.ModelServingNameLabelKey: "test-model-serving",
									workloadv1alpha1.RoleLabelKey:             "role1",
								},
							},
							MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
							SubGroupSize:   ptr.To(int32(2)),
							MinSubGroups:   ptr.To(int32(2)),
						},
					},
				},
			},
		},
		{
			name: "No GangPolicy MinRoleReplicas",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-model-serving",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "role1",
								Replicas: ptr.To(int32(3)),
							},
						},
						GangPolicy: &workloadv1alpha1.GangPolicy{
							MinRoleReplicas: map[string]int32{},
						},
					},
				},
			},
			podGroup: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{},
			},
			minRoleMember: map[string]int32{
				"role1": 2,
			},
			expectedResult: &schedulingv1beta1.PodGroup{
				Spec: schedulingv1beta1.PodGroupSpec{
					SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{
						{
							Name: "role1",
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									workloadv1alpha1.ModelServingNameLabelKey: "test-model-serving",
									workloadv1alpha1.RoleLabelKey:             "role1",
								},
							},
							MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
							SubGroupSize:   ptr.To(int32(2)),
							MinSubGroups:   ptr.To(int32(3)),
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := appendSubGroupPolicy(tt.modelServing, tt.podGroup, tt.minRoleMember)

			// Compare the result with expected result
			assert.Equal(t, len(tt.expectedResult.Spec.SubGroupPolicy), len(result.Spec.SubGroupPolicy))

			for i, expectedSubGroup := range tt.expectedResult.Spec.SubGroupPolicy {
				actualSubGroup := result.Spec.SubGroupPolicy[i]

				assert.Equal(t, expectedSubGroup.Name, actualSubGroup.Name)
				assert.Equal(t, expectedSubGroup.LabelSelector, actualSubGroup.LabelSelector)
				assert.Equal(t, expectedSubGroup.MatchLabelKeys, actualSubGroup.MatchLabelKeys)
				assert.Equal(t, expectedSubGroup.SubGroupSize, actualSubGroup.SubGroupSize)
				assert.Equal(t, expectedSubGroup.MinSubGroups, actualSubGroup.MinSubGroups)
				assert.Equal(t, expectedSubGroup.NetworkTopology, actualSubGroup.NetworkTopology)
			}
		})
	}
}
