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

package controller_manager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// TODO(user): Add E2E tests for ModelServing lifecycle (Create, Update, Delete and check if it works)
func TestModelServingLifecycle(t *testing.T) {
	// Placeholder for ModelServing lifecycle tests
	t.Skip("ModelServing lifecycle test is not implemented yet")
}

// TestModelServingScaleUp tests the ability to scale up a ModelServing's ServingGroup
func TestModelServingScaleUp(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create a basic ModelServing with 1 replica
	modelServing := createBasicModelServing("test-scale-up", 1, 1)

	// Create the ModelServing
	t.Log("Creating ModelServing with 1 servingGHroup replica")
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Wait for the initial ModelServing to be ready
	t.Log("Waiting for initial ModelServing (1 servingGroup replica) to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify initial state - should have 1 replica
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(1), *initialMS.Spec.Replicas, "Initial ModelServing should have 1 replica")
	assert.Equal(t, int32(1), initialMS.Status.AvailableReplicas, "Initial ModelServing should have 1 available replica")

	// Update the ModelServing to scale up to 3 replicas
	scaleUpMS := initialMS.DeepCopy()
	newReplicas := int32(3)
	scaleUpMS.Spec.Replicas = &newReplicas

	t.Log("Updating ModelServing to scale up to 3 replicas")
	updatedMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, scaleUpMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for scale up")

	// Verify the spec was updated
	assert.Equal(t, int32(3), *updatedMS.Spec.Replicas, "Updated ModelServing should have 3 replicas in spec")

	// Wait for the scaled-up ModelServing to be ready
	t.Log("Waiting for scaled-up ModelServing (3 replicas) to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, updatedMS.Name)

	// Final verification - should have 3 replicas
	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, updatedMS.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get final ModelServing")
	assert.Equal(t, int32(3), *finalMS.Spec.Replicas, "Final ModelServing should have 3 replicas in spec")
	assert.Equal(t, int32(3), finalMS.Status.AvailableReplicas, "Final ModelServing should have 3 available replicas")

	t.Log("ModelServing scale up test passed successfully")
}

// TestModelServingPodRecovery verifies that when a pod is deleted,
// the corresponding role can recreate the pod successfully.
func TestModelServingPodRecovery(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client locally (do NOT modify test suite)
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a basic ModelServing
	modelServing := createBasicModelServing("test-pod-recovery", 1, 1)

	t.Log("Creating ModelServing for pod recovery test")
	_, err = kthenaClient.WorkloadV1alpha1().
		ModelServings(testNamespace).
		Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Register cleanup for ModelServing
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", modelServing.Namespace, modelServing.Name)
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", modelServing.Namespace, modelServing.Name, err)
		}
	})

	// Wait until ModelServing is ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// List pods using label selector scoped to the current ModelServing instance
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods with label selector")

	// Set original pod to first item since list already uses label selector
	var originalPod *corev1.Pod
	if len(podList.Items) > 0 {
		originalPod = &podList.Items[0]
	}

	// If no pod with the label is found, skip the test
	if originalPod == nil {
		t.Logf("No pod found with label selector %q, skipping pod recovery test", labelSelector)
		t.Skip()
	}

	originalPodUID := originalPod.UID
	originalPodName := originalPod.Name
	t.Logf("Deleting pod %s (UID: %s)", originalPodName, originalPodUID)

	// Delete the pod
	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, originalPodName, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete pod")

	// Wait for a new pod with different UID and PodReady condition set to True
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		for _, pod := range pods.Items {
			// Check if it's a new pod (different UID from original)
			if pod.UID != originalPodUID {
				// Check for PodReady condition with status True
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						t.Logf("New pod created and ready: %s (UID: %s)", pod.Name, pod.UID)
						return true
					}
				}
			}
		}
		return false
	}, 2*time.Minute, 5*time.Second, "New pod was not recreated with PodReady condition after deletion")

	t.Log("Pod recovery test passed successfully")
}

