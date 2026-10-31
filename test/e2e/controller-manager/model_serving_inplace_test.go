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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const inPlaceUpdateTimeout = 3 * time.Minute

// podSnapshot records what identifies a Pod across an update. A Pod that is
// deleted and recreated keeps its name but gets a new UID.
type podSnapshot struct {
	uid  types.UID
	role string
}

// newInPlaceModelServing builds a ModelServing with servingGroups ServingGroups. With
// ServingGroupRollingUpdate maxUnavailable limits the ServingGroups updated at once; with
// RoleRollingUpdate it limits the prefill Role replicas (prefillReplicas) updated at once.
func newInPlaceModelServing(name string, strategy workload.RolloutStrategyType, servingGroups, prefillReplicas int32, maxUnavailable int) *workload.ModelServing {
	prefill := createRole("prefill", prefillReplicas, 0)
	decode := createRole("decode", 1, 0)
	rollout := &workload.RolloutStrategy{
		Type:         strategy,
		UpdateMethod: workload.InPlaceUpdateMethod,
	}
	if strategy == workload.RoleRollingUpdate {
		prefill.MaxUnavailable = ptr.To(intstr.FromInt(maxUnavailable))
		decode.MaxUnavailable = ptr.To(intstr.FromInt(1))
	} else {
		rollout.RollingUpdateConfiguration = &workload.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(maxUnavailable))}
	}
	ms := createBasicModelServing(name, servingGroups, 0, prefill, decode)
	ms.Spec.RecoveryPolicy = workload.RoleRecreate
	ms.Spec.RolloutStrategy = rollout
	return ms
}

func snapshotPods(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string) map[string]podSnapshot {
	t.Helper()
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: modelServingLabelSelector(msName)})
	require.NoError(t, err)
	snapshot := make(map[string]podSnapshot, len(pods.Items))
	for _, pod := range pods.Items {
		snapshot[pod.Name] = podSnapshot{uid: pod.UID, role: pod.Labels[workload.RoleLabelKey]}
	}
	require.NotEmpty(t, snapshot)
	return snapshot
}

func listPodsByName(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string) map[string]corev1.Pod {
	t.Helper()
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: modelServingLabelSelector(msName)})
	require.NoError(t, err)
	byName := make(map[string]corev1.Pod, len(pods.Items))
	for _, pod := range pods.Items {
		byName[pod.Name] = pod
	}
	return byName
}

func isPodReady(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitForInPlaceUpdated waits until every Pod of the roles in updatedRoles runs
// image, is Ready, and has finished its in-place update (state done, no longer in progress).
func waitForInPlaceUpdated(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName, image string, updatedRoles ...string) {
	t.Helper()
	updated := map[string]bool{}
	for _, role := range updatedRoles {
		updated[role] = true
	}
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: modelServingLabelSelector(msName)})
		if err != nil || len(pods.Items) == 0 {
			return false
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if !updated[pod.Labels[workload.RoleLabelKey]] {
				continue
			}
			state := inplace.StateOf(pod)
			if !isPodReady(*pod) || pod.Spec.Containers[0].Image != image || state == nil || !state.Done || inplace.InProgress(pod) {
				return false
			}
		}
		return true
	}, inPlaceUpdateTimeout, 2*time.Second, "pods of roles %v should be updated in place to %s", updatedRoles, image)
}

// trackUnavailableUnits watches the Pods of a ModelServing and records the largest number of update
// units that were unavailable at the same time. unitOf maps a Pod to its unit (a ServingGroup or a
// Role replica) and reports false for Pods that are not part of the update. A unit is unavailable
// while any of its Pods is not Ready or has an unfinished in-place update. The returned function
// stops the watch and returns the peak.
func trackUnavailableUnits(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string, unitOf func(corev1.Pod) (string, bool)) func() int {
	t.Helper()
	watchCtx, cancel := context.WithCancel(ctx)
	watcher, err := kubeClient.CoreV1().Pods(testNamespace).Watch(watchCtx, metav1.ListOptions{LabelSelector: modelServingLabelSelector(msName)})
	require.NoError(t, err)

	var mu sync.Mutex
	peak := 0
	var watchErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		pods := map[string]corev1.Pod{}
		for event := range watcher.ResultChan() {
			if event.Type == watch.Error {
				if watchCtx.Err() != nil {
					// Stopping the tracker itself breaks the stream.
					return
				}
				mu.Lock()
				watchErr = fmt.Errorf("watch error: %v", apierrors.FromObject(event.Object))
				mu.Unlock()
				return
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if event.Type == watch.Deleted {
				delete(pods, pod.Name)
			} else {
				pods[pod.Name] = *pod
			}
			unavailable := map[string]bool{}
			for _, p := range pods {
				if unit, ok := unitOf(p); ok && (!isPodReady(p) || inplace.InProgress(&p)) {
					unavailable[unit] = true
				}
			}
			mu.Lock()
			if len(unavailable) > peak {
				peak = len(unavailable)
			}
			mu.Unlock()
		}
		if watchCtx.Err() == nil {
			mu.Lock()
			watchErr = fmt.Errorf("watch closed before the update finished")
			mu.Unlock()
		}
	}()
	return func() int {
		cancel()
		watcher.Stop()
		<-done
		mu.Lock()
		defer mu.Unlock()
		// A broken watch means the peak may be incomplete, so it cannot prove the budget held.
		require.NoError(t, watchErr, "the unavailable-unit tracker lost its watch")
		return peak
	}
}

