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
	"k8s.io/utils/ptr"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	controllerutils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

func TestModelServingRollingUpdateMaxUnavailable(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 4 replicas and maxUnavailable set to 2
	replicas := int32(4)
	modelServing := createBasicModelServing("test-rolling-update-maxunavailable", replicas, replicas)
	t.Log("Creating ModelServing with 4 replicas and maxUnavailable=2")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Verify initial state
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(4), *initialMS.Spec.Replicas, "Initial ModelServing should have 4 replicas")

	// Update the ModelServing to trigger a rolling update (change image)
	updatedMS := initialMS.DeepCopy()
	// Modify the container image to trigger a rolling update
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage

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
	assert.Equal(t, nginxAlpineImage, finalMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image, "Final ModelServing should have updated image")

	// Verify that maxUnavailable was never exceeded during the update
	assert.True(t, maxObservedUnavailable <= 2, "Max unavailable replicas (%d) exceeded maxUnavailable limit (2)", maxObservedUnavailable)
	t.Logf("Max observed unavailable replicas during update: %d", maxObservedUnavailable)

	watcherCancel()
	mu.Lock()
	t.Logf("Maximum observed unavailable replicas during test: %d", maxObservedUnavailable)
	mu.Unlock()

	t.Log("ModelServing rolling update maxUnavailable test passed successfully")
}

// TestModelServingRollingUpdateMaxSurge verifies that ServingGroupRollingUpdate
// temporarily creates maxSurge ServingGroup capacity before removing old groups.
func TestModelServingRollingUpdateMaxSurge(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	replicas := int32(4)
	modelServing := createBasicModelServing("test-rolling-update-maxsurge", replicas, 1)
	modelServing.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = ptr.To(intstr.FromInt(0))
	modelServing.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = ptr.To(intstr.FromInt(1))
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	initial, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initial.Status.CurrentRevision
	require.NotEmpty(t, initialRevision)

	updated := initial.DeepCopy()
	updated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Verify that one additional Running ServingGroup becomes available while
	// old and new revisions coexist. Ordinals do not identify surge capacity:
	// binpack scale-down may leave any ordinal in the final replica set.
	require.Eventually(t, func() bool {
		states, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
		if err != nil || len(states) > int(replicas+1) {
			return false
		}
		if len(states) != int(replicas+1) {
			return false
		}
		hasOld, hasNew := false, false
		for _, state := range states {
			hasOld = hasOld || state.Revision == initialRevision
			hasNew = hasNew || (state.Revision != initialRevision && state.Image == nginxAlpineImage)
		}
		return hasOld && hasNew
	}, 2*time.Minute, time.Second, "expected maxSurge capacity while old and new ServingGroups coexist")

	finalMS := waitForRollingUpdateConverged(t, ctx, kthenaClient, kubeClient, modelServing.Name, replicas, initialRevision, nginxAlpineImage)
	assert.Equal(t, replicas, finalMS.Status.Replicas)
	assert.Equal(t, replicas, finalMS.Status.AvailableReplicas)
	assert.Equal(t, replicas, finalMS.Status.UpdatedReplicas)
	assert.Equal(t, finalMS.Status.UpdateRevision, finalMS.Status.CurrentRevision)
}

// TestModelServingRollingUpdateMaxUnavailableWithBadImage tests maxUnavailable constraint when transitioning to bad image
func TestModelServingRollingUpdateMaxUnavailableWithBadImage(t *testing.T) {
	ctx, kthenaClient, _ := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 6 replicas and maxUnavailable set to 2
	replicas := int32(6)
	modelServing := createBasicModelServing("test-rolling-update-bad-image", replicas, 0)
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

// TestModelServingPartitionBoundaryProtection verifies partition boundaries during rolling updates.
func TestModelServingPartitionBoundaryProtection(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const (
		replicas  = int32(5)
		partition = int32(3)
	)

	modelServing := createPartitionedModelServing("test-partition-boundary", replicas, partition)
	t.Logf("Creating ModelServing with %d replicas and partition=%d", replicas, partition)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s", initialMS.Status.CurrentRevision)
	require.NotEmpty(t, initialRevision, "Initial CurrentRevision should be set")

	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	t.Logf("Updating image to %s", nginxAlpineImage)

	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	updateRevision := waitForPartitionState(t, ctx, kthenaClient, kubeClient, modelServing.Name, partition, replicas, initialRevision)
	assert.NotEqual(t, initialRevision, updateRevision)
}

// within partition are rebuilt using historical revision.
func TestModelServingPartitionDeletedGroupHistoricalRevision(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const (
		replicas  = int32(5)
		partition = int32(3)
	)

	modelServing := createPartitionedModelServing("test-partition-historical", replicas, partition)
	modelServing.Spec.RecoveryPolicy = workload.RoleRecreate
	t.Logf("Creating ModelServing with %d replicas and partition=%d", replicas, partition)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s", initialRevision)
	require.NotEmpty(t, initialRevision, "Initial CurrentRevision should be set")

	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	t.Logf("Updating image to %s", nginxAlpineImage)

	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	updateRevision := waitForPartitionState(t, ctx, kthenaClient, kubeClient, modelServing.Name, partition, replicas, initialRevision)
	t.Log("Partitioned update established")

	targetOrdinal := 1
	targetGroupName := fmt.Sprintf("%s-%d", modelServing.Name, targetOrdinal)
	labelSelector := fmt.Sprintf("%s=%s", workload.GroupNameLabelKey, targetGroupName)

	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err)
	require.NotEmpty(t, pods.Items)

	podToDelete := pods.Items[0]
	t.Logf("Deleting pod %s (ordinal %d)", podToDelete.Name, targetOrdinal)

	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, podToDelete.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	require.Eventually(t, func() bool {
		ordinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
		if err != nil {
			t.Logf("Failed to collect serving group states: %v", err)
			return false
		}
		state, ok := ordinalStates[int32(targetOrdinal)]
		if !ok {
			return false
		}
		t.Logf("Recreated protected ordinal %d => group=%s revision=%s image=%s", targetOrdinal, state.GroupName, state.Revision, state.Image)
		return state.Revision == initialRevision &&
			state.Image == nginxImage
	}, 3*time.Minute, 2*time.Second, "Recreated pod should use historical revision")

	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	ordinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
	require.NoError(t, err)
	protectedCorrect, updatedCorrect := calculateGroupPartitionState(t, ordinalStates, partition, replicas, initialRevision, updateRevision)
	assert.Equal(t, int(partition), protectedCorrect)
	assert.Equal(t, int(replicas-partition), updatedCorrect)
	assert.Equal(t, initialRevision, finalMS.Status.CurrentRevision)
	assert.Equal(t, updateRevision, finalMS.Status.UpdateRevision)
}

