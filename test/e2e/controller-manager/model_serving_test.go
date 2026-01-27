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

// TestModelServingLifecycle tests the complete lifecycle of a ModelServing:
// Create, Update, and Delete operations.
func TestModelServingLifecycle(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client for resource verification
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// === CREATE TEST ===
	t.Log("=== Testing CREATE operation ===")

	// Create a basic ModelServing with 1 servingGroup replica and 1 role replica
	modelServing := createBasicModelServing("test-lifecycle", 1, 1)

	t.Log("Creating ModelServing")
	createdMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Wait for ModelServing to be ready
	t.Log("Waiting for ModelServing to become ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, createdMS.Name)

	// Verify ModelServing is ready with correct spec
	readyMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, createdMS.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing after creation")
	assert.Equal(t, int32(1), *readyMS.Spec.Replicas, "ModelServing should have 1 servingGroup replica")
	assert.Equal(t, int32(1), readyMS.Status.AvailableReplicas, "ModelServing should have 1 available replica")
	t.Log("CREATE test passed: ModelServing is ready")

	// === UPDATE TEST ===
	t.Log("=== Testing UPDATE operation ===")

	// Update the role replicas from 1 to 2
	updateMS := readyMS.DeepCopy()
	newRoleReplicas := int32(2)
	updateMS.Spec.Template.Roles[0].Replicas = &newRoleReplicas

	t.Log("Updating ModelServing role replicas from 1 to 2")
	updatedMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updateMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing")

	// Verify the spec was updated
	assert.Equal(t, int32(2), *updatedMS.Spec.Template.Roles[0].Replicas, "Role replicas should be updated to 2")

	// Wait for the update to be applied
	t.Log("Waiting for updated ModelServing to become ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, updatedMS.Name)

	// Verify pods are created for the updated role replicas
	labelSelector := "modelserving.volcano.sh/name=" + updatedMS.Name
	require.Eventually(t, func() bool {
		podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		// Should have 2 pods now (1 servingGroup * 2 role replicas)
		return len(podList.Items) >= 2
	}, 2*time.Minute, 5*time.Second, "Expected 2 pods after update")

	t.Log("UPDATE test passed: Role replicas updated successfully")

	// === DELETE TEST ===
	t.Log("=== Testing DELETE operation ===")

	// Delete the ModelServing
	t.Log("Deleting ModelServing")
	err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, updatedMS.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServing")

	// Verify ModelServing is deleted
	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, updatedMS.Name, metav1.GetOptions{})
		return err != nil // Should return NotFound error
	}, 2*time.Minute, 5*time.Second, "ModelServing should be deleted")

	// Verify all associated pods are cleaned up
	require.Eventually(t, func() bool {
		podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		return len(podList.Items) == 0
	}, 2*time.Minute, 5*time.Second, "All pods should be cleaned up after deletion")

	// Verify all associated services are cleaned up
	require.Eventually(t, func() bool {
		svcList, err := kubeClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		return len(svcList.Items) == 0
	}, 2*time.Minute, 5*time.Second, "All services should be cleaned up after deletion")

	t.Log("DELETE test passed: ModelServing and all resources cleaned up")
	t.Log("ModelServing lifecycle test completed successfully")
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
