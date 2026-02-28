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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const nginxImage = "nginx:1.28.2"

// TODO(user): Add E2E tests for ModelServing lifecycle (Create, Update, Delete and check if it works)
func TestModelServingLifecycle(t *testing.T) {
	// Placeholder for ModelServing lifecycle tests
	t.Skip("ModelServing lifecycle test is not implemented yet")
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

// TestModelServingPodRecovery verifies that when a pod is deleted,
// the corresponding role can recreate the pod successfully.
func TestModelServingPodRecovery(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client
	kubeClient, err := utils.GetKubeClient()
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a basic ModelServing
	modelServing := createBasicModelServing("test-pod-recovery", 1)
	modelServing.Spec.RecoveryPolicy = workload.RoleRecreate

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
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client
	kubeClient, err := utils.GetKubeClient()
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

	// Create Kubernetes client
	kubeClient, err := utils.GetKubeClient()
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
	kubeClient, err := utils.GetKubeClient()
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

// TestModelServingRoleStatusEvents verifies that role status transitions are surfaced via Kubernetes Events.
func TestModelServingRoleStatusEvents(t *testing.T) {
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

	// Create Kubernetes client locally
	kubeConfig, err := utils.GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err, "Failed to create Kubernetes client")

	// Create a simple ModelServing with a single role replica to keep the signal clean.
	modelServing := createBasicModelServing("test-role-status-events", 1)

	t.Log("Creating ModelServing for role status events test")
	createdMS, err := kthenaClient.WorkloadV1alpha1().
		ModelServings(testNamespace).
		Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Ensure cleanup
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", createdMS.Namespace, createdMS.Name)
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, createdMS.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", createdMS.Namespace, createdMS.Name, err)
		}
	})

	// Wait until ModelServing is ready so that role transitions to Running.
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, createdMS.Name)

	// Refresh to get UID for precise event filtering.
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, createdMS.Name, metav1.GetOptions{})
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
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

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
	ctx, kthenaClient := setupControllerManagerE2ETest(t)

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
		return errors.IsNotFound(err)
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
