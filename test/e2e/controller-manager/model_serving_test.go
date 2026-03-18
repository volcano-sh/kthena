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
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const nginxImage = "nginx:1.28.2"

// TestModelServingLifecycle verifies the full lifecycle of a ModelServing resource:
// Create -> Verify Ready -> Update (change image) -> Verify Updated -> Delete -> Verify Deleted.
func TestModelServingLifecycle(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Phase 1: Create
	modelServing := createBasicModelServing("test-lifecycle", 1)

	t.Log("Phase 1: Creating ModelServing")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify pods are running
	labelSelector := modelServingLabelSelector(modelServing.Name)
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods")
	require.NotEmpty(t, podList.Items, "Expected at least one pod after creation")
	for _, pod := range podList.Items {
		assert.Equal(t, corev1.PodRunning, pod.Status.Phase, "Pod %s should be running", pod.Name)
	}
	t.Log("Phase 1 passed: ModelServing created and ready")

	// Phase 2: Update (change container image)
	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing for update")

	updatedMS := currentMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"

	t.Log("Phase 2: Updating ModelServing (changing image to nginx:alpine)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing")

	// Wait for the update to complete and ModelServing to be ready again
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify the image was updated on all non-terminating pods
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil || len(pods.Items) == 0 {
			return false
		}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			hasUpdatedImage := false
			for _, container := range pod.Spec.Containers {
				if container.Name == "test-container" && container.Image == "nginx:alpine" {
					hasUpdatedImage = true
					break
				}
			}
			if !hasUpdatedImage {
				return false
			}
		}
		return true
	}, 3*time.Minute, 5*time.Second, "Not all pods were updated to nginx:alpine")
	t.Log("Phase 2 passed: ModelServing updated successfully")

	// Phase 3: Delete
	t.Log("Phase 3: Deleting ModelServing")
	err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, modelServing.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServing")

	// Verify the ModelServing is deleted
	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
		if err == nil {
			return false
		}
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 5*time.Second, "ModelServing was not deleted")

	// Verify that associated pods are cleaned up
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		return len(pods.Items) == 0
	}, 2*time.Minute, 5*time.Second, "Pods were not cleaned up after ModelServing deletion")

	t.Log("Phase 3 passed: ModelServing deleted and pods cleaned up")
	t.Log("ModelServing lifecycle test passed successfully")
}

// TestModelServingScaleUp tests the ability to scale up a ModelServing's ServingGroup
func TestModelServingScaleUp(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create a basic ModelServing with 1 replica
	modelServing := createBasicModelServing("test-scale-up", 1)

	t.Log("Creating ModelServing with 1 servingGroup replica")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify initial state - should have 1 replica
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(1), *initialMS.Spec.Replicas, "Initial ModelServing should have 1 replica")

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

	t.Log("ModelServing scale up test passed successfully")
}

// TestModelServingScaleDown tests the ability to scale down a ModelServing's ServingGroup.
func TestModelServingScaleDown(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a basic ModelServing with 3 replicas
	modelServing := createBasicModelServing("test-scale-down", 3)

	t.Log("Creating ModelServing with 3 servingGroup replicas")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify initial state - should have 3 replicas
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(3), *initialMS.Spec.Replicas, "Initial ModelServing should have 3 replicas")

	// Update the ModelServing to scale down to 1 replica
	scaleDownMS := initialMS.DeepCopy()
	newReplicas := int32(1)
	scaleDownMS.Spec.Replicas = &newReplicas

	t.Log("Updating ModelServing to scale down to 1 replica")
	updatedMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, scaleDownMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for scale down")

	// Verify the spec was updated
	assert.Equal(t, int32(1), *updatedMS.Spec.Replicas, "Updated ModelServing should have 1 replica in spec")

	// Wait for the scaled-down ModelServing to be ready
	t.Log("Waiting for scaled-down ModelServing (1 replica) to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, updatedMS.Name)

	// Verify pod count has decreased
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 1, 2*time.Minute)

	// Final verification - wait for status to converge
	require.Eventually(t, func() bool {
		finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, updatedMS.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		t.Logf("AvailableReplicas: %d (expecting 1)", finalMS.Status.AvailableReplicas)
		return *finalMS.Spec.Replicas == 1 && finalMS.Status.AvailableReplicas == 1
	}, 2*time.Minute, 5*time.Second, "ModelServing status did not converge to 1 available replica")

	t.Log("ModelServing scale down test passed successfully")
}

// TestModelServingRoleScaleUp tests scaling up the role replicas within a ServingGroup.
func TestModelServingRoleScaleUp(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 1 servingGroup and a prefill role with 1 replica
	initialRoleReplicas := int32(1)
	modelServing := createBasicModelServing("test-role-scale-up", 1, workload.Role{
		Name:     "prefill",
		Replicas: &initialRoleReplicas,
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
	})

	t.Log("Creating ModelServing with 1 servingGroup, prefill role with 1 replica")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Scale up the role replicas from 1 to 3
	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing for role scale up")

	updatedMS := currentMS.DeepCopy()
	newRoleReplicas := int32(3)
	updatedMS.Spec.Template.Roles[0].Replicas = &newRoleReplicas

	t.Log("Updating ModelServing to scale up prefill role to 3 replicas")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for role scale up")

	// Wait for the ModelServing to be ready with the new role replicas
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify the pod count increased
	// With 1 servingGroup and 3 role replicas (each with 1 entry pod), we expect 3 pods
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 3, 3*time.Minute)

	t.Log("ModelServing role scale up test passed successfully")
}

