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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// TestModelServingLifecycle verifies the full lifecycle of a ModelServing resource:
// Create -> Verify Ready -> Update (change image) -> Verify Updated -> Delete -> Verify Deleted.
func TestModelServingLifecycle(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// --- Phase 1: Create ---
	modelServing := createBasicModelServing("test-lifecycle", 1)

	t.Log("Phase 1: Creating ModelServing")
	created, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")
	assert.Equal(t, modelServing.Name, created.Name)

	// Wait for ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify pods are running
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods")
	require.NotEmpty(t, podList.Items, "Expected at least one pod after creation")
	for _, pod := range podList.Items {
		assert.Equal(t, corev1.PodRunning, pod.Status.Phase, "Pod %s should be running", pod.Name)
	}
	t.Log("Phase 1 passed: ModelServing created and ready")

	// --- Phase 2: Update (change container image) ---
	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing for update")

	updatedMS := currentMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"

	t.Log("Phase 2: Updating ModelServing (changing image to nginx:alpine)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing")

	// Wait for the update to complete and ModelServing to be ready again
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify the image was updated on pods
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil || len(pods.Items) == 0 {
			return false
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			for _, container := range pod.Spec.Containers {
				if container.Name == "test-container" && container.Image == "nginx:alpine" {
					return true
				}
			}
		}
		return false
	}, 3*time.Minute, 5*time.Second, "Pod image was not updated to nginx:alpine")
	t.Log("Phase 2 passed: ModelServing updated successfully")

	// --- Phase 3: Delete ---
	t.Log("Phase 3: Deleting ModelServing")
	err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, modelServing.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServing")

	// Verify the ModelServing is deleted
	require.Eventually(t, func() bool {
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
		return err != nil // should return NotFound error
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
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create a basic ModelServing with 1 replica
	modelServing := createBasicModelServing("test-scale-up", 1)

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

// TestModelServingScaleDown tests the ability to scale down a ModelServing's ServingGroup.
func TestModelServingScaleDown(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a basic ModelServing with 3 replicas
	modelServing := createBasicModelServing("test-scale-down", 3)

	t.Log("Creating ModelServing with 3 servingGroup replicas")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{})
	})

	// Wait for the initial ModelServing to be ready
	t.Log("Waiting for initial ModelServing (3 servingGroup replicas) to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify initial state - should have 3 replicas
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(3), *initialMS.Spec.Replicas, "Initial ModelServing should have 3 replicas")
	assert.Equal(t, int32(3), initialMS.Status.AvailableReplicas, "Initial ModelServing should have 3 available replicas")

	// Verify we have the expected number of pods
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods before scale down")
	initialPodCount := len(podList.Items)
	t.Logf("Initial pod count: %d", initialPodCount)

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

	// Final verification - should have 1 replica
	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, updatedMS.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get final ModelServing")
	assert.Equal(t, int32(1), *finalMS.Spec.Replicas, "Final ModelServing should have 1 replica in spec")
	assert.Equal(t, int32(1), finalMS.Status.AvailableReplicas, "Final ModelServing should have 1 available replica")

	// Verify pod count has decreased
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
		t.Logf("Current running pod count: %d (expecting 1)", runningCount)
		return runningCount == 1
	}, 2*time.Minute, 5*time.Second, "Pod count did not decrease to expected value after scale down")

	t.Log("ModelServing scale down test passed successfully")
}

// TestModelServingRoleScaleUp tests scaling up the role replicas within a ServingGroup.
func TestModelServingRoleScaleUp(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

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
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{})
	})

	// Wait for ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Count initial pods
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods")
	initialPodCount := len(podList.Items)
	t.Logf("Initial pod count: %d", initialPodCount)

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
		t.Logf("Current running pod count after role scale up: %d (expecting 3)", runningCount)
		return runningCount == 3
	}, 3*time.Minute, 5*time.Second, "Pod count did not increase after role scale up")

	t.Log("ModelServing role scale up test passed successfully")
}

// TestModelServingRoleScaleDown tests scaling down the role replicas within a ServingGroup.
func TestModelServingRoleScaleDown(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

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
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{})
	})

	// Wait for ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Count initial pods (expect 3: 1 servingGroup × 3 role replicas × 1 entry pod)
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			return false
		}
		runningCount := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				runningCount++
			}
		}
		return runningCount == 3
	}, 3*time.Minute, 5*time.Second, "Expected 3 running pods initially")

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
		t.Logf("Current running pod count after role scale down: %d (expecting 1)", runningCount)
		return runningCount == 1
	}, 3*time.Minute, 5*time.Second, "Pod count did not decrease after role scale down")

	t.Log("ModelServing role scale down test passed successfully")
}