// TestModelServingServiceRecovery verifies that when the headless Service
// is deleted, it can be recreated successfully and ModelServing remains healthy.
func TestModelServingServiceRecovery(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client locally
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a basic ModelServing
	modelServing := createBasicModelServing("test-service-recovery", 1, 1)

	t.Log("Creating ModelServing for service recovery test")
	_, err = kthenaClient.WorkloadV1alpha1().
		ModelServings(testNamespace).
		Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Register cleanup for ModelServing
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", modelServing.Namespace, modelServing.Name)
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", modelServing.Namespace, modelServing.Name, err)
		}
	})

	// Wait until ModelServing is ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Get the ModelServing to obtain its UID
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	// List Services with label selector scoped to the current ModelServing
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	serviceList, err := kubeClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list Services in namespace")

	// Filter Services owned by this ModelServing and find the headless one
	var originalService *corev1.Service
	var originalServiceUID string
	for _, svc := range serviceList.Items {
		// Check if service is owned by the ModelServing
		ownedByMS := false
		for _, ref := range svc.OwnerReferences {
			if ref.UID == ms.UID {
				ownedByMS = true
				break
			}
		}
		// Select if it's owned by the ModelServing and is headless
		if ownedByMS && svc.Spec.ClusterIP == corev1.ClusterIPNone {
			originalService = &svc
			originalServiceUID = string(svc.UID)
			break
		}
	}

	// If no headless Service owned by the ModelServing exists, gracefully skip the test
	if originalService == nil {
		t.Log("No headless Service owned by ModelServing found, skipping service recovery test")
		t.Skip()
	}

	t.Logf("Deleting headless Service %s (UID: %s)", originalService.Name, originalServiceUID)

	// Delete the Service
	err = kubeClient.CoreV1().Services(testNamespace).Delete(ctx, originalService.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete headless Service")

	// Wait for a new headless Service with same owner but different UID to appear
	require.Eventually(t, func() bool {
		serviceList, err := kubeClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		for _, svc := range serviceList.Items {
			// Check if service is owned by the same ModelServing
			ownedByMS := false
			for _, ref := range svc.OwnerReferences {
				if ref.UID == ms.UID {
					ownedByMS = true
					break
				}
			}
			// Return true if it's a new service (different UID) owned by the ModelServing and is headless
			if ownedByMS && string(svc.UID) != originalServiceUID && svc.Spec.ClusterIP == corev1.ClusterIPNone {
				t.Logf("New Service created: %s (UID: %s)", svc.Name, svc.UID)
				return true
			}
		}
		return false
	}, 2*time.Minute, 5*time.Second, "Headless Service owned by ModelServing was not recreated after deletion")

	// Verify ModelServing is still ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	t.Log("ModelServing service recovery test passed")
}

// TestModelServingWithDuplicateHostAliases verifies that ModelServing with duplicate IP hostAliases
// can be created and pods are running successfully
func TestModelServingWithDuplicateHostAliases(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client locally
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a ModelServing with duplicate IP hostAliases
	modelServing := createBasicModelServing("test-duplicate-hostaliases", 1, 1)
	modelServing.Spec.Template.Roles[0].EntryTemplate.Spec.HostAliases = []corev1.HostAlias{
		{
			IP:        "10.1.2.3",
			Hostnames: []string{"test.com", "example.com"},
		},
		{
			IP:        "10.1.2.3",
			Hostnames: []string{"test.org"},
		},
	}

	t.Log("Creating ModelServing with duplicate IP hostAliases")
	_, err = kthenaClient.WorkloadV1alpha1().
		ModelServings(testNamespace).
		Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing with duplicate hostAliases")

	// Register cleanup for ModelServing
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", modelServing.Namespace, modelServing.Name)
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", modelServing.Namespace, modelServing.Name, err)
		}
	})

	// Wait until ModelServing is ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify that pods are created and running with the correct hostAliases
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	require.Eventually(t, func() bool {
		podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}

		// Check that we have at least one pod and it has the expected hostAliases
		for _, pod := range podList.Items {
			// Check if pod is running
			if pod.Status.Phase == corev1.PodRunning {
				// Verify that hostAliases contains entries with duplicate IPs
				hostAliases := pod.Spec.HostAliases
				hasDuplicateIP := false

				ipCount := make(map[string]int)
				for _, alias := range hostAliases {
					ipCount[alias.IP]++
					if ipCount[alias.IP] > 1 {
						hasDuplicateIP = true
						break
					}
				}

				// Also check if we have the expected hostnames
				expectedHostnames := map[string]bool{
					"test.com":    true,
					"example.com": true,
					"test.org":    true,
				}

				foundHostnames := make(map[string]bool)
				for _, alias := range hostAliases {
					for _, hostname := range alias.Hostnames {
						foundHostnames[hostname] = true
					}
				}

				allExpectedFound := true
				for expected := range expectedHostnames {
					if !foundHostnames[expected] {
						allExpectedFound = false
						break
					}
				}

				if hasDuplicateIP && allExpectedFound {
					return true
				}
			}
		}
		return false
	}, 2*time.Minute, 5*time.Second, "Pods were not created with duplicate IP hostAliases or did not reach running state")

	t.Log("ModelServing with duplicate IP hostAliases test passed successfully")
}