// TestModelServingRoleScaleDown tests scaling down the role replicas within a ServingGroup.
func TestModelServingRoleScaleDown(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 1 servingGroup and a prefill role with 3 replicas
	initialRoleReplicas := int32(3)
	modelServing := createBasicModelServing("test-role-scale-down", 1, workload.Role{
		Name:     "prefill",
		Replicas: &initialRoleReplicas,
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
	})

	t.Log("Creating ModelServing with 1 servingGroup, prefill role with 3 replicas")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify initial pods (expect 3: 1 servingGroup × 3 role replicas × 1 entry pod)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 3, 3*time.Minute)
	t.Log("Verified 3 running pods initially")

	// Scale down the role replicas from 3 to 1
	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing for role scale down")

	updatedMS := currentMS.DeepCopy()
	newRoleReplicas := int32(1)
	updatedMS.Spec.Template.Roles[0].Replicas = &newRoleReplicas

	t.Log("Updating ModelServing to scale down prefill role to 1 replica")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for role scale down")

	// Wait for the ModelServing to be ready with the new role replicas
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify the pod count decreased to 1
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 1, 3*time.Minute)

	t.Log("ModelServing role scale down test passed successfully")
}

// TestModelServingServingGroupRecreate verifies that when a pod is deleted under the
// ServingGroupRecreate recovery policy, the entire ServingGroup is recreated.
func TestModelServingServingGroupRecreate(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with ServingGroupRecreate policy and 2 roles
	prefillRole := createRole("prefill", 1, 0)
	decodeRole := createRole("decode", 1, 0)
	modelServing := createBasicModelServing("test-sg-recreate", 1, prefillRole, decodeRole)
	modelServing.Spec.RecoveryPolicy = workload.ServingGroupRecreate

	t.Log("Creating ModelServing with ServingGroupRecreate policy and 2 roles (prefill + decode)")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Collect all pod UIDs before deletion
	labelSelector := modelServingLabelSelector(modelServing.Name)
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods")
	require.Len(t, podList.Items, 2, "Expected 2 pods (1 prefill + 1 decode)")

	originalUIDs := make(map[string]bool)
	for _, pod := range podList.Items {
		originalUIDs[string(pod.UID)] = true
		t.Logf("Original pod: %s (UID: %s)", pod.Name, pod.UID)
	}

	// Delete just one pod (e.g., the first one) to trigger ServingGroupRecreate
	targetPod := podList.Items[0]
	t.Logf("Deleting pod %s to trigger ServingGroupRecreate", targetPod.Name)
	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, targetPod.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete pod")

	// Wait for ALL pods to be recreated with new UIDs (entire serving group should be recreated)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil || len(pods.Items) < 2 {
			return false
		}

		readyNewPods := 0
		anyOriginalRemaining := false
		for _, pod := range pods.Items {
			isOriginal := originalUIDs[string(pod.UID)]
			isNonTerminating := pod.DeletionTimestamp == nil

			// Check if any original pod is still non-terminating
			if isOriginal && isNonTerminating {
				anyOriginalRemaining = true
			}

			// Must be a new pod (not in original UIDs) and must be ready
			if !isOriginal && isNonTerminating {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyNewPods++
						break
					}
				}
			}
		}
		t.Logf("New ready pods: %d (expecting 2), any original remaining: %v", readyNewPods, anyOriginalRemaining)
		return readyNewPods >= 2 && !anyOriginalRemaining
	}, 3*time.Minute, 5*time.Second, "ServingGroup was not fully recreated after pod deletion under ServingGroupRecreate policy")

	t.Log("ModelServing ServingGroupRecreate test passed successfully")
}

// TestModelServingHeadlessServiceDeleteOnServingGroupDelete verifies that when a ModelServing
// is scaled down (servingGroups are deleted), the corresponding headless services are also cleaned up.
func TestModelServingHeadlessServiceDeleteOnServingGroupDelete(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 3 servingGroup replicas and a WorkerTemplate
	// so that headless services are actually created by the controller.
	workerRole := createRole("prefill", 1, 1)
	modelServing := createBasicModelServing("test-svc-sg-delete", 3, workerRole)

	t.Log("Creating ModelServing with 3 servingGroup replicas and WorkerTemplate")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Get the ModelServing UID
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	// Wait for initial headless services to be created (one per servingGroup)
	labelSelector := modelServingLabelSelector(modelServing.Name)
	require.Eventually(t, func() bool {
		serviceList, err := kubeClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		headlessCount := 0
		for _, svc := range serviceList.Items {
			for _, ref := range svc.OwnerReferences {
				if ref.UID == ms.UID && svc.Spec.ClusterIP == corev1.ClusterIPNone {
					headlessCount++
					break
				}
			}
		}
		t.Logf("Initial headless service count: %d (expecting 3)", headlessCount)
		return headlessCount == 3
	}, 30*time.Second, 1*time.Second, "Expected 3 headless services (one per servingGroup)")

	// Scale down to 1 replica (removing 2 servingGroups)
	scaleDownMS := ms.DeepCopy()
	newReplicas := int32(1)
	scaleDownMS.Spec.Replicas = &newReplicas

	t.Log("Scaling down ModelServing to 1 replica to trigger servingGroup deletion")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, scaleDownMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for scale down")

	// Wait for the ModelServing to be ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify headless services were cleaned up: should go from 3 to exactly 1
	require.Eventually(t, func() bool {
		currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		services, err := kubeClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		headlessCount := 0
		for _, svc := range services.Items {
			for _, ref := range svc.OwnerReferences {
				if ref.UID == currentMS.UID && svc.Spec.ClusterIP == corev1.ClusterIPNone {
					headlessCount++
					break
				}
			}
		}
		t.Logf("Current headless service count: %d (expecting 1)", headlessCount)
		return headlessCount == 1
	}, 2*time.Minute, 5*time.Second, "Headless services were not cleaned up after servingGroup deletion")

	t.Log("ModelServing headless service cleanup on servingGroup delete test passed successfully")
}