// TestModelServingServingGroupRecreate verifies that when a pod is deleted under the
// ServingGroupRecreate recovery policy, the entire ServingGroup is recreated.
func TestModelServingServingGroupRecreate(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a ModelServing with ServingGroupRecreate policy and 2 roles
	prefillRole := createRole("prefill", 1, 0)
	decodeRole := createRole("decode", 1, 0)
	modelServing := createBasicModelServing("test-sg-recreate", 1, prefillRole, decodeRole)
	modelServing.Spec.RecoveryPolicy = workload.ServingGroupRecreate

	t.Log("Creating ModelServing with ServingGroupRecreate policy and 2 roles (prefill + decode)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{})
	})

	// Wait until ModelServing is ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Collect all pod UIDs before deletion
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
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
		for _, pod := range pods.Items {
			// Must be a new pod (not in original UIDs) and must be ready
			if !originalUIDs[string(pod.UID)] && pod.DeletionTimestamp == nil {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyNewPods++
						break
					}
				}
			}
		}
		t.Logf("New ready pods: %d (expecting 2 for full ServingGroup recreate)", readyNewPods)
		return readyNewPods >= 2
	}, 3*time.Minute, 5*time.Second, "ServingGroup was not fully recreated after pod deletion under ServingGroupRecreate policy")

	t.Log("ModelServing ServingGroupRecreate test passed successfully")
}

// TestModelServingHeadlessServiceDeleteOnServingGroupDelete verifies that when a ModelServing
// is scaled down (servingGroups are deleted), the corresponding headless services are also cleaned up.
func TestModelServingHeadlessServiceDeleteOnServingGroupDelete(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a ModelServing with 3 servingGroup replicas
	modelServing := createBasicModelServing("test-svc-sg-delete", 3)

	t.Log("Creating ModelServing with 3 servingGroup replicas")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{})
	})

	// Wait for ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Get the ModelServing UID
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	// Count initial headless services owned by this ModelServing
	labelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
	serviceList, err := kubeClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list services")

	initialHeadlessCount := 0
	for _, svc := range serviceList.Items {
		for _, ref := range svc.OwnerReferences {
			if ref.UID == ms.UID && svc.Spec.ClusterIP == corev1.ClusterIPNone {
				initialHeadlessCount++
				break
			}
		}
	}
	t.Logf("Initial headless service count: %d", initialHeadlessCount)

	// Scale down to 1 replica (removing 2 servingGroups)
	scaleDownMS := ms.DeepCopy()
	newReplicas := int32(1)
	scaleDownMS.Spec.Replicas = &newReplicas

	t.Log("Scaling down ModelServing to 1 replica to trigger servingGroup deletion")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, scaleDownMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for scale down")

	// Wait for the ModelServing to be ready
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify headless services were cleaned up proportionally
	if initialHeadlessCount > 0 {
		require.Eventually(t, func() bool {
			// Re-read the ModelServing to get latest UID
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
			t.Logf("Current headless service count: %d (expecting fewer than %d)", headlessCount, initialHeadlessCount)
			return headlessCount < initialHeadlessCount
		}, 2*time.Minute, 5*time.Second, "Headless services were not cleaned up after servingGroup deletion")
	}

	t.Log("ModelServing headless service cleanup on servingGroup delete test passed successfully")
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
	modelServing := createBasicModelServing("test-pod-recovery", 1)

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
	modelServing := createBasicModelServing("test-service-recovery", 1)

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

func TestModelServingRollingUpdateMaxUnavailable(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

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
										Image: "nginx:latest", // Initial image
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

	// Create the ModelServing
	t.Log("Creating ModelServing with 4 replicas and maxUnavailable=2")
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Wait for the initial ModelServing to be ready
	t.Log("Waiting for initial ModelServing (4 replicas) to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify initial state
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(4), *initialMS.Spec.Replicas, "Initial ModelServing should have 4 replicas")
	assert.Equal(t, int32(4), initialMS.Status.AvailableReplicas, "Initial ModelServing should have 4 available replicas")

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
	assert.Equal(t, int32(4), finalMS.Status.AvailableReplicas, "Final ModelServing should have 4 available replicas after update")
	assert.Equal(t, finalMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image, "nginx:alpine", "Final ModelServing should have updated image")

	// Verify that maxUnavailable was never exceeded during the update
	assert.True(t, maxObservedUnavailable <= 2, "Max unavailable replicas (%d) exceeded maxUnavailable limit (2)", maxObservedUnavailable)
	t.Logf("Max observed unavailable replicas during update: %d", maxObservedUnavailable)

	watcherCancel()
	mu.Lock()
	t.Logf("Maximum observed unavailable replicas during test: %d", maxObservedUnavailable)
	mu.Unlock()

	t.Log("ModelServing rolling update maxUnavailable test passed successfully")
}

// TestModelServingControllerManagerRestart verifies that ModelServing pod creation
// is successful even when the controller-manager restarts during reconciliation.
func TestModelServingControllerManagerRestart(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a complicated ModelServing with multiple roles
	// 5 serving groups × (3 pods for prefill + 2 pods for decode) = 25 pods total
	prefillRole := createRole("prefill", 1, 2)
	decodeRole := createRole("decode", 1, 1)
	modelServing := createBasicModelServing("test-controller-restart", 5, prefillRole, decodeRole)

	t.Log("Creating complicated ModelServing with 5 serving groups and 2 roles (25 total pods expected)")
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
	msLabelSelector := "modelserving.volcano.sh/name=" + modelServing.Name
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
		WorkerReplicas: workerReplicas,
		WorkerTemplate: &workload.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "worker-container",
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