// assigns the updated revision to newly created ServingGroups and leaves protected groups untouched.
func TestModelServingPartitionScaleUp(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const (
		initialReplicas = int32(5)
		partition       = int32(3)
		scaledReplicas  = int32(7)
	)

	modelServing := createPartitionedModelServing("test-partition-scale-up", initialReplicas, partition)
	t.Logf("Creating ModelServing with %d replicas and partition=%d", initialReplicas, partition)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Get initial state and trigger a rolling update to establish the partition
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s", initialRevision)
	require.NotEmpty(t, initialRevision, "Initial CurrentRevision should be set")

	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	t.Logf("Updating image to %s to establish partition state", nginxAlpineImage)

	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	updateRevision := waitForPartitionState(t, ctx, kthenaClient, kubeClient, modelServing.Name, partition, initialReplicas, initialRevision)
	t.Logf("Partition state established: CurrentRevision=%s, UpdateRevision=%s", initialRevision, updateRevision)

	// Capture initial UIDs of protected groups to ensure they are not recreated
	initialProtectedStates, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
	require.NoError(t, err)

	// Scale up from 5 to 7 replicas
	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)

	scaleUpMS := currentMS.DeepCopy()
	scaleUpMS.Spec.Replicas = ptr.To(scaledReplicas)
	t.Logf("Scaling up from %d to %d replicas while partition=%d", initialReplicas, scaledReplicas, partition)

	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, scaleUpMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify: protected ordinals 0-2 have old revision, ordinals 3-6 have new revision
	require.Eventually(t, func() bool {
		ordinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
		if err != nil {
			t.Logf("Failed to collect serving group states: %v", err)
			return false
		}
		if len(ordinalStates) != int(scaledReplicas) {
			t.Logf("Running serving group count: %d (expecting %d)", len(ordinalStates), scaledReplicas)
			return false
		}
		if !hasExpectedOrdinalRange(ordinalStates, scaledReplicas) {
			t.Logf("ServingGroup ordinals do not cover [0, %d): %v", scaledReplicas, ordinalStates)
			return false
		}
		protectedCorrect, updatedCorrect := calculateGroupPartitionState(t, ordinalStates, partition, scaledReplicas, initialRevision, updateRevision)
		t.Logf("Protected: %d/%d, Updated: %d/%d", protectedCorrect, partition, updatedCorrect, scaledReplicas-partition)
		return protectedCorrect == int(partition) && updatedCorrect == int(scaledReplicas-partition)
	}, 3*time.Minute, 2*time.Second, "Partition state did not converge after scale up")

	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, scaledReplicas, *finalMS.Spec.Replicas)
	assert.Equal(t, initialRevision, finalMS.Status.CurrentRevision)
	assert.Equal(t, updateRevision, finalMS.Status.UpdateRevision)

	// Verify protected UIDs didn't change
	finalOrdinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
	require.NoError(t, err)
	for ordinal, initialState := range initialProtectedStates {
		if ordinal < partition {
			finalState, ok := finalOrdinalStates[ordinal]
			require.True(t, ok, "Protected group %d should still exist", ordinal)
			assert.Equal(t, initialState.Revision, finalState.Revision, "Protected group %d should not have been recreated", ordinal)
		}
	}

	t.Log("ModelServing partition scale up test passed successfully")
}

// removes updated ServingGroups (ordinals >= partition) first and leaves protected groups untouched.
func TestModelServingPartitionScaleDown(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const (
		initialReplicas = int32(5)
		partition       = int32(3)
		scaledReplicas  = int32(3)
	)

	modelServing := createPartitionedModelServing("test-partition-scale-down", initialReplicas, partition)
	t.Logf("Creating ModelServing with %d replicas and partition=%d", initialReplicas, partition)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	// Get initial state and trigger a rolling update to establish the partition
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s", initialRevision)
	require.NotEmpty(t, initialRevision, "Initial CurrentRevision should be set")

	t.Logf("Updating image to %s to establish partition state", nginxAlpineImage)

	// rolling update
	updateModelServingWithRetry(t, ctx, kthenaClient, modelServing.Name, func(ms *workload.ModelServing) {
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	})
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	updateRevision := waitForPartitionState(t, ctx, kthenaClient, kubeClient, modelServing.Name, partition, initialReplicas, initialRevision)

	// Scale down from 5 to 3 replicas (equal to partition, so all updated groups should be removed)
	t.Logf("Scaling down from %d to %d replicas while partition=%d", initialReplicas, scaledReplicas, partition)

	// Update the ModelServing to scale down
	updateModelServingWithRetry(t, ctx, kthenaClient, modelServing.Name, func(ms *workload.ModelServing) {
		ms.Spec.Replicas = ptr.To(scaledReplicas)
	})
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify: only protected ordinals 0-2 remain with old revision, no updated groups

	ordinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, modelServing.Name)
	require.NoError(t, err)

	// All remaining groups should be on the old (current) revision
	protectedCorrect, updatedCorrect := calculateGroupPartitionState(t, ordinalStates, partition, scaledReplicas, initialRevision, updateRevision)
	require.Equal(t, int(scaledReplicas), protectedCorrect, "Protected groups not as expected after scale down")
	require.Equal(t, 0, updatedCorrect, "Updated groups not as expected after scale down")

	finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, scaledReplicas, *finalMS.Spec.Replicas)
	require.Equal(t, scaledReplicas, finalMS.Status.Replicas)
	require.Equal(t, int32(0), finalMS.Status.UpdatedReplicas)
	require.Equal(t, scaledReplicas, finalMS.Status.CurrentReplicas)
	require.Equal(t, scaledReplicas, finalMS.Status.AvailableReplicas)
}