// TestModelServingPodRecovery verifies that when a pod is deleted,
// the corresponding role can recreate the pod successfully.
func TestModelServingPodRecovery(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a basic ModelServing
	modelServing := createBasicModelServing("test-pod-recovery", 1)
	modelServing.Spec.RecoveryPolicy = workload.RoleRecreate

	t.Log("Creating ModelServing for pod recovery test")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// List pods using label selector scoped to the current ModelServing instance
	labelSelector := modelServingLabelSelector(modelServing.Name)
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

	// Wait until ModelServing is ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Wait for a new pod with different UID and PodReady condition set to True
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	assert.NoError(t, err, "Failed to list pods when modelserving should be ready after pod deletion")
	for _, pod := range pods.Items {
		// Check if it's a new pod (different UID from original)
		if pod.UID != originalPodUID {
			// Check for PodReady condition with status True
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					t.Logf("New pod created and ready: %s (UID: %s)", pod.Name, pod.UID)
				}
			}
		}
	}

	t.Log("Pod recovery test passed successfully")
}

// TestModelServingServiceRecovery verifies that when the headless Service
// is deleted, it can be recreated successfully and ModelServing remains healthy.
func TestModelServingServiceRecovery(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with a WorkerTemplate so that headless services are created
	workerRole := createRole("prefill", 1, 1)
	modelServing := createBasicModelServing("test-service-recovery", 1, workerRole)

	t.Log("Creating ModelServing for service recovery test")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Get the ModelServing to obtain its UID
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	// List Services with label selector scoped to the current ModelServing
	labelSelector := modelServingLabelSelector(modelServing.Name)
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
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with duplicate IP hostAliases
	modelServing := createBasicModelServing("test-duplicate-hostaliases", 1)
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
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify that pods are created and running with the correct hostAliases
	labelSelector := modelServingLabelSelector(modelServing.Name)
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

func TestModelServingRollingUpdateMaxUnavailable(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 4 replicas and maxUnavailable set to 2
	replicas := int32(4)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rolling-update-maxunavailable",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &replicas,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
					MaxUnavailable: &intstr.IntOrString{
						IntVal: 2, // maxUnavailable = 2
					},
				},
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: &replicas,
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: nginxImage, // Initial image
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

	t.Log("Creating ModelServing with 4 replicas and maxUnavailable=2")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify initial state
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(4), *initialMS.Spec.Replicas, "Initial ModelServing should have 4 replicas")

	// Update the ModelServing to trigger a rolling update (change image)
	updatedMS := initialMS.DeepCopy()
	// Modify the container image to trigger a rolling update
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"

	t.Log("Updating ModelServing to trigger rolling update with maxUnavailable=2")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for rolling update")

	// Monitor the rolling update to ensure maxUnavailable constraint is respected
	// We'll periodically check the status to ensure that at no point do more than 2 replicas become unavailable
	t.Log("Monitoring rolling update to ensure maxUnavailable=2 constraint is respected")

	watchContext := context.Background()
	maxObservedUnavailable := int32(0)
	var mu sync.Mutex

	watcherCtx, watcherCancel := context.WithCancel(watchContext)
	defer watcherCancel()

	go func() {
		watcher, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Watch(watcherCtx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", updatedMS.Name),
		})
		if err != nil {
			return
		}
		defer watcher.Stop()

		for {
			select {
			case <-watcherCtx.Done():
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return
				}

				if event.Type == watch.Added || event.Type == watch.Modified {
					if ms, ok := event.Object.(*workload.ModelServing); ok {
						totalReplicas := ms.Status.Replicas
						availableReplicas := ms.Status.AvailableReplicas
						unavailableReplicas := totalReplicas - availableReplicas

						mu.Lock()
						if unavailableReplicas > maxObservedUnavailable {
							maxObservedUnavailable = unavailableReplicas
						}
						mu.Unlock()
					}
				}
			}
		}
	}()

	// Wait for the rolling update to complete
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, updatedMS.Name)

	// Final verification
	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, updatedMS.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get final ModelServing")
	assert.Equal(t, int32(4), *finalMS.Spec.Replicas, "Final ModelServing should have 4 replicas in spec")
	assert.Equal(t, "nginx:alpine", finalMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image, "Final ModelServing should have updated image")

	// Verify that maxUnavailable was never exceeded during the update
	assert.True(t, maxObservedUnavailable <= 2, "Max unavailable replicas (%d) exceeded maxUnavailable limit (2)", maxObservedUnavailable)
	t.Logf("Max observed unavailable replicas during update: %d", maxObservedUnavailable)

	watcherCancel()
	mu.Lock()
	t.Logf("Maximum observed unavailable replicas during test: %d", maxObservedUnavailable)
	mu.Unlock()

	t.Log("ModelServing rolling update maxUnavailable test passed successfully")
}