// assertUpdateSteps checks that units were never unavailable beyond maxUnavailable, and that
// more than one unit was updated at once, so the update ran in batches and not one by one.
func assertUpdateSteps(t *testing.T, peak, maxUnavailable int, units string) {
	t.Helper()
	assert.LessOrEqual(t, peak, maxUnavailable, "%s exceeded maxUnavailable", units)
	assert.Greater(t, peak, 1, "%s were updated one at a time instead of in steps of maxUnavailable", units)
}

// assertNoPodRecreated fails when a Pod from before the update is gone or has a new UID.
func assertNoPodRecreated(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string, before map[string]podSnapshot) {
	t.Helper()
	after := listPodsByName(t, ctx, kubeClient, msName)
	assert.Len(t, after, len(before), "the number of pods must not change")
	for name, snap := range before {
		pod, ok := after[name]
		if !assert.True(t, ok, "pod %s was deleted during the update", name) {
			continue
		}
		assert.Equal(t, snap.uid, pod.UID, "pod %s was recreated instead of updated in place", name)
	}
}

// assertContainerRestartedOnce checks that kubelet restarted the container for the new image, and only once.
func assertContainerRestartedOnce(t *testing.T, pod corev1.Pod) {
	t.Helper()
	require.NotEmpty(t, pod.Status.ContainerStatuses)
	assert.Equal(t, int32(1), pod.Status.ContainerStatuses[0].RestartCount, "pod %s should have restarted exactly once", pod.Name)
}

func assertRevisionsConverged(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string, roles ...string) {
	t.Helper()
	wanted := map[string]bool{}
	for _, role := range roles {
		wanted[role] = true
	}
	revisions := map[string]bool{}
	for _, pod := range listPodsByName(t, ctx, kubeClient, msName) {
		if wanted[pod.Labels[workload.RoleLabelKey]] {
			revisions[pod.Labels[workload.RevisionLabelKey]] = true
		}
	}
	assert.Len(t, revisions, 1, "pods must not carry mixed revisions after the update, got %v", revisions)
}

func setImages(image string, roles ...string) func(*workload.ModelServing) {
	return func(ms *workload.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			role := &ms.Spec.Template.Roles[i]
			if len(roles) > 0 && !contains(roles, role.Name) {
				continue
			}
			role.EntryTemplate.Spec.Containers[0].Image = image
		}
	}
}

func contains(items []string, item string) bool {
	for _, i := range items {
		if i == item {
			return true
		}
	}
	return false
}

func createInPlaceModelServing(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, ms *workload.ModelServing) {
	t.Helper()
	waitForWebhookReady(t, ctx, kthenaClient, testNamespace)
	createAndWaitForModelServing(t, ctx, kthenaClient, ms)
}

// TestModelServingInPlaceUpdateServingGroup verifies that with ServingGroupRollingUpdate an
// image-only change patches the existing Pods of every ServingGroup instead of recreating them.
func TestModelServingInPlaceUpdateServingGroup(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)
	const servingGroups, maxUnavailable = 4, 2
	ms := newInPlaceModelServing("test-inplace-servinggroup", workload.ServingGroupRollingUpdate, servingGroups, 2, maxUnavailable)
	createInPlaceModelServing(t, ctx, kthenaClient, ms)

	before := snapshotPods(t, ctx, kubeClient, ms.Name)
	stopTracking := trackUnavailableUnits(t, ctx, kubeClient, ms.Name, func(pod corev1.Pod) (string, bool) {
		return pod.Labels[workload.GroupNameLabelKey], true
	})
	t.Logf("Updating image of all roles in place, %d pods before update", len(before))
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, setImages(nginxAlpineImage))

	waitForInPlaceUpdated(t, ctx, kubeClient, ms.Name, nginxAlpineImage, "prefill", "decode")
	utilsWaitReady(t, ctx, kthenaClient, ms.Name)
	assertUpdateSteps(t, stopTracking(), maxUnavailable, "ServingGroups")

	assertNoPodRecreated(t, ctx, kubeClient, ms.Name, before)
	for _, pod := range listPodsByName(t, ctx, kubeClient, ms.Name) {
		assert.Equal(t, nginxAlpineImage, pod.Spec.Containers[0].Image)
		assertContainerRestartedOnce(t, pod)
	}
	assertRevisionsConverged(t, ctx, kubeClient, ms.Name, "prefill", "decode")
}