// TestModelServingRollingUpdate verifies rolling updates without partition.
func TestModelServingRollingUpdate(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const replicas = int32(3)

	modelServing := createBasicModelServing("test-rolling-update", replicas, 0)
	t.Logf("Creating ModelServing with %d replicas", replicas)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	t.Logf("Initial CurrentRevision: %s", initialRevision)

	labelSelector := modelServingLabelSelector(modelServing.Name)
	verifyAllPodsHaveImage(t, ctx, kubeClient, labelSelector, nginxImage, "before update")

	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	t.Logf("Updating image to %s", nginxAlpineImage)

	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	verifyAllPodsHaveImage(t, ctx, kubeClient, labelSelector, nginxAlpineImage, "after update")

	finalMS := waitForRollingUpdateConverged(t, ctx, kthenaClient, kubeClient, modelServing.Name, replicas, initialRevision, nginxAlpineImage)
	t.Logf("Rolling update completed - CurrentRevision: %s", finalMS.Status.CurrentRevision)
}

func createPartitionedModelServing(name string, replicas, partition int32) *workload.ModelServing {
	roleReplicas := int32(1)
	return &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas: &replicas,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
					Partition:      ptr.To(intstr.FromInt32(partition)),
					MaxUnavailable: ptr.To(intstr.FromInt(int(replicas))),
				},
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "role",
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
						WorkerReplicas: 0,
					},
				},
			},
		},
	}
}

type servingGroupState struct {
	GroupName string
	Ordinal   int32
	Revision  string
	Image     string
}

func collectRunningServingGroupStates(ctx context.Context, kubeClient *kubernetes.Clientset, msName string) (map[int32]servingGroupState, error) {
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: modelServingLabelSelector(msName),
	})
	if err != nil {
		return nil, err
	}

	states := make(map[int32]servingGroupState)
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		groupName := pod.Labels[workload.GroupNameLabelKey]
		if groupName == "" {
			continue
		}
		parentName, ordinal := controllerutils.GetParentNameAndOrdinal(groupName)
		if parentName != msName || ordinal < 0 {
			continue
		}
		revision := pod.Labels[workload.RevisionLabelKey]
		if revision == "" || len(pod.Spec.Containers) == 0 {
			continue
		}

		state := servingGroupState{
			GroupName: groupName,
			Ordinal:   int32(ordinal),
			Revision:  revision,
			Image:     pod.Spec.Containers[0].Image,
		}

		if _, ok := states[state.Ordinal]; !ok {
			states[state.Ordinal] = state
		}
	}

	return states, nil
}

func waitForPartitionState(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset,
	kubeClient *kubernetes.Clientset, msName string, partition, replicas int32, initialRevision string) string {
	t.Helper()

	var updateRevision string
	require.Eventually(t, func() bool {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, msName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		ordinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, msName)
		if err != nil {
			t.Logf("Failed to collect serving group states: %v", err)
			return false
		}
		if len(ordinalStates) != int(replicas) {
			t.Logf("Running serving group count: %d (expecting %d)", len(ordinalStates), replicas)
			return false
		}
		if !hasExpectedOrdinalRange(ordinalStates, replicas) {
			t.Logf("ServingGroup ordinals do not cover [0, %d): %v", replicas, ordinalStates)
			return false
		}
		protectedCorrect, updatedCorrect := calculateGroupPartitionState(t, ordinalStates, partition, replicas, initialRevision, ms.Status.UpdateRevision)
		t.Logf("CurrentRevision: %s, UpdateRevision: %s, Protected: %d/%d, Updated: %d/%d",
			ms.Status.CurrentRevision, ms.Status.UpdateRevision, protectedCorrect, partition, updatedCorrect, replicas-partition)
		if ms.Status.CurrentRevision != initialRevision ||
			ms.Status.UpdateRevision == "" ||
			ms.Status.UpdateRevision == initialRevision ||
			protectedCorrect != int(partition) ||
			updatedCorrect != int(replicas-partition) {
			return false
		}
		updateRevision = ms.Status.UpdateRevision
		return true
	}, 3*time.Minute, 2*time.Second, "Partition state did not converge")

	return updateRevision
}

// running ServingGroups all use UpdateRevision and the updated image. Ordinals may
// remain sparse after binpack scale-down, so convergence is based on count and state.
func waitForRollingUpdateConverged(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset,
	kubeClient *kubernetes.Clientset, msName string, replicas int32, initialRevision, updatedImage string) *workload.ModelServing {
	t.Helper()

	var finalMS *workload.ModelServing
	require.Eventually(t, func() bool {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, msName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		if ms.Status.UpdateRevision == "" ||
			ms.Status.UpdateRevision == initialRevision ||
			ms.Status.CurrentRevision != ms.Status.UpdateRevision {
			return false
		}
		if ms.Status.Replicas != replicas ||
			ms.Status.AvailableReplicas != replicas ||
			ms.Status.UpdatedReplicas != replicas {
			t.Logf("Replicas: %d, AvailableReplicas: %d, UpdatedReplicas: %d (expecting %d)",
				ms.Status.Replicas, ms.Status.AvailableReplicas, ms.Status.UpdatedReplicas, replicas)
			return false
		}
		ordinalStates, err := collectRunningServingGroupStates(ctx, kubeClient, msName)
		if err != nil {
			t.Logf("Failed to collect serving group states: %v", err)
			return false
		}
		if len(ordinalStates) != int(replicas) {
			t.Logf("Running serving group count: %d (expecting %d)", len(ordinalStates), replicas)
			return false
		}
		for ordinal, state := range ordinalStates {
			if state.Revision != ms.Status.UpdateRevision || state.Image != updatedImage {
				t.Logf("Ordinal %d not on UpdateRevision yet: revision=%s image=%s", ordinal, state.Revision, state.Image)
				return false
			}
		}
		finalMS = ms
		return true
	}, 3*time.Minute, 2*time.Second, "Rolling update did not converge")

	return finalMS
}