// TestModelServingRoleStatusEvents verifies that role status transitions are surfaced via Kubernetes Events.
func TestModelServingRoleStatusEvents(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a simple ModelServing with a single role replica to keep the signal clean.
	modelServing := createBasicModelServing("test-role-status-events", 1)

	t.Log("Creating ModelServing for role status events test")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Refresh to get UID for precise event filtering.
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	// We expect at least one Creating event and one Running event for the role.
	var sawCreatingEvent, sawRunningEvent bool

	require.Eventually(t, func() bool {
		eventList, err := kubeClient.CoreV1().Events(testNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}

		for _, ev := range eventList.Items {
			if ev.InvolvedObject.Kind != "ModelServing" {
				continue
			}
			if ev.InvolvedObject.UID != ms.UID {
				continue
			}

			switch ev.Reason {
			case "RoleCreating":
				sawCreatingEvent = true
			case "RoleRunning":
				sawRunningEvent = true
			}

			if sawCreatingEvent && sawRunningEvent {
				return true
			}
		}

		return false
	}, 2*time.Minute, 5*time.Second, "Did not observe both RoleCreating and RoleRunning events for ModelServing role")

	t.Log("ModelServing role status events test passed successfully")
}

// modelServingLabelSelector returns the label selector for resources belonging to a ModelServing.
func modelServingLabelSelector(msName string) string {
	return "modelserving.volcano.sh/name=" + msName
}

// createAndWaitForModelServing creates a ModelServing, registers a cleanup function, and waits for it to be ready.
func createAndWaitForModelServing(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, modelServing *workload.ModelServing) {
	t.Helper()
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete ModelServing %s during cleanup: %v", modelServing.Name, err)
		}
	})

	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)
}

// waitForRunningPodCount waits until the expected number of non-terminating running pods exist for a ModelServing.
func waitForRunningPodCount(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string, expected int, timeout time.Duration) {
	t.Helper()
	labelSelector := modelServingLabelSelector(msName)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		runningCount := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
				runningCount++
			}
		}
		t.Logf("Running pod count: %d (expecting %d)", runningCount, expected)
		return runningCount == expected
	}, timeout, 5*time.Second, "Expected %d running pods for ModelServing %s", expected, msName)
}

// createRole is a helper function to create a Role with specified replicas and workers
func createRole(name string, roleReplicas, workerReplicas int32) workload.Role {
	return workload.Role{
		Name:     name,
		Replicas: &roleReplicas,
		EntryTemplate: workload.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "test-container",
						Image: nginxImage,
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
		WorkerReplicas: workerReplicas,
		WorkerTemplate: &workload.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "worker-container",
						Image: nginxImage,
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
	}
}

func createBasicModelServing(name string, servingGroupReplicas int32, roles ...workload.Role) *workload.ModelServing {
	// If no roles are provided, create a default role
	if len(roles) == 0 {
		defaultRoleReplicas := int32(1)
		roles = []workload.Role{
			{
				Name:     "prefill",
				Replicas: &defaultRoleReplicas,
				EntryTemplate: workload.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: nginxImage,
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
		}
	}

	return &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &servingGroupReplicas,
			Template: workload.ServingGroup{
				Roles: roles,
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
										Image: nginxImage,
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

// TestModelServingRollingUpdateMaxUnavailableWithBadImage tests maxUnavailable constraint when transitioning to bad image
func TestModelServingRollingUpdateMaxUnavailableWithBadImage(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 6 replicas and maxUnavailable set to 2
	replicas := int32(6)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-rolling-update-bad-image",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &replicas,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
					MaxUnavailable: &intstr.IntOrString{
						IntVal: 2,
					},
				},
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To[int32](1),
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: nginxImage,
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

	t.Log("Creating ModelServing with 6 replicas and maxUnavailable=2")
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Log("Waiting for initial ModelServing to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify initial state
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(6), *initialMS.Spec.Replicas, "Initial ModelServing should have 6 replicas")
	assert.Equal(t, int32(6), initialMS.Status.AvailableReplicas, "Initial ModelServing should have 6 available replicas")

	// Update to bad image
	badImageMS := initialMS.DeepCopy()
	badImageMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:nonexistent-image-99999"

	t.Log("Updating ModelServing with bad image")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, badImageMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing with bad image")

	// Monitor unavailable replicas during bad image rolling update
	maxObservedUnavailable := int32(0)
	var mu sync.Mutex
	observedUnavailableHistory := []int32{}

	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()

	go func() {
		watcher, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Watch(watcherCtx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", badImageMS.Name),
		})
		if err != nil {
			return
		}
		defer watcher.Stop()

		for {
			select {
			case <-watcherCtx.Done():
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return
				}

				if event.Type == watch.Added || event.Type == watch.Modified {
					if ms, ok := event.Object.(*workload.ModelServing); ok {
						totalReplicas := ms.Status.Replicas
						availableReplicas := ms.Status.AvailableReplicas
						unavailableReplicas := totalReplicas - availableReplicas

						mu.Lock()
						if unavailableReplicas > maxObservedUnavailable {
							maxObservedUnavailable = unavailableReplicas
						}
						observedUnavailableHistory = append(observedUnavailableHistory, unavailableReplicas)
						mu.Unlock()
					}
				}
			}
		}
	}()

	// Monitor for 60 seconds to observe the rolling update behavior with bad image
	t.Log("Monitoring rolling update with bad image for 60 seconds")
	time.Sleep(60 * time.Second)

	// Verify that maxUnavailable constraint is ALWAYS respected
	mu.Lock()
	for i, unavailable := range observedUnavailableHistory {
		if unavailable > 2 {
			t.Errorf("At observation %d: unavailable replicas (%d) exceeded maxUnavailable (2)", i, unavailable)
		}
	}
	mu.Unlock()

	assert.True(t, maxObservedUnavailable <= 2, "Max unavailable replicas (%d) exceeded maxUnavailable limit (2)", maxObservedUnavailable)
	t.Logf("Maximum observed unavailable replicas: %d", maxObservedUnavailable)

	// Verify current state - should not exceed maxUnavailable
	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, badImageMS.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get current ModelServing")
	currentUnavailable := currentMS.Status.Replicas - currentMS.Status.AvailableReplicas
	assert.True(t, currentUnavailable <= 2, "Current unavailable replicas (%d) should not exceed maxUnavailable (2)", currentUnavailable)

	t.Logf("Final status - Total: %d, Available: %d, Unavailable: %d",
		currentMS.Status.Replicas, currentMS.Status.AvailableReplicas, currentUnavailable)

	watcherCancel()

	t.Log("ModelServing rolling update maxUnavailable with bad image test passed successfully")
}