// TestModelServingInPlaceUpdateRole verifies that with RoleRollingUpdate only the Pods of the
// Role whose image changed are restarted, and that they are patched rather than recreated.
func TestModelServingInPlaceUpdateRole(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)
	const prefillReplicas, maxUnavailable = 4, 2
	ms := newInPlaceModelServing("test-inplace-role", workload.RoleRollingUpdate, 1, prefillReplicas, maxUnavailable)
	createInPlaceModelServing(t, ctx, kthenaClient, ms)

	before := snapshotPods(t, ctx, kubeClient, ms.Name)
	stopTracking := trackUnavailableUnits(t, ctx, kubeClient, ms.Name, func(pod corev1.Pod) (string, bool) {
		if pod.Labels[workload.RoleLabelKey] != "prefill" {
			return "", false
		}
		return pod.Labels[workload.GroupNameLabelKey] + "/" + pod.Labels[workload.RoleIDKey], true
	})
	t.Log("Updating image of the prefill role in place")
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, setImages(nginxAlpineImage, "prefill"))

	waitForInPlaceUpdated(t, ctx, kubeClient, ms.Name, nginxAlpineImage, "prefill")
	utilsWaitReady(t, ctx, kthenaClient, ms.Name)
	assertUpdateSteps(t, stopTracking(), maxUnavailable, "prefill Role replicas")

	assertNoPodRecreated(t, ctx, kubeClient, ms.Name, before)
	for name, pod := range listPodsByName(t, ctx, kubeClient, ms.Name) {
		switch before[name].role {
		case "prefill":
			assert.Equal(t, nginxAlpineImage, pod.Spec.Containers[0].Image)
			assertContainerRestartedOnce(t, pod)
		default:
			assert.Equal(t, nginxImage, pod.Spec.Containers[0].Image, "unchanged role must keep its image")
			assert.Equal(t, int32(0), pod.Status.ContainerStatuses[0].RestartCount, "unchanged role %s must not restart", name)
			assert.Nil(t, inplace.StateOf(&pod), "unchanged role %s must not be touched", name)
		}
	}
	assertRevisionsConverged(t, ctx, kubeClient, ms.Name, "prefill")
}

// TestModelServingInPlaceUpdateFallsBackToRecreate verifies that a change beyond container
// images is not applied in place: the Pods are deleted and recreated, as with Recreate.
func TestModelServingInPlaceUpdateFallsBackToRecreate(t *testing.T) {
	tests := []struct {
		name     string
		strategy workload.RolloutStrategyType
	}{
		{name: "ServingGroupRollingUpdate", strategy: workload.ServingGroupRollingUpdate},
		{name: "RoleRollingUpdate", strategy: workload.RoleRollingUpdate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)
			ms := newInPlaceModelServing("test-inplace-fallback-"+map[workload.RolloutStrategyType]string{
				workload.ServingGroupRollingUpdate: "sg",
				workload.RoleRollingUpdate:         "role",
			}[tt.strategy], tt.strategy, 2, 2, 1)
			createInPlaceModelServing(t, ctx, kthenaClient, ms)

			before := snapshotPods(t, ctx, kubeClient, ms.Name)
			updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(ms *workload.ModelServing) {
				container := &ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0]
				container.Image = nginxAlpineImage
				container.Env = append(container.Env, corev1.EnvVar{Name: "INPLACE_E2E", Value: "changed"})
			})

			updatedRole := ms.Spec.Template.Roles[0].Name
			// Walk the original Pods, so a Pod that is temporarily absent cannot pass the check.
			var originals []string
			for name, snapshot := range before {
				if snapshot.role == updatedRole {
					originals = append(originals, name)
				}
			}
			require.NotEmpty(t, originals, "role %s should have Pods before the update", updatedRole)
			require.Eventually(t, func() bool {
				current := listPodsByName(t, ctx, kubeClient, ms.Name)
				for _, name := range originals {
					pod, ok := current[name]
					if !ok || pod.UID == before[name].uid || !isPodReady(pod) || pod.Spec.Containers[0].Image != nginxAlpineImage {
						return false
					}
				}
				return true
			}, inPlaceUpdateTimeout, 2*time.Second, "pods of role %s should be recreated", updatedRole)
			utilsWaitReady(t, ctx, kthenaClient, ms.Name)

			for name, pod := range listPodsByName(t, ctx, kubeClient, ms.Name) {
				if before[name].role == updatedRole {
					assert.NotEqual(t, before[name].uid, pod.UID, "pod %s should have been recreated", name)
					assert.Nil(t, inplace.StateOf(&pod), "a recreated pod has no in-place state")
				}
			}
		})
	}
}

func utilsWaitReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, msName string) {
	t.Helper()
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, msName)
}