func TestModelServingRecoveryPolicyServingGroupRecreate(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client locally
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a ModelServing with 2 ServingGroup replicas and RecoveryPolicy set to ServingGroupRecreate
	modelServing := createBasicModelServing("test-recovery-sg-recreate", 2, 2)
	modelServing.Spec.RecoveryPolicy = workload.ServingGroupRecreate

	t.Log("Creating ModelServing with RecoveryPolicy set to ServingGroupRecreate")
	_, err = kthenaClient.WorkloadV1alpha1().
		ModelServings(testNamespace).
		Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Register cleanup for ModelServing
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", modelServing.Namespace, modelServing.Name)
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", modelServing.Namespace, modelServing.Name, err)
		}
	})

	// Wait until ModelServing is ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Get initial pods to identify pods belonging to one ServingGroup
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods with label selector")

	// Group pods by their ServingGroup (using labels to identify which ServingGroup they belong to)
	servingGroupPods := make(map[string][]*corev1.Pod)
	for i := range podList.Items {
		pod := &podList.Items[i]
		// Look for the label that indicates which ServingGroup the pod belongs to
		sgLabel, exists := pod.Labels["servinggroup.volcano.sh/name"]
		if !exists {
			continue
		}
		servingGroupPods[sgLabel] = append(servingGroupPods[sgLabel], pod)
	}

	// Select a ServingGroup that has pods to delete a pod from
	var targetServingGroup string
	var targetPod *corev1.Pod
	for sg, pods := range servingGroupPods {
		if len(pods) > 0 {
			targetServingGroup = sg
			targetPod = pods[0] // Take the first pod from this ServingGroup
			break
		}
	}

	if targetPod == nil {
		t.Fatal("No pods found in any ServingGroup")
	}

	t.Logf("Selected pod %s from ServingGroup %s to delete", targetPod.Name, targetServingGroup)

	// Store original UIDs of all pods in the target ServingGroup
	originalPodUIDs := make(map[string]string)
	for _, pod := range servingGroupPods[targetServingGroup] {
		originalPodUIDs[pod.Name] = string(pod.UID)
	}
	t.Logf("Original pod UIDs in ServingGroup %s: %v", targetServingGroup, originalPodUIDs)

	// Record original number of pods in the target ServingGroup
	originalPodCount := len(servingGroupPods[targetServingGroup])

	// Delete the selected pod
	t.Logf("Deleting pod %s from ServingGroup %s", targetPod.Name, targetServingGroup)
	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, targetPod.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete pod")

	// Wait for all pods in the target ServingGroup to be recreated
	// We expect all original pods to be replaced with new ones
	t.Log("Waiting for all pods in the target ServingGroup to be recreated due to ServingGroupRecreate policy...")

	require.Eventually(t, func() bool {
		// Get current pods
		currentPodList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}

		// Group current pods by ServingGroup
		currentServingGroupPods := make(map[string][]*corev1.Pod)
		for i := range currentPodList.Items {
			pod := &currentPodList.Items[i]
			sgLabel, exists := pod.Labels["servinggroup.volcano.sh/name"]
			if !exists {
				continue
			}
			currentServingGroupPods[sgLabel] = append(currentServingGroupPods[sgLabel], pod)
		}

		// Check if all pods in the target ServingGroup have been replaced (different UIDs)
		currentTargetPods := currentServingGroupPods[targetServingGroup]
		if len(currentTargetPods) != originalPodCount {
			// Still waiting for all pods to be recreated
			t.Logf("Current pod count in target ServingGroup %s: %d, expecting: %d", targetServingGroup, len(currentTargetPods), originalPodCount)
			return false
		}

		// Check if all pods in the target ServingGroup are new (different UIDs from original)
		allReplaced := true
		for _, currentPod := range currentTargetPods {
			originalUID, exists := originalPodUIDs[currentPod.Name]
			if exists && originalUID == string(currentPod.UID) {
				// Found a pod with same UID as original - not all pods have been replaced yet
				t.Logf("Pod %s still has original UID %s", currentPod.Name, originalUID)
				allReplaced = false
				break
			}
		}

		if allReplaced {
			t.Logf("All pods in ServingGroup %s have been replaced with new UIDs", targetServingGroup)
		} else {
			t.Logf("Still waiting for all pods in ServingGroup %s to be replaced")
		}

		return allReplaced
	}, 3*time.Minute, 5*time.Second, "Not all pods in the target ServingGroup were recreated with new UIDs after pod deletion")

	// Verify the ModelServing eventually becomes ready again
	t.Log("Waiting for ModelServing to become ready again after ServingGroup recreation")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	t.Log("ServingGroupRecreate recovery policy test passed successfully")
}

func createBasicModelServing(name string, servingGroupReplicas, roleReplicas int32) *workload.ModelServing {
	return &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &servingGroupReplicas,
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: &roleReplicas,
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
										Ports: []corev1.ContainerPort{
											{
												Name:          "http",
												ContainerPort: 80,
											},
										},
									},
								},
							},
						},
						WorkerReplicas: 0,
					},
				},
			},
		},
	}
}

func createInvalidModelServing() *workload.ModelServing {
	negativeReplicas := int32(-1)
	return &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-modelserving",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &negativeReplicas,
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "role1",
						Replicas: &negativeReplicas,
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test",
										Image: "nginx:latest",
									},
								},
							},
						},
						WorkerReplicas: 0,
					},
				},
			},
		},
	}
}