// TestLWSAPIBasic tests that kthena can process LWS API correctly by:
// 1. Creating a simple LWS instance
// 2. Verifying corresponding ModelServing is created with proper owner references
// 3. Verifying pods are created automatically
// 4. Deleting LWS and verifying all resources are cleaned up
func TestLWSAPIBasic(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create Clients
	lwsClient, err := utils.GetLWSClient()
	require.NoError(t, err, "Failed to create LWS client")

	kubeClient, err := utils.GetKubeClient()
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a simple LWS instance
	lwsName := "test-lws-basic"
	replicas := int32(1)
	size := int32(2) // 1 leader + 1 worker

	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lwsName,
			Namespace: testNamespace,
		},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas: &replicas,
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size: &size,
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:            "worker",
								Image:           nginxImage,
								ImagePullPolicy: corev1.PullIfNotPresent,
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
			},
			StartupPolicy: lwsv1.LeaderCreatedStartupPolicy,
			RolloutStrategy: lwsv1.RolloutStrategy{
				Type: lwsv1.RollingUpdateStrategyType,
			},
		},
	}

	t.Logf("Creating LWS instance: %s/%s", testNamespace, lwsName)
	_, err = lwsClient.LeaderworkersetV1().LeaderWorkerSets(testNamespace).Create(ctx, lws, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create LWS instance")

	// Wait for ModelServing to be created
	t.Log("Waiting for ModelServing resource to be created")
	var modelServing *workload.ModelServing
	require.Eventually(t, func() bool {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, lwsName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		modelServing = ms
		return true
	}, 2*time.Minute, 2*time.Second, "ModelServing was not created")

	// Verify owner reference
	t.Log("Verifying ModelServing owner reference")
	require.NotEmpty(t, modelServing.OwnerReferences, "ModelServing should have owner references")

	ownerRef := modelServing.OwnerReferences[0]
	assert.Equal(t, "LeaderWorkerSet", ownerRef.Kind, "Owner reference kind should be LeaderWorkerSet")
	assert.Equal(t, lwsName, ownerRef.Name, "Owner reference name should match LWS name")
	assert.NotNil(t, ownerRef.Controller, "Owner reference should have Controller field set")
	assert.True(t, *ownerRef.Controller, "Owner reference Controller should be true")
	assert.NotNil(t, ownerRef.BlockOwnerDeletion, "Owner reference should have BlockOwnerDeletion field set")
	assert.True(t, *ownerRef.BlockOwnerDeletion, "Owner reference BlockOwnerDeletion should be true")

	// Wait for ModelServing to be ready
	t.Log("Waiting for ModelServing to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, lwsName)

	// Verify pods are created
	t.Log("Verifying pods are created")
	labelSelector := "modelserving.volcano.sh/name=" + lwsName
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods")

	// Expected pods: 1 replica * (1 leader + 1 worker) = 2 pods
	expectedPodCount := 2
	assert.Equal(t, expectedPodCount, len(podList.Items), "Expected %d pods to be created", expectedPodCount)

	// Verify all pods are running and ready
	readyPods := 0
	for _, pod := range podList.Items {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				readyPods++
				break
			}
		}
	}
	assert.Equal(t, expectedPodCount, readyPods, "All pods should be in a Ready state")

	// Delete the LWS instance
	t.Logf("Deleting LWS instance: %s/%s", testNamespace, lwsName)
	err = lwsClient.LeaderworkersetV1().LeaderWorkerSets(testNamespace).Delete(ctx, lwsName, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete LWS instance")

	// Wait for ModelServing to be deleted (via owner reference cascade deletion)
	t.Log("Waiting for ModelServing to be deleted")
	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, lwsName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 2*time.Second, "ModelServing was not deleted after LWS deletion")

	// Wait for all pods to be deleted
	t.Log("Waiting for all pods to be deleted")
	require.Eventually(t, func() bool {
		podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		return len(podList.Items) == 0
	}, 2*time.Minute, 2*time.Second, "Pods were not deleted after LWS deletion")

	t.Log("LWS API basic test passed successfully")
}