// and how many are on the updated revision, based on the partition boundary.
func calculateGroupPartitionState(t *testing.T, ordinalStates map[int32]servingGroupState,
	partition, replicas int32, currentRevision, updateRevision string) (protected, updated int) {
	t.Helper()
	for ordinal, state := range ordinalStates {
		isProtected := partition > 0 && ordinal < partition
		if isProtected && state.Revision == currentRevision && state.Image == nginxImage {
			protected++
		} else if !isProtected && state.Revision == updateRevision {
			updated++
		}
	}
	return
}

func verifyAllPodsHaveImage(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset,
	labelSelector, expectedImage, phase string) {
	t.Helper()
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
			for _, container := range pod.Spec.Containers {
				if container.Image != expectedImage {
					return false
				}
			}
		}
		return true
	}, 2*time.Minute, 1*time.Second, "All pods should have image %s %s", expectedImage, phase)

	t.Logf("Verified all pods have image %s %s", expectedImage, phase)
}

// by updating individual roles without recreating the entire ServingGroup
func TestModelServingRoleBasedRollingUpdate(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	// Create a ModelServing with 2 replicas and 2 roles
	replicas := int32(2)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-based-rolling-update",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas:       &replicas,
			RecoveryPolicy: workload.RoleRecreate,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.RoleRollingUpdate, // Using role-based rolling update
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To[int32](2), // Each ServingGroup has 2 prefill pods
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
					{
						Name:     "decode",
						Replicas: ptr.To[int32](1), // Each ServingGroup has 1 decode pod
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

	// waiting for webhook to be ready before running tests
	waitForWebhookReady(t, ctx, kthenaClient, testNamespace)

	// Create the ModelServing
	t.Log("Creating ModelServing with 2 replicas and 2 roles for role-based rolling update test")
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, modelServing, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing")

	// Register cleanup for ModelServing
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		t.Logf("Cleaning up ModelServing: %s/%s", modelServing.Namespace, modelServing.Name)
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, modelServing.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Warning: Failed to delete ModelServing %s/%s: %v", modelServing.Namespace, modelServing.Name, err)
		}
	})

	// Wait for the initial ModelServing to be ready
	t.Log("Waiting for initial ModelServing to be ready")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	// Verify initial state
	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get initial ModelServing")
	assert.Equal(t, int32(2), *initialMS.Spec.Replicas, "Initial ModelServing should have 2 replicas")
	assert.Equal(t, int32(2), initialMS.Status.AvailableReplicas, "Initial ModelServing should have 2 available replicas")

	// Update the ModelServing to trigger a role-based rolling update (change prefill role image)
	updatedMS := initialMS.DeepCopy()
	// Modify the container image of the prefill role to trigger a rolling update
	for i := range updatedMS.Spec.Template.Roles {
		if updatedMS.Spec.Template.Roles[i].Name == "prefill" {
			updatedMS.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "nginx:alpine"
			break
		}
	}

	decodePodLabelSelector := fmt.Sprintf("modelserving.volcano.sh/name=%s,modelserving.volcano.sh/role=decode", modelServing.Name)
	decodePodList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: decodePodLabelSelector,
	})
	assert.NoError(t, err, "Failed to list decode pods before update")

	assert.Equalf(t, 2, len(decodePodList.Items), "There should be 2 decode pods before update")
	decodePodsUID := make(map[string]string, len(decodePodList.Items))
	for _, pod := range decodePodList.Items {
		decodePodsUID[pod.Name] = string(pod.UID)
	}

	t.Log("Updating ModelServing to trigger role-based rolling update (changing prefill role image)")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err, "Failed to update ModelServing for role-based rolling update")

	// Monitor the role-based rolling update to ensure only prefill role pods are replaced
	t.Log("Monitoring role-based rolling update to ensure only prefill role pods are replaced while decode role pods remain")

	// It is possible that the ‘modelServing Ready’ check completed before the change in the modelServing status,
	// causing subsequent checks to fail. Therefore, the checks have been retried.
	// This has improved the robustness of the end-to-end tests.
	require.Eventually(t, func() bool {
		// Wait for the rolling update to complete
		utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, updatedMS.Name)

		// Get final state
		finalMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, updatedMS.Name, metav1.GetOptions{})
		require.NoError(t, err, "Failed to get final ModelServing")
		assert.Equal(t, int32(2), *finalMS.Spec.Replicas, "Final ModelServing should have 2 replicas in spec")
		assert.Equal(t, int32(2), finalMS.Status.AvailableReplicas, "Final ModelServing should have 2 available replicas after update")

		// Verify that the prefill role image has been updated
		prefillRoleUpdated := false
		for _, role := range finalMS.Spec.Template.Roles {
			if role.Name == "prefill" && role.EntryTemplate.Spec.Containers[0].Image == "nginx:alpine" {
				prefillRoleUpdated = true
				break
			}
		}
		assert.True(t, prefillRoleUpdated, "Prefill role should have been updated to nginx:alpine")

		prefillPodLabelSelector := fmt.Sprintf("modelserving.volcano.sh/name=%s,modelserving.volcano.sh/role=prefill", modelServing.Name)
		prefillPodList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: prefillPodLabelSelector,
		})
		if err != nil {
			t.Logf("Failed to list prefill pods: %v", err)
			return false
		}

		// Check if all prefill pods have the updated image
		for _, pod := range prefillPodList.Items {
			if pod.Spec.Containers[0].Image != "nginx:alpine" {
				t.Logf("Prefill pod %s still has image %s, expecting nginx:alpine", pod.Name, pod.Spec.Containers[0].Image)
				return false
			}
		}

		// Check if all prefill pods have the updated image
		decodePodLabelSelector := fmt.Sprintf("modelserving.volcano.sh/name=%s,modelserving.volcano.sh/role=decode", modelServing.Name)
		decodePodList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: decodePodLabelSelector,
		})
		if err != nil {
			t.Logf("Failed to list decode pods: %v", err)
			return false
		}

		// Check if all decode pods still have the original image
		for _, pod := range decodePodList.Items {
			if pod.Spec.Containers[0].Image != nginxImage {
				t.Logf("Decode pod %s has image %s, expecting original image %s", pod.Name, pod.Spec.Containers[0].Image, nginxImage)
				return false
			}

			uid, exist := decodePodsUID[pod.Name]
			if !exist || string(pod.UID) != uid {
				t.Logf("Decode pod %s has been replaced", pod.Name)
				return false
			}
		}

		return true
	}, 2*time.Minute, 1*time.Second)

	t.Log("ModelServing role-based rolling update test passed successfully")
}