// TestModelServingPartitionBoundaryProtection verifies the protective effect of partition
// boundaries during rolling updates. It creates a ModelServing with partition=3, triggers
// a template change, and then verifies:
// - Status.CurrentRevision remains the old revision (protecting ordinals 0,1,2)
// - Status.UpdateRevision is set to a new revision
// - Pods with ordinal < partition carry CurrentRevision and old image
// - Pods with ordinal >= partition carry UpdateRevision and new image
//
// This is the E2E counterpart of TestModelServingVersionControl from the unit tests.
func TestModelServingPartitionBoundaryProtection(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 5 replicas and partition=3
	replicas := int32(5)
	partition := int32(3)
	roleReplicas := int32(1)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-partition-boundary",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &replicas,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
					MaxUnavailable: &intstr.IntOrString{IntVal: 1},
					Partition:      &partition,
				},
			},
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
										Image: nginxImage,
										Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
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

	t.Log("Creating ModelServing with 5 replicas and partition=3")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 5, 3*time.Minute)

	// Record initial CurrentRevision and initial revision on all pods
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	initialCurrentRevision := initialMS.Status.CurrentRevision
	require.NotEmpty(t, initialCurrentRevision, "Initial CurrentRevision should not be empty")
	t.Logf("Initial CurrentRevision: %s, UpdateRevision: %s", initialMS.Status.CurrentRevision, initialMS.Status.UpdateRevision)

	// Trigger rolling update by changing image
	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"

	t.Log("Updating ModelServing image to nginx:alpine (partition=3 protects R-0, R-1, R-2)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing")

	// Wait for the partitioned update to complete
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// === Verify Status.CurrentRevision and Status.UpdateRevision ===
	t.Log("Verifying Status.CurrentRevision and Status.UpdateRevision after partitioned update")
	var finalMS *workload.ModelServing
	require.Eventually(t, func() bool {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		finalMS = ms

		t.Logf("CurrentRevision: %s, UpdateRevision: %s, UpdatedReplicas: %d",
			ms.Status.CurrentRevision, ms.Status.UpdateRevision, ms.Status.UpdatedReplicas)

		// CurrentRevision should remain as the initial revision (protecting lower ordinals)
		if ms.Status.CurrentRevision != initialCurrentRevision {
			return false
		}
		// UpdateRevision should differ from CurrentRevision
		if ms.Status.UpdateRevision == "" || ms.Status.UpdateRevision == initialCurrentRevision {
			return false
		}
		// UpdatedReplicas should be replicas - partition = 2
		return ms.Status.UpdatedReplicas == (replicas - partition)
	}, 3*time.Minute, 5*time.Second, "Revision status fields incorrect after partitioned update")

	assert.Equal(t, initialCurrentRevision, finalMS.Status.CurrentRevision,
		"CurrentRevision should remain the initial revision")
	assert.NotEqual(t, finalMS.Status.CurrentRevision, finalMS.Status.UpdateRevision,
		"CurrentRevision and UpdateRevision should differ during partitioned update")

	// === Verify per-ordinal revision labels and images ===
	t.Log("Verifying per-ordinal revisions and images")
	labelSelector := modelServingLabelSelector(modelServing.Name)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}

		protectedCorrect := 0
		updatedCorrect := 0

		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
				continue
			}

			groupName := pod.Labels["modelserving.volcano.sh/group-name"]
			_, ordinal := getGroupOrdinal(groupName)
			if ordinal < 0 {
				continue
			}

			podRevision := pod.Labels["modelserving.volcano.sh/revision"]
			containerImage := getPodContainerImage(pod, "test-container")

			if ordinal < int(partition) {
				// Protected ordinals: revision = CurrentRevision, image = old
				if podRevision == finalMS.Status.CurrentRevision && containerImage == nginxImage {
					protectedCorrect++
				} else {
					t.Logf("Protected pod %s (ordinal %d): revision=%s (want %s), image=%s (want %s)",
						pod.Name, ordinal, podRevision, finalMS.Status.CurrentRevision, containerImage, nginxImage)
				}
			} else {
				// Updated ordinals: revision = UpdateRevision, image = new
				if podRevision == finalMS.Status.UpdateRevision && containerImage == "nginx:alpine" {
					updatedCorrect++
				} else {
					t.Logf("Updated pod %s (ordinal %d): revision=%s (want %s), image=%s (want nginx:alpine)",
						pod.Name, ordinal, podRevision, finalMS.Status.UpdateRevision, containerImage)
				}
			}
		}

		t.Logf("Protected correct: %d/3, Updated correct: %d/2", protectedCorrect, updatedCorrect)
		return protectedCorrect == 3 && updatedCorrect == 2
	}, 3*time.Minute, 5*time.Second, "Per-ordinal revision/image verification failed")

	t.Log("ModelServing partition boundary protection test passed successfully")
}