// role-level maxUnavailable and leaves unaffected roles untouched.
func TestModelServingRoleRollingUpdateMaxUnavailable(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const badImage = "nginx:role-rollingupdate-maxunavailable-bad-image"
	replicas := int32(1)
	prefillReplicas := int32(4)
	decodeReplicas := int32(1)

	maxUnavailable := intstr.FromInt(2)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-rolling-maxunavailable",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas:       &replicas,
			RecoveryPolicy: workload.RoleRecreate,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.RoleRollingUpdate,
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: &prefillReplicas,
						RollingUpdateConfiguration: workload.RollingUpdateConfiguration{
							MaxUnavailable: &maxUnavailable,
						},
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "test-container",
									Image: nginxImage,
									Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
								}},
							},
						},
					},
					{
						Name:     "decode",
						Replicas: &decodeReplicas,
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "test-container",
									Image: nginxImage,
									Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
								}},
							},
						},
					},
				},
			},
		},
	}

	t.Log("Creating ModelServing for RoleRollingUpdate maxUnavailable test")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	decodeUIDs := map[string]string{}
	decodeSelector := fmt.Sprintf("%s,%s=decode", modelServingLabelSelector(modelServing.Name), workload.RoleLabelKey)
	decodePods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: decodeSelector})
	require.NoError(t, err)
	require.Len(t, decodePods.Items, int(decodeReplicas), "unexpected initial decode pod count")
	for _, pod := range decodePods.Items {
		decodeUIDs[pod.Name] = string(pod.UID)
	}

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	updatedMS := initialMS.DeepCopy()
	for i := range updatedMS.Spec.Template.Roles {
		if updatedMS.Spec.Template.Roles[i].Name == "prefill" {
			updatedMS.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = badImage
		}
	}

	t.Log("Updating prefill role to a bad image; only maxUnavailable prefill replicas should be replaced")
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)

	countRolePods := func() (oldPrefillRunning, badPrefill, decodeUnchanged int, ok bool) {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: modelServingLabelSelector(modelServing.Name),
		})
		if err != nil {
			t.Logf("Failed to list pods: %v", err)
			return 0, 0, 0, false
		}

		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || len(pod.Spec.Containers) == 0 {
				continue
			}
			switch pod.Labels[workload.RoleLabelKey] {
			case "prefill":
				if pod.Spec.Containers[0].Image == nginxImage && pod.Status.Phase == corev1.PodRunning {
					oldPrefillRunning++
				}
				if pod.Spec.Containers[0].Image == badImage {
					badPrefill++
				}
			case "decode":
				if pod.Spec.Containers[0].Image != nginxImage {
					continue
				}
				if uid, exists := decodeUIDs[pod.Name]; exists && uid == string(pod.UID) {
					decodeUnchanged++
				}
			}
		}
		return oldPrefillRunning, badPrefill, decodeUnchanged, true
	}

	require.Eventually(t, func() bool {
		oldPrefillRunning, badPrefill, decodeUnchanged, ok := countRolePods()
		t.Logf("oldPrefillRunning=%d badPrefill=%d decodeUnchanged=%d", oldPrefillRunning, badPrefill, decodeUnchanged)
		return ok && oldPrefillRunning == 2 && badPrefill == 2 && decodeUnchanged == int(decodeReplicas)
	}, 2*time.Minute, 2*time.Second, "RoleRollingUpdate should replace only maxUnavailable prefill replicas")

	stableUntil := time.Now().Add(30 * time.Second)
	for time.Now().Before(stableUntil) {
		oldPrefillRunning, badPrefill, decodeUnchanged, ok := countRolePods()
		t.Logf("stable check: oldPrefillRunning=%d badPrefill=%d decodeUnchanged=%d", oldPrefillRunning, badPrefill, decodeUnchanged)
		require.True(t, ok && oldPrefillRunning >= 2 && badPrefill <= 2 && decodeUnchanged == int(decodeReplicas),
			"RoleRollingUpdate should not exceed role maxUnavailable while updated pods are unavailable")
		time.Sleep(2 * time.Second)
	}
}

// temporarily creates additional Role capacity and contracts after convergence.
// TestModelServingRoleRollingUpdateMaxSurge verifies that RoleRollingUpdate
// temporarily creates maxSurge Role capacity and contracts after convergence.
func TestModelServingRoleRollingUpdateMaxSurge(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	replicas := int32(1)
	prefillReplicas := int32(2)
	decodeReplicas := int32(1)
	maxUnavailable := intstr.FromInt(0)
	maxSurge := intstr.FromInt(1)
	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-role-rolling-maxsurge", Namespace: testNamespace},
		Spec: workload.ModelServingSpec{
			Replicas:        &replicas,
			RecoveryPolicy:  workload.RoleRecreate,
			RolloutStrategy: &workload.RolloutStrategy{Type: workload.RoleRollingUpdate},
			Template: workload.ServingGroup{Roles: []workload.Role{
				{
					Name:     "prefill",
					Replicas: &prefillReplicas,
					RollingUpdateConfiguration: workload.RollingUpdateConfiguration{
						MaxUnavailable: &maxUnavailable,
						MaxSurge:       &maxSurge,
					},
					EntryTemplate: workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container", Image: nginxImage}}}},
				},
				{
					Name:          "decode",
					Replicas:      &decodeReplicas,
					EntryTemplate: workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test-container", Image: nginxImage}}}},
				},
			}},
		},
	}
	waitForWebhookReady(t, ctx, kthenaClient, testNamespace)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	decodeSelector := fmt.Sprintf("%s,%s=decode", modelServingLabelSelector(modelServing.Name), workload.RoleLabelKey)
	decodePods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: decodeSelector})
	require.NoError(t, err)
	require.Len(t, decodePods.Items, 1)
	decodeUID := decodePods.Items[0].UID

	current, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	updated := current.DeepCopy()
	for i := range updated.Spec.Template.Roles {
		if updated.Spec.Template.Roles[i].Name == "prefill" {
			updated.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
		}
	}
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	prefillSelector := fmt.Sprintf("%s,%s=prefill", modelServingLabelSelector(modelServing.Name), workload.RoleLabelKey)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: prefillSelector})
		if err != nil || len(pods.Items) > int(prefillReplicas)+1 {
			return false
		}
		if len(pods.Items) != int(prefillReplicas)+1 {
			return false
		}
		hasOld, hasNew := false, false
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || len(pod.Spec.Containers) == 0 {
				continue
			}
			hasOld = hasOld || pod.Spec.Containers[0].Image == nginxImage
			hasNew = hasNew || pod.Spec.Containers[0].Image == nginxAlpineImage
		}
		return hasOld && hasNew
	}, 2*time.Minute, time.Second, "expected maxSurge Role capacity while old and new replicas coexist")

	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: prefillSelector})
		if err != nil || len(pods.Items) != int(prefillReplicas) {
			return false
		}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Image != nginxAlpineImage {
				return false
			}
		}
		return true
	}, 2*time.Minute, time.Second, "expected Role surge capacity to contract after rollout")

	decodePods, err = kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: decodeSelector})
	require.NoError(t, err)
	require.Len(t, decodePods.Items, 1)
	assert.Equal(t, decodeUID, decodePods.Items[0].UID, "unchanged Role should not be replaced")
}

// Align with ServingGroup semantics: partition protects replicas whose ordinals are in [0, partition).
func TestModelServingRoleRollingUpdatePartition(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	replicas := int32(1)
	initialRoleReplicas := int32(4)
	partition := int32(2)

	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-rolling-partition",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas:       &replicas,
			RecoveryPolicy: workload.RoleRecreate,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.RoleRollingUpdate,
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: &initialRoleReplicas,
						RollingUpdateConfiguration: workload.RollingUpdateConfiguration{
							Partition:      ptr.To(intstr.FromInt32(partition)),
							MaxUnavailable: ptr.To(intstr.FromInt(int(initialRoleReplicas))),
						},
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "test-container",
									Image: nginxImage,
									Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
								}},
							},
						},
						WorkerReplicas: 0,
					},
				},
			},
		},
	}

	selector := fmt.Sprintf("%s,%s=prefill,%s=%s", modelServingLabelSelector(modelServing.Name), workload.RoleLabelKey, workload.EntryLabelKey, controllerutils.Entry)

	t.Logf("Creating ModelServing with %d prefill replicas for RoleRollingUpdate partition test", initialRoleReplicas)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, int(initialRoleReplicas), 3*time.Minute)

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	updatedMS.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt32(partition))

	t.Logf("Updating prefill role image to %s; partition=%d should keep ordinals [0, %d) on old image", nginxAlpineImage, partition, partition)
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			t.Logf("Failed to list prefill entry pods: %v", err)
			return false
		}
		if len(pods.Items) != int(initialRoleReplicas) {
			t.Logf("Prefill pod count=%d, expecting %d", len(pods.Items), initialRoleReplicas)
			return false
		}

		imageByOrdinal := map[int32]string{}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) == 0 {
				continue
			}
			roleID := pod.Labels[workload.RoleIDKey]
			_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
			imageByOrdinal[int32(ordinal)] = pod.Spec.Containers[0].Image
		}
		if len(imageByOrdinal) != int(initialRoleReplicas) {
			t.Logf("Observed running ordinals=%d, expecting %d", len(imageByOrdinal), initialRoleReplicas)
			return false
		}
		if !hasExpectedOrdinalRange(imageByOrdinal, initialRoleReplicas) {
			t.Logf("Role ordinals do not cover [0, %d): %v", initialRoleReplicas, imageByOrdinal)
			return false
		}

		for ord := int32(0); ord < partition; ord++ {
			if imageByOrdinal[ord] != nginxImage {
				t.Logf("Protected prefill-%d image=%s, expecting old image=%s", ord, imageByOrdinal[ord], nginxImage)
				return false
			}
		}
		updatedCorrect := 0
		for ord, img := range imageByOrdinal {
			if ord < partition {
				continue
			}
			if img != nginxAlpineImage {
				t.Logf("Updatable prefill-%d image=%s, expecting new image=%s", ord, img, nginxAlpineImage)
				return false
			}
			updatedCorrect++
		}
		if updatedCorrect != int(initialRoleReplicas-partition) {
			t.Logf("Updated replicas=%d, expecting %d; images=%v", updatedCorrect, initialRoleReplicas-partition, imageByOrdinal)
			return false
		}
		return true
	}, 3*time.Minute, 2*time.Second, "RoleRollingUpdate partition state did not converge")
}