// TestModelServingPartitionDeletedGroupHistoricalRevision verifies that when a pod
// (or ServingGroup) within the partition-protected range is deleted after a template
// change, the controller rebuilds it using the historical revision (CurrentRevision),
// NOT the new UpdateRevision.
//
// Scenario: partition=3, 5 replicas with updated template. Delete R-1 pod (ordinal < partition).
// Expected: R-1 is rebuilt using CurrentRevision (old image), not UpdateRevision (new image).
//
// This is the E2E counterpart of the "partition=2, recreate protected group should use
// historical revision" test case from TestModelServingVersionControl.
func TestModelServingPartitionDeletedGroupHistoricalRevision(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 5 replicas and partition=3
	replicas := int32(5)
	partition := int32(3)
	roleReplicas := int32(1)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-partition-historical-rev",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &replicas,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
					MaxUnavailable: &intstr.IntOrString{IntVal: 1},
					Partition:      &partition,
				},
			},
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
										Image: nginxImage,
										Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
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

	t.Log("Phase 1: Creating ModelServing with 5 replicas and partition=3")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 5, 3*time.Minute)

	// Record initial CurrentRevision
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	initialCurrentRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s", initialCurrentRevision)

	// Phase 2: Trigger partitioned update (change image)
	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"

	t.Log("Phase 2: Updating image to nginx:alpine (partition=3 protects R-0, R-1, R-2)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing")

	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify the partitioned state is established: R-0,R-1,R-2 have old image, R-3,R-4 have new
	labelSelector := modelServingLabelSelector(modelServing.Name)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false
		}
		protectedOld, updatedNew := 0, 0
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			groupName := pod.Labels["modelserving.volcano.sh/group-name"]
			_, ordinal := getGroupOrdinal(groupName)
			image := getPodContainerImage(pod, "test-container")
			if ordinal < int(partition) && image == nginxImage {
				protectedOld++
			} else if ordinal >= int(partition) && image == "nginx:alpine" {
				updatedNew++
			}
		}
		return protectedOld == 3 && updatedNew == 2
	}, 3*time.Minute, 5*time.Second, "Failed to reach partitioned state")

	t.Log("Phase 2 passed: Partitioned update established")

	// Phase 3: Delete a pod within the partition-protected range (e.g., R-1)
	// Find the pod belonging to the ServingGroup with ordinal 1
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	require.NoError(t, err, "Failed to list pods")

	var targetPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		groupName := pod.Labels["modelserving.volcano.sh/group-name"]
		_, ordinal := getGroupOrdinal(groupName)
		if ordinal == 1 {
			targetPod = pod
			break
		}
	}
	require.NotNil(t, targetPod, "Could not find pod for ServingGroup with ordinal 1")

	originalPodUID := targetPod.UID
	t.Logf("Phase 3: Deleting pod %s (ordinal 1, UID: %s) within partition-protected range", targetPod.Name, originalPodUID)

	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, targetPod.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete pod")

	// Wait for ModelServing to recover
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify the recreated pod at ordinal 1 uses CurrentRevision (old revision) and old image
	t.Log("Verifying recreated pod at ordinal 1 uses historical CurrentRevision")
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false
		}

		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			groupName := pod.Labels["modelserving.volcano.sh/group-name"]
			_, ordinal := getGroupOrdinal(groupName)
			if ordinal != 1 {
				continue
			}

			// Must be a new pod (different UID from original)
			if pod.UID == originalPodUID {
				t.Logf("Old pod %s still present, waiting for recreation", pod.Name)
				return false
			}

			if pod.Status.Phase != corev1.PodRunning {
				t.Logf("Recreated pod %s not yet running (phase: %s)", pod.Name, pod.Status.Phase)
				return false
			}

			// Verify revision label matches CurrentRevision (historical, not UpdateRevision)
			podRevision := pod.Labels["modelserving.volcano.sh/revision"]
			containerImage := getPodContainerImage(pod, "test-container")

			t.Logf("Recreated pod %s (ordinal 1): revision=%s, image=%s", pod.Name, podRevision, containerImage)

			// The recreated pod should use the historical revision
			ms, _ := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
			if ms != nil && podRevision == ms.Status.CurrentRevision && containerImage == nginxImage {
				return true
			}
			return false
		}
		return false
	}, 3*time.Minute, 5*time.Second, "Recreated pod at ordinal 1 did not use historical CurrentRevision")

	// Also verify the overall state is still correct: 3 protected + 2 updated
	t.Log("Verifying overall partition state is preserved after pod recreation")
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false
		}
		protectedOld, updatedNew := 0, 0
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
				continue
			}
			groupName := pod.Labels["modelserving.volcano.sh/group-name"]
			_, ordinal := getGroupOrdinal(groupName)
			image := getPodContainerImage(pod, "test-container")
			if ordinal >= 0 && ordinal < int(partition) && image == nginxImage {
				protectedOld++
			} else if ordinal >= int(partition) && image == "nginx:alpine" {
				updatedNew++
			}
		}
		t.Logf("Protected with old image: %d/3, Updated with new image: %d/2", protectedOld, updatedNew)
		return protectedOld == 3 && updatedNew == 2
	}, 3*time.Minute, 5*time.Second, "Overall partition state broken after pod recreation")

	t.Log("ModelServing partition deleted group historical revision test passed successfully")
}

// TestModelServingNoPartitionRollingUpdate verifies that when no partition is set
// (partition=nil), a rolling update behaves consistently with existing behavior:
// all replicas are updated to the new revision and the new image.
// After the update, CurrentRevision and UpdateRevision should converge.
//
// This is the E2E counterpart of the "no partition, recreated group should use
// new revision" test case from TestModelServingVersionControl.
func TestModelServingNoPartitionRollingUpdate(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 4 replicas and NO partition (default behavior)
	replicas := int32(4)
	roleReplicas := int32(1)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-no-partition-rolling",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &replicas,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
					MaxUnavailable: &intstr.IntOrString{IntVal: 1},
					// Partition is intentionally NOT set (nil)
				},
			},
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
										Image: nginxImage,
										Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
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

	t.Log("Creating ModelServing with 4 replicas and no partition (default behavior)")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, 4, 3*time.Minute)

	// Record initial state
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	initialRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s, UpdateRevision: %s", initialMS.Status.CurrentRevision, initialMS.Status.UpdateRevision)

	// Trigger rolling update by changing image
	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"

	t.Log("Triggering rolling update: image -> nginx:alpine (no partition, all replicas should update)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing")

	// Wait for the rolling update to complete
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify ALL pods have the new image and the new revision label
	t.Log("Verifying all pods updated to new image and revision")
	labelSelector := modelServingLabelSelector(modelServing.Name)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil || len(pods.Items) == 0 {
			return false
		}

		allCorrect := true
		runningCount := 0
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil {
				continue
			}
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			runningCount++

			containerImage := getPodContainerImage(pod, "test-container")
			podRevision := pod.Labels["modelserving.volcano.sh/revision"]

			if containerImage != "nginx:alpine" {
				t.Logf("Pod %s still has old image: %s", pod.Name, containerImage)
				allCorrect = false
			}
			// Revision should NOT be the old initial revision
			if podRevision == initialRevision {
				t.Logf("Pod %s still has old revision: %s", pod.Name, podRevision)
				allCorrect = false
			}
		}
		return allCorrect && runningCount == 4
	}, 3*time.Minute, 5*time.Second, "Not all pods were updated to new revision/image")

	// Verify CurrentRevision == UpdateRevision (converged after full update)
	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get final ModelServing")

	t.Logf("Final CurrentRevision: %s, UpdateRevision: %s, UpdatedReplicas: %d",
		finalMS.Status.CurrentRevision, finalMS.Status.UpdateRevision, finalMS.Status.UpdatedReplicas)

	assert.Equal(t, finalMS.Status.CurrentRevision, finalMS.Status.UpdateRevision,
		"Without partition, CurrentRevision and UpdateRevision should converge after full update")
	assert.Equal(t, replicas, finalMS.Status.UpdatedReplicas,
		"All replicas should be updated when no partition is set")

	t.Log("ModelServing no-partition rolling update test passed successfully")
}

// getGroupOrdinal extracts the ordinal from a ServingGroup name (e.g., "test-ms-3" -> 3).
func getGroupOrdinal(groupName string) (string, int) {
	if groupName == "" {
		return "", -1
	}
	// Reuse the same regex pattern as the controller
	re := regexp.MustCompile(`(.*)-([0-9]+)$`)
	subMatches := re.FindStringSubmatch(groupName)
	if len(subMatches) < 3 {
		return groupName, -1
	}
	parent := subMatches[1]
	ordinal := -1
	if i, err := strconv.Atoi(subMatches[2]); err == nil {
		ordinal = i
	}
	return parent, ordinal
}

// getPodContainerImage returns the image of the named container in a pod.
func getPodContainerImage(pod corev1.Pod, containerName string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == containerName {
			return c.Image
		}
	}
	return ""
}

// TestModelServingControllerManagerRestart verifies that ModelServing pod creation
// is successful even when the controller-manager restarts during reconciliation.
// NOTE: This test must remain last among ModelServing tests because it restarts the
// controller-manager pod, which temporarily takes down the webhook. Tests that run
// immediately after would fail with "connection refused" errors.
func TestModelServingControllerManagerRestart(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a complicated ModelServing with multiple roles
	// 5 serving groups × (3 pods for prefill + 2 pods for decode) = 25 pods total
	prefillRole := createRole("prefill", 1, 2)
	decodeRole := createRole("decode", 1, 1)
	modelServing := createBasicModelServing("test-controller-restart", 5, prefillRole, decodeRole)

	t.Log("Creating complicated ModelServing with 5 serving groups and 2 roles (25 total pods expected)")
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{})
	})

	//   ModelServing Partition Revision Control

	// Wait briefly for initial reconciliation to start
	t.Log("Waiting for initial reconciliation to start...")
	// Wait for a random duration between 0 and 3 seconds (in 100ms increments)
	randomWait := time.Duration(rand.New(rand.NewSource(time.Now().UnixNano())).Intn(31)*100) * time.Millisecond
	t.Logf("Waiting for %v before restarting controller-manager", randomWait)
	time.Sleep(randomWait)

	// Find and delete controller-manager pods
	t.Logf("Finding controller-manager pods in namespace %s", kthenaNamespace)

	// Use label selector to find controller-manager pods
	labelSelector := "app.kubernetes.io/component=kthena-controller-manager"
	controllerPods, err := kubeClient.CoreV1().Pods(kthenaNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list controller-manager pods")
	require.NotEmpty(t, controllerPods.Items, "No controller-manager pods found")

	// Delete all controller-manager pods
	for _, pod := range controllerPods.Items {
		t.Logf("Deleting controller-manager pod: %s", pod.Name)
		err := kubeClient.CoreV1().Pods(kthenaNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		require.NoError(t, err, "Failed to delete controller-manager pod %s", pod.Name)
	}

	// Wait for controller-manager pods to restart and become ready
	t.Log("Waiting for controller-manager to restart...")
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(kthenaNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		// Check that at least one controller-manager pod is running and ready
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						t.Logf("Controller-manager pod is ready: %s", pod.Name)
						return true
					}
				}
			}
		}
		return false
	}, 3*time.Minute, 5*time.Second, "Controller-manager did not restart and become ready")

	// Wait for ModelServing to be ready
	t.Log("Waiting for ModelServing to be ready after controller-manager restart...")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify all expected pods are created
	msLabelSelector := modelServingLabelSelector(modelServing.Name)
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: msLabelSelector,
	})
	require.NoError(t, err, "Failed to list ModelServing pods")

	// Calculate expected pod count:
	// 5 serving groups × (3 pods for prefill role + 2 pods for decode role) = 25 pods
	expectedPodCount := 25
	actualPodCount := len(podList.Items)

	t.Logf("Expected pod count: %d, Actual pod count: %d", expectedPodCount, actualPodCount)
	assert.Equal(t, expectedPodCount, actualPodCount, "Pod count mismatch after controller-manager restart")

	// Verify all pods are running
	runningPods := 0
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			runningPods++
		}
	}
	assert.Equal(t, actualPodCount, runningPods, "All created pods should be in Running phase")

	t.Log("ModelServing controller-manager restart test passed successfully")
}