// revision and the final Role ordinals exactly cover [0, roleReplicas).
func TestModelServingRolePartitionScaleUp(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const (
		servingGroupReplicas = int32(1)
		roleReplicas         = int32(3)
		partition            = int32(1)
	)

	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-partition-scale-up",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas:       ptr.To(servingGroupReplicas),
			RecoveryPolicy: workload.RoleRecreate,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.RoleRollingUpdate,
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To(roleReplicas),
						RollingUpdateConfiguration: workload.RollingUpdateConfiguration{
							Partition:      ptr.To(intstr.FromInt32(partition)),
							MaxUnavailable: ptr.To(intstr.FromInt(int(roleReplicas))),
						},
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "test-container",
									Image: nginxImage,
									Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
								}},
							},
						},
						WorkerReplicas: 0,
					},
				},
			},
		},
	}

	selector := fmt.Sprintf("%s,%s=prefill,%s=%s", modelServingLabelSelector(modelServing.Name), workload.RoleLabelKey, workload.EntryLabelKey, controllerutils.Entry)

	t.Logf("Creating ModelServing with %d prefill replicas and partition=%d", roleReplicas, partition)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, int(roleReplicas), 3*time.Minute)

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	require.NotEmpty(t, initialRevision, "Initial CurrentRevision should be set")

	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	t.Logf("Updating prefill role image to %s to establish partition state", nginxAlpineImage)
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	var updateRevision string
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil || len(pods.Items) != int(roleReplicas) {
			return false
		}
		imageByOrdinal := map[int32]string{}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) == 0 {
				continue
			}
			roleID := pod.Labels[workload.RoleIDKey]
			_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
			imageByOrdinal[int32(ordinal)] = pod.Spec.Containers[0].Image
		}
		if len(imageByOrdinal) != int(roleReplicas) {
			return false
		}
		if !hasExpectedOrdinalRange(imageByOrdinal, roleReplicas) {
			t.Logf("Role ordinals do not cover [0, %d): %v", roleReplicas, imageByOrdinal)
			return false
		}
		for ord := int32(0); ord < partition; ord++ {
			if imageByOrdinal[ord] != nginxImage {
				t.Logf("Partition state not ready: protected prefill-%d image=%s", ord, imageByOrdinal[ord])
				return false
			}
		}
		updatedCorrect := 0
		for ord, img := range imageByOrdinal {
			if ord < partition {
				continue
			}
			if img != nginxAlpineImage {
				t.Logf("Partition state not ready: non-protected prefill-%d image=%s", ord, img)
				return false
			}
			updatedCorrect++
		}
		if updatedCorrect != int(roleReplicas-partition) {
			t.Logf("Partition state not ready: updated replicas=%d, expecting %d, images=%v", updatedCorrect, roleReplicas-partition, imageByOrdinal)
			return false
		}
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
		if err != nil || ms.Status.UpdateRevision == "" || ms.Status.UpdateRevision == initialRevision {
			return false
		}
		updateRevision = ms.Status.UpdateRevision
		return true
	}, 3*time.Minute, 2*time.Second, "Role partition state did not converge before scale up recovery test")

	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	require.NoError(t, err)
	var podToDelete *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		roleID := pod.Labels[workload.RoleIDKey]
		_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
		if ordinal == 0 {
			podToDelete = pod
			break
		}
	}
	require.NotNil(t, podToDelete, "prefill-0 pod should exist")
	originalUID := string(podToDelete.UID)
	t.Logf("Deleting protected prefill-0 pod %s", podToDelete.Name)
	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, podToDelete.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			t.Logf("Failed to list prefill entry pods: %v", err)
			return false
		}
		if len(pods.Items) != int(roleReplicas) {
			t.Logf("Prefill pod count=%d, expecting %d", len(pods.Items), roleReplicas)
			return false
		}

		imageByOrdinal := map[int32]string{}
		revisionByOrdinal := map[int32]string{}
		uidByOrdinal := map[int32]string{}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) == 0 {
				continue
			}
			roleID := pod.Labels[workload.RoleIDKey]
			_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
			ord := int32(ordinal)
			imageByOrdinal[ord] = pod.Spec.Containers[0].Image
			revisionByOrdinal[ord] = pod.Labels[workload.RevisionLabelKey]
			uidByOrdinal[ord] = string(pod.UID)
		}
		if len(imageByOrdinal) != int(roleReplicas) {
			t.Logf("Observed ordinals=%d, expecting %d", len(imageByOrdinal), roleReplicas)
			return false
		}
		if !hasExpectedOrdinalRange(imageByOrdinal, roleReplicas) {
			t.Logf("Role ordinals do not cover [0, %d): %v", roleReplicas, imageByOrdinal)
			return false
		}
		if imageByOrdinal[0] != nginxImage {
			t.Logf("Recreated prefill-0 image=%s, expecting old image=%s", imageByOrdinal[0], nginxImage)
			return false
		}
		if revisionByOrdinal[0] != initialRevision {
			t.Logf("Recreated prefill-0 revision=%s, expecting %s", revisionByOrdinal[0], initialRevision)
			return false
		}
		if uidByOrdinal[0] == originalUID {
			t.Log("prefill-0 was not recreated with a new UID")
			return false
		}
		updatedCount := 0
		for ord, img := range imageByOrdinal {
			if ord == 0 {
				continue
			}
			if img != nginxAlpineImage {
				t.Logf("Non-protected prefill-%d image=%s, expecting %s", ord, img, nginxAlpineImage)
				return false
			}
			if revisionByOrdinal[ord] != updateRevision {
				t.Logf("Non-protected prefill-%d revision=%s, expecting %s", ord, revisionByOrdinal[ord], updateRevision)
				return false
			}
			updatedCount++
		}
		if updatedCount != int(roleReplicas)-int(partition) {
			t.Logf("Non-protected replicas=%d, expecting %d; images=%v", updatedCount, roleReplicas-partition, imageByOrdinal)
			return false
		}
		return true
	}, 3*time.Minute, 2*time.Second, "Partition-protected role replica should be recreated at ordinal 0 with historical revision")

	t.Log("ModelServing role partition scale up test passed successfully")
}

// removes updated replicas first and leaves partition-protected replicas untouched.
func TestModelServingRolePartitionScaleDown(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const (
		servingGroupReplicas = int32(1)
		initialRoleReplicas  = int32(5)
		scaledRoleReplicas   = int32(3)
		partition            = int32(3)
	)

	modelServing := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-role-partition-scale-down",
			Namespace: testNamespace,
		},
		Spec: workload.ModelServingSpec{
			Replicas:       ptr.To(servingGroupReplicas),
			RecoveryPolicy: workload.RoleRecreate,
			RolloutStrategy: &workload.RolloutStrategy{
				Type: workload.RoleRollingUpdate,
			},
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To(initialRoleReplicas),
						RollingUpdateConfiguration: workload.RollingUpdateConfiguration{
							Partition:      ptr.To(intstr.FromInt32(partition)),
							MaxUnavailable: ptr.To(intstr.FromInt(int(initialRoleReplicas))),
						},
						EntryTemplate: workload.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:  "test-container",
									Image: nginxImage,
									Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
								}},
							},
						},
						WorkerReplicas: 0,
					},
				},
			},
		},
	}

	selector := fmt.Sprintf("%s,%s=prefill,%s=%s", modelServingLabelSelector(modelServing.Name), workload.RoleLabelKey, workload.EntryLabelKey, controllerutils.Entry)

	t.Logf("Creating ModelServing with %d prefill replicas and partition=%d", initialRoleReplicas, partition)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	waitForRunningPodCount(t, ctx, kubeClient, modelServing.Name, int(initialRoleReplicas), 3*time.Minute)

	initialMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	initialRevision := initialMS.Status.CurrentRevision
	require.NotEmpty(t, initialRevision, "Initial CurrentRevision should be set")

	updatedMS := initialMS.DeepCopy()
	updatedMS.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	t.Logf("Updating prefill role image to %s to establish partition state", nginxAlpineImage)
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, updatedMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil || len(pods.Items) != int(initialRoleReplicas) {
			return false
		}
		imageByOrdinal := map[int32]string{}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) == 0 {
				continue
			}
			roleID := pod.Labels[workload.RoleIDKey]
			_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
			imageByOrdinal[int32(ordinal)] = pod.Spec.Containers[0].Image
		}
		if len(imageByOrdinal) != int(initialRoleReplicas) {
			return false
		}
		for ord := int32(0); ord < partition; ord++ {
			if imageByOrdinal[ord] != nginxImage {
				return false
			}
		}
		updatedCorrect := 0
		for ord, img := range imageByOrdinal {
			if ord < partition {
				continue
			}
			if img != nginxAlpineImage {
				return false
			}
			updatedCorrect++
		}
		if updatedCorrect != int(initialRoleReplicas-partition) {
			t.Logf("Partition state not ready: updated replicas=%d, expecting %d, images=%v",
				updatedCorrect, initialRoleReplicas-partition, imageByOrdinal)
			return false
		}
		return true
	}, 3*time.Minute, 2*time.Second, "Role partition state did not converge before scale down")

	// Make the scale-down decision sensitive to partition:
	// - Assign LOWER deletionCost to protected ordinals [0, partition) so they would be deleted first without partition.
	// - Assign HIGHER deletionCost to non-protected ordinals so they would be kept without partition.
	// With partition enabled, the controller must still delete non-protected replicas first.
	podsBeforeScaleDown, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	require.NoError(t, err)
	for _, pod := range podsBeforeScaleDown.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		roleID := pod.Labels[workload.RoleIDKey]
		_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
		if int32(ordinal) < partition {
			patchPodDeletionCost(t, ctx, kubeClient, pod.Name, 0)
		} else {
			patchPodDeletionCost(t, ctx, kubeClient, pod.Name, 10000+ordinal)
		}
	}

	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err)
	scaleDownMS := currentMS.DeepCopy()
	scaleDownMS.Spec.Template.Roles[0].Replicas = ptr.To(scaledRoleReplicas)
	scaleDownMS.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(int(scaledRoleReplicas)))
	t.Logf("Scaling down prefill role from %d to %d replicas while partition=%d", initialRoleReplicas, scaledRoleReplicas, partition)
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, scaleDownMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, modelServing.Name)

	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			t.Logf("Failed to list prefill entry pods: %v", err)
			return false
		}
		if len(pods.Items) != int(scaledRoleReplicas) {
			t.Logf("Prefill pod count=%d, expecting %d", len(pods.Items), scaledRoleReplicas)
			return false
		}

		imageByOrdinal := map[int32]string{}
		revisionByOrdinal := map[int32]string{}
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || len(pod.Spec.Containers) == 0 {
				continue
			}
			roleID := pod.Labels[workload.RoleIDKey]
			_, ordinal := controllerutils.GetParentNameAndOrdinal(roleID)
			ord := int32(ordinal)
			imageByOrdinal[ord] = pod.Spec.Containers[0].Image
			revisionByOrdinal[ord] = pod.Labels[workload.RevisionLabelKey]
		}
		if len(imageByOrdinal) != int(scaledRoleReplicas) {
			t.Logf("Observed ordinals=%d, expecting %d", len(imageByOrdinal), scaledRoleReplicas)
			return false
		}
		for ord := int32(0); ord < scaledRoleReplicas; ord++ {
			if imageByOrdinal[ord] != nginxImage {
				t.Logf("Protected prefill-%d image=%s, expecting old image=%s", ord, imageByOrdinal[ord], nginxImage)
				return false
			}
			if revisionByOrdinal[ord] != initialRevision {
				t.Logf("Protected prefill-%d revision=%s, expecting %s", ord, revisionByOrdinal[ord], initialRevision)
				return false
			}
		}
		updatedCorrect := 0
		for ord := range imageByOrdinal {
			if ord >= partition {
				updatedCorrect++
			}
		}
		if updatedCorrect != 0 {
			t.Logf("Updated replicas remaining=%d, expecting 0; images=%v", updatedCorrect, imageByOrdinal)
			return false
		}
		return true
	}, 3*time.Minute, 2*time.Second, "Scale down should keep only partition-protected role replicas")

	t.Log("ModelServing role partition scale down test passed successfully")
}
