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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const (
	bootstrapReadyGate        corev1.PodConditionType = "e2e.kthena.io/bootstrap-ready"
	bootstrapE2EServerAddress string                  = "modelexpress.mx:8001"
	bootstrapE2EReadyURL      string                  = "http://127.0.0.1:8000/health"
)

// TestModelServingBootstrapAcceleration verifies end-to-end ModelExpress bootstrap acceleration:
//  1. Cold start admits only the initial seed ServingGroup while out-of-scope roles remain ungated.
//  2. Managed ModelExpress environment variables (including per-position MX_MODEL_REVISION,
//     MX_SERVER_ADDRESS, MX_POOL_REG, and MX_VMM_ARENA) are injected only into in-scope engine containers.
//  3. Entry-only readiness on a multi-Pod role replica does not release the remaining groups.
//  4. Full seed replica readiness releases all remaining ServingGroups and emits BootstrapBatchCreated events.
func TestModelServingBootstrapAcceleration(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	prefill := createRole("prefill", 1, 1)
	for _, template := range []*workload.PodTemplateSpec{&prefill.EntryTemplate, prefill.WorkerTemplate} {
		template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
		template.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: bootstrapReadyGate}}
	}
	decode := createRole("decode", 1, 0)
	decode.EntryTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))

	ms := createBasicModelServing("test-bootstrap-acceleration", 3, 0, prefill, decode)
	ms.Spec.SchedulerName = "volcano"
	ms.Spec.RecoveryPolicy = workload.NoneRestartPolicy
	ms.Spec.BootstrapAccelerateStrategy = &workload.BootstrapAccelerateStrategy{
		Roles: []workload.BootstrapRole{
			{Name: "prefill", InitialSeedReplicas: ptr.To(intstr.FromInt32(1))},
		},
		ModelExpress: &workload.ModelExpressConfig{
			EngineContainers: []string{"test-container", "worker-container"},
			ReadyURL:         bootstrapE2EReadyURL,
		},
	}

	labelSelector := modelServingLabelSelector(ms.Name)
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing with bootstrapAccelerateStrategy")
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, ms.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete ModelServing %s during cleanup: %v", ms.Name, err)
		}
		waitForPodsGone(t, cleanupCtx, kubeClient, labelSelector, 2*time.Minute)
	})

	seedGroup := ms.Name + "-0"
	seedEntryName := seedGroup + "-prefill-0-0"
	seedWorkerName := seedGroup + "-prefill-0-1"
	seedDecodeName := seedGroup + "-decode-0-0"

	// Phase 1: Wait for the seed ServingGroup (1 prefill entry + 1 prefill worker + 1 decode entry = 3 Pods).
	t.Log("Phase 1: Waiting for seed ServingGroup Pods to start")
	var seedPods map[string]corev1.Pod
	require.Eventually(t, func() bool {
		pods, listErr := listActiveModelServingPods(ctx, kubeClient, labelSelector)
		if listErr != nil || len(pods) != 3 {
			return false
		}
		byName := make(map[string]corev1.Pod, len(pods))
		for _, pod := range pods {
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			byName[pod.Name] = pod
		}
		_, hasEntry := byName[seedEntryName]
		_, hasWorker := byName[seedWorkerName]
		decodePod, hasDecode := byName[seedDecodeName]
		if !hasEntry || !hasWorker || !hasDecode || !utils.IsPodReady(decodePod) {
			return false
		}
		seedPods = byName
		return true
	}, 2*time.Minute, time.Second, "Seed ServingGroup %s did not start expected 3 Pods", seedGroup)

	// Verify the controller holds at 1 ServingGroup across reconciles while the seed replica is NotReady.
	assertBootstrapHeldAtPodCount(t, ctx, kthenaClient, kubeClient, ms.Name, labelSelector, 3,
		"Remaining ServingGroups must stay deferred while seed replica is NotReady")

	// Verify injected ModelExpress environment variables on entry, worker, and out-of-scope decode Pods.
	entryPod := seedPods[seedEntryName]
	workerPod := seedPods[seedWorkerName]
	decodePod := seedPods[seedDecodeName]
	roleHash := entryPod.Labels[workload.RoleTemplateHashLabelKey]
	require.NotEmpty(t, roleHash, "Entry Pod must carry RoleTemplateHashLabelKey")
	assert.Equal(t, roleHash, workerPod.Labels[workload.RoleTemplateHashLabelKey])

	assertModelExpressContainerEnv(t, entryPod.Spec.Containers[0], roleHash+"-0")
	assertModelExpressContainerEnv(t, workerPod.Spec.Containers[0], roleHash+"-1")
	for _, env := range decodePod.Spec.Containers[0].Env {
		assert.False(t, strings.HasPrefix(env.Name, "MX_"), "Out-of-scope decode Pod should not have %s injected", env.Name)
	}

	// Phase 2: Release only the entry Pod readiness gate; worker remains NotReady so seed is still incomplete.
	t.Log("Phase 2: Releasing entry Pod readiness gate only")
	require.NoError(t, releaseBootstrapPodGate(ctx, kubeClient, entryPod))
	require.Eventually(t, func() bool {
		pod, getErr := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, seedEntryName, metav1.GetOptions{})
		return getErr == nil && utils.IsPodReady(*pod)
	}, time.Minute, time.Second, "Seed entry Pod should become Ready after gate release")

	assertBootstrapHeldAtPodCount(t, ctx, kthenaClient, kubeClient, ms.Name, labelSelector, 3,
		"Entry readiness alone must not release remaining ServingGroups while worker is NotReady")

	// Phase 3: Release the worker Pod readiness gate; full seed readiness releases both remaining ServingGroups.
	t.Log("Phase 3: Releasing worker Pod readiness gate to complete seed replica")
	require.NoError(t, releaseBootstrapPodGate(ctx, kubeClient, workerPod))

	require.Eventually(t, func() bool {
		pods, listErr := listActiveModelServingPods(ctx, kubeClient, labelSelector)
		if listErr != nil || len(pods) != 9 {
			return false
		}
		for _, pod := range pods {
			if pod.Labels[workload.RoleLabelKey] == "prefill" {
				_ = releaseBootstrapPodGate(ctx, kubeClient, pod)
			}
		}
		return true
	}, 2*time.Minute, time.Second, "All 3 ServingGroups (9 Pods) should be created once the seed replica is Ready")

	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)

	currentMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		events, listErr := kubeClient.CoreV1().Events(testNamespace).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			return false
		}
		for _, ev := range events.Items {
			if ev.InvolvedObject.Kind == "ModelServing" && ev.InvolvedObject.UID == currentMS.UID && ev.Reason == "BootstrapBatchCreated" {
				return true
			}
		}
		return false
	}, time.Minute, time.Second, "Expected BootstrapBatchCreated event on ModelServing")
}

// TestModelServingBootstrapGangCoreAndScaleUp verifies bootstrap acceleration with Volcano gang scheduling:
//  1. When initialSeedReplicas (1) is below gangPolicy.minRoleReplicas (2), the seed budget is raised
//     to the gang minimum so Volcano schedules the 2-replica gang core while non-core replicas wait.
//  2. Readiness of a single core replica releases the remaining intra-ServingGroup replicas via scaleUpRoles.
//  3. Scaling up spec.replicas on a warm fleet with Ready sources admits the entire new ServingGroup at once.
func TestModelServingBootstrapGangCoreAndScaleUp(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	prefill := createEntryOnlyBootstrapRole("prefill", 4)

	ms := createBasicModelServing("test-bootstrap-gang-scaleup", 1, 0, prefill)
	ms.Spec.SchedulerName = "volcano"
	ms.Spec.RecoveryPolicy = workload.NoneRestartPolicy
	ms.Spec.Template.GangPolicy = &workload.GangPolicy{
		MinRoleReplicas: map[string]int32{"prefill": 2},
	}
	ms.Spec.BootstrapAccelerateStrategy = &workload.BootstrapAccelerateStrategy{
		Roles: []workload.BootstrapRole{
			{Name: "prefill", InitialSeedReplicas: ptr.To(intstr.FromInt32(1))},
		},
		ModelExpress: &workload.ModelExpressConfig{
			EngineContainers: []string{"test-container"},
			ReadyURL:         bootstrapE2EReadyURL,
		},
	}

	labelSelector := createBootstrapModelServingWithCleanup(t, ctx, kthenaClient, kubeClient, ms)
	group0 := ms.Name + "-0"
	group1 := ms.Name + "-1"

	// Phase 1: Verify only the 2-replica gang core (prefill-0, prefill-1) is created and scheduled by Volcano.
	t.Log("Phase 1: Waiting for 2-replica Volcano gang core to start while prefill-2 and prefill-3 remain deferred")
	corePods := waitForRunningBootstrapPodsByName(t, ctx, kubeClient, labelSelector,
		[]string{group0 + "-prefill-0-0", group0 + "-prefill-1-0"})
	for _, pod := range corePods {
		assert.NotEmpty(t, pod.Annotations["scheduling.k8s.io/group-name"],
			"Pod %s should be annotated with Volcano PodGroup", pod.Name)
	}
	assertBootstrapHeldAtPodCount(t, ctx, kthenaClient, kubeClient, ms.Name, labelSelector, 2,
		"Non-core replicas prefill-2 and prefill-3 must stay deferred while gang core is NotReady")

	// Phase 2: Release only prefill-0 (leaving prefill-1 NotReady); 1 Ready source releases prefill-2 and prefill-3.
	t.Log("Phase 2: Releasing prefill-0 gate only to unlock remaining intra-ServingGroup replicas")
	require.NoError(t, releaseBootstrapPodGate(ctx, kubeClient, corePods[group0+"-prefill-0-0"]))
	waitForActiveBootstrapPodsAndReleaseGates(t, ctx, kubeClient, labelSelector, 4)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)

	// Phase 3: Scale spec.replicas from 1 -> 2; warm sources admit all 4 replicas of group-1 in one batch.
	t.Log("Phase 3: Scaling spec.replicas 1 -> 2 with warm sources and verifying all 4 replicas of group-1 start without staging")
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.Replicas = ptr.To(int32(2))
	})
	waitForRunningBootstrapPodsByName(t, ctx, kubeClient, labelSelector, []string{
		group0 + "-prefill-0-0", group0 + "-prefill-1-0", group0 + "-prefill-2-0", group0 + "-prefill-3-0",
		group1 + "-prefill-0-0", group1 + "-prefill-1-0", group1 + "-prefill-2-0", group1 + "-prefill-3-0",
	})
	waitForActiveBootstrapPodsAndReleaseGates(t, ctx, kubeClient, labelSelector, 8)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)
}

// TestModelServingBootstrapRollingUpdateAndInPlaceFallback verifies rolling update and revision behavior:
//  1. Editing spec.bootstrapAccelerateStrategy on a Ready fleet is revision-neutral (no revision bump or Pod restarts).
//  2. With updateMethod: InPlace, bootstrap-accelerated roles fall back to recreating Pods because
//     MX_MODEL_REVISION is bound to the role template hash and cannot be patched in place.
//  3. A template update starts a new source pool, staging the first v2 replica as a seed despite Ready v1 replicas
//     and maxUnavailable allowing full replacement, then releasing the rest once the v2 seed is Ready.
func TestModelServingBootstrapRollingUpdateAndInPlaceFallback(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	prefill := createEntryOnlyBootstrapRole("prefill", 1)

	ms := createBasicModelServing("test-bootstrap-rollout-inplace", 3, 0, prefill)
	ms.Spec.RecoveryPolicy = workload.RoleRecreate
	ms.Spec.RolloutStrategy = &workload.RolloutStrategy{
		Type:         workload.ServingGroupRollingUpdate,
		UpdateMethod: workload.InPlaceUpdateMethod,
		RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{
			MaxUnavailable: ptr.To(intstr.FromInt32(3)),
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
		},
	}
	ms.Spec.BootstrapAccelerateStrategy = &workload.BootstrapAccelerateStrategy{
		Roles: []workload.BootstrapRole{
			{Name: "prefill", InitialSeedReplicas: ptr.To(intstr.FromInt32(1))},
		},
		ModelExpress: &workload.ModelExpressConfig{
			EngineContainers: []string{"test-container"},
			ReadyURL:         bootstrapE2EReadyURL,
		},
	}

	labelSelector := createBootstrapModelServingWithCleanup(t, ctx, kthenaClient, kubeClient, ms)

	// Phase 1: Bring v1 fleet Ready (1 seed -> 3 groups).
	t.Log("Phase 1: Converging initial v1 fleet with bootstrap acceleration")
	seedV1 := waitForRunningBootstrapPodsByName(t, ctx, kubeClient, labelSelector, []string{ms.Name + "-0-prefill-0-0"})
	v1RoleHash := seedV1[ms.Name+"-0-prefill-0-0"].Labels[workload.RoleTemplateHashLabelKey]
	require.NotEmpty(t, v1RoleHash)
	require.NoError(t, releaseBootstrapPodGate(ctx, kubeClient, seedV1[ms.Name+"-0-prefill-0-0"]))
	waitForActiveBootstrapPodsAndReleaseGates(t, ctx, kubeClient, labelSelector, 3)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)

	v1MS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	v1Revision := v1MS.Status.CurrentRevision
	require.NotEmpty(t, v1Revision)
	v1Snapshots := snapshotPods(t, ctx, kubeClient, ms.Name)
	require.Len(t, v1Snapshots, 3)

	// Phase 2: Mutate spec.bootstrapAccelerateStrategy and verify revision neutrality (zero Pod restarts).
	t.Log("Phase 2: Editing bootstrapAccelerateStrategy and verifying revision neutrality")
	var editedGeneration int64
	updatedReadyURL := "http://127.0.0.1:8000/readyz"
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromInt32(2))
		current.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = updatedReadyURL
		editedGeneration = current.Generation + 1
	})
	require.Eventually(t, func() bool {
		current, getErr := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
		return getErr == nil && current.Status.ObservedGeneration >= editedGeneration
	}, time.Minute, time.Second, "Controller did not observe strategy edit")

	afterEditMS, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, v1Revision, afterEditMS.Status.CurrentRevision, "Strategy edit must not change CurrentRevision")
	assert.Equal(t, v1Revision, afterEditMS.Status.UpdateRevision, "Strategy edit must not change UpdateRevision")
	assert.Equal(t, v1Snapshots, snapshotPods(t, ctx, kubeClient, ms.Name), "Strategy edit must not recreate any Pod")

	// Reset seed budget to 1 before triggering the image update.
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromInt32(1))
	})

	// Phase 3: Update container image; verify InPlace fallback to recreate and 1-seed staging on the new pool.
	t.Log("Phase 3: Updating image under updateMethod=InPlace and verifying recreate fallback + v2 seed staging")
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
	})

	var v2SeedPod corev1.Pod
	require.Eventually(t, func() bool {
		pods, listErr := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if listErr != nil || len(pods.Items) != 1 {
			return false
		}
		pod := pods.Items[0]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning ||
			pod.Spec.Containers[0].Image != nginxAlpineImage {
			return false
		}
		v2SeedPod = pod
		return true
	}, 2*time.Minute, time.Second, "Expected outdated v1 Pods to be replaced by a single v2 seed Pod")

	assertBootstrapHeldAtPodCount(t, ctx, kthenaClient, kubeClient, ms.Name, labelSelector, 1,
		"Remaining v2 ServingGroups must wait until the v2 seed replica becomes Ready")

	assert.NotEqual(t, v1Snapshots[v2SeedPod.Name].uid, v2SeedPod.UID,
		"Pod %s should be recreated rather than updated in place", v2SeedPod.Name)
	assert.Nil(t, inplace.StateOf(&v2SeedPod), "Recreated Pod must not carry in-place update state")
	assert.False(t, inplace.InProgress(&v2SeedPod), "Recreated Pod must not be marked in-place updating")

	v2RoleHash := v2SeedPod.Labels[workload.RoleTemplateHashLabelKey]
	require.NotEmpty(t, v2RoleHash)
	assert.NotEqual(t, v1RoleHash, v2RoleHash, "Image change must produce a new RoleTemplateHash pool")
	assertModelExpressContainerEnvWithReadyURL(t, v2SeedPod.Spec.Containers[0], v2RoleHash+"-0", updatedReadyURL)

	// Phase 4: Release v2 seed gate; remaining 2 v2 ServingGroups are created and converge Ready.
	t.Log("Phase 4: Releasing v2 seed gate to complete rolling update")
	require.NoError(t, releaseBootstrapPodGate(ctx, kubeClient, v2SeedPod))
	v2Pods := waitForActiveBootstrapPodsAndReleaseGates(t, ctx, kubeClient, labelSelector, 3)
	for _, pod := range v2Pods {
		assert.Equal(t, nginxAlpineImage, pod.Spec.Containers[0].Image)
		assertModelExpressContainerEnvWithReadyURL(t, pod.Spec.Containers[0], v2RoleHash+"-0", updatedReadyURL)
	}
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)
}

// TestModelServingBootstrapPercentageSeedAndDynamicDisable verifies percentage seeds, user env overrides,
// non-engine container isolation, and live removal of spec.bootstrapAccelerateStrategy:
//  1. initialSeedReplicas: "34%" on 3 replicas rounds up to 2 seed ServingGroups while group-2 is deferred.
//  2. User-provided MX_* env vars take precedence on engine containers while init and sidecar containers stay untouched.
//  3. Clearing spec.bootstrapAccelerateStrategy while seeds are still NotReady immediately admits deferred
//     groups without MX injection and without restarting existing seed Pods.
func TestModelServingBootstrapPercentageSeedAndDynamicDisable(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	prefill := createEntryOnlyBootstrapRole("prefill", 1)
	prefill.EntryTemplate.Spec.InitContainers = []corev1.Container{{
		Name:    "init-probe",
		Image:   nginxImage,
		Command: []string{"sh", "-c", "true"},
	}}
	prefill.EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: plugins.EnvNIXLBackend, Value: "LIBFABRIC"},
	}
	prefill.EntryTemplate.Spec.Containers = append(prefill.EntryTemplate.Spec.Containers, corev1.Container{
		Name:    "sidecar-container",
		Image:   nginxAlpineImage,
		Command: []string{"sh", "-c", "sleep 3600"},
	})

	ms := createBasicModelServing("test-bootstrap-pct-disable", 3, 0, prefill)
	ms.Spec.RecoveryPolicy = workload.NoneRestartPolicy
	ms.Spec.BootstrapAccelerateStrategy = &workload.BootstrapAccelerateStrategy{
		Roles: []workload.BootstrapRole{
			{Name: "prefill", InitialSeedReplicas: ptr.To(intstr.FromString("34%"))},
		},
		ModelExpress: &workload.ModelExpressConfig{
			EngineContainers: []string{"test-container"},
			ReadyURL:         bootstrapE2EReadyURL,
		},
	}

	labelSelector := createBootstrapModelServingWithCleanup(t, ctx, kthenaClient, kubeClient, ms)
	seed0Name := ms.Name + "-0-prefill-0-0"
	seed1Name := ms.Name + "-1-prefill-0-0"
	deferredName := ms.Name + "-2-prefill-0-0"

	// Phase 1: ceil(3 * 34%) = 2 seed ServingGroups start; group-2 remains deferred.
	t.Log("Phase 1: Verifying 34% initialSeedReplicas admits 2 seed groups and preserves user env overrides")
	seedPods := waitForRunningBootstrapPodsByName(t, ctx, kubeClient, labelSelector, []string{seed0Name, seed1Name})
	assertBootstrapHeldAtPodCount(t, ctx, kthenaClient, kubeClient, ms.Name, labelSelector, 2,
		"Third ServingGroup must remain deferred while both percentage-seed groups are NotReady")

	seed0 := seedPods[seed0Name]
	roleHash := seed0.Labels[workload.RoleTemplateHashLabelKey]
	require.NotEmpty(t, roleHash)
	engineEnv := containerEnvMap(seed0.Spec.Containers[0])
	assert.Equal(t, "LIBFABRIC", engineEnv[plugins.EnvNIXLBackend].Value, "User-defined MX env var must take precedence")
	assert.Equal(t, roleHash+"-0", engineEnv[plugins.EnvModelRevision].Value)
	assert.Equal(t, bootstrapE2EServerAddress, engineEnv[plugins.EnvServerAddress].Value)
	assert.Equal(t, bootstrapE2EReadyURL, engineEnv[plugins.EnvArtifactReadyURL].Value)
	for _, env := range seed0.Spec.InitContainers[0].Env {
		assert.False(t, strings.HasPrefix(env.Name, "MX_"), "Init container must not receive %s", env.Name)
	}
	for _, env := range seed0.Spec.Containers[1].Env {
		assert.False(t, strings.HasPrefix(env.Name, "MX_"), "Sidecar container must not receive %s", env.Name)
	}

	// Phase 2: Remove bootstrapAccelerateStrategy while seeds are still NotReady; group-2 starts immediately.
	t.Log("Phase 2: Clearing bootstrapAccelerateStrategy and verifying deferred group starts without MX injection")
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.BootstrapAccelerateStrategy = nil
	})

	allPods := waitForRunningBootstrapPodsByName(t, ctx, kubeClient, labelSelector, []string{seed0Name, seed1Name, deferredName})
	assert.Equal(t, seedPods[seed0Name].UID, allPods[seed0Name].UID, "Disabling strategy must not restart seed-0")
	assert.Equal(t, seedPods[seed1Name].UID, allPods[seed1Name].UID, "Disabling strategy must not restart seed-1")

	deferredEnv := containerEnvMap(allPods[deferredName].Spec.Containers[0])
	assert.Equal(t, "LIBFABRIC", deferredEnv[plugins.EnvNIXLBackend].Value, "Template env var remains present")
	_, hasModelRev := deferredEnv[plugins.EnvModelRevision]
	_, hasServerAddr := deferredEnv[plugins.EnvServerAddress]
	assert.False(t, hasModelRev, "Pod created after strategy removal must not have MX_MODEL_REVISION")
	assert.False(t, hasServerAddr, "Pod created after strategy removal must not have MX_SERVER_ADDRESS")

	waitForActiveBootstrapPodsAndReleaseGates(t, ctx, kubeClient, labelSelector, 3)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)
}

func createEntryOnlyBootstrapRole(name string, roleReplicas int32) workload.Role {
	role := createRole(name, roleReplicas, 0)
	role.WorkerTemplate = nil
	role.EntryTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
	role.EntryTemplate.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: bootstrapReadyGate}}
	return role
}

func createBootstrapModelServingWithCleanup(
	t *testing.T,
	ctx context.Context,
	kthenaClient *clientset.Clientset,
	kubeClient *kubernetes.Clientset,
	ms *workload.ModelServing,
) string {
	t.Helper()
	labelSelector := modelServingLabelSelector(ms.Name)
	_, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create ModelServing %s", ms.Name)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, ms.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete ModelServing %s during cleanup: %v", ms.Name, err)
		}
		waitForPodsGone(t, cleanupCtx, kubeClient, labelSelector, 2*time.Minute)
	})
	return labelSelector
}

func waitForRunningBootstrapPodsByName(
	t *testing.T,
	ctx context.Context,
	kubeClient *kubernetes.Clientset,
	labelSelector string,
	expectedNames []string,
) map[string]corev1.Pod {
	t.Helper()
	var byName map[string]corev1.Pod
	require.Eventually(t, func() bool {
		pods, err := listActiveModelServingPods(ctx, kubeClient, labelSelector)
		if err != nil || len(pods) != len(expectedNames) {
			return false
		}
		candidate := make(map[string]corev1.Pod, len(pods))
		for _, pod := range pods {
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			candidate[pod.Name] = pod
		}
		for _, name := range expectedNames {
			if _, ok := candidate[name]; !ok {
				return false
			}
		}
		byName = candidate
		return true
	}, 2*time.Minute, time.Second, "Did not observe expected running Pods %v", expectedNames)
	return byName
}

func waitForActiveBootstrapPodsAndReleaseGates(
	t *testing.T,
	ctx context.Context,
	kubeClient *kubernetes.Clientset,
	labelSelector string,
	expectedCount int,
) []corev1.Pod {
	t.Helper()
	var active []corev1.Pod
	require.Eventually(t, func() bool {
		pods, err := listActiveModelServingPods(ctx, kubeClient, labelSelector)
		if err != nil || len(pods) != expectedCount {
			return false
		}
		for _, pod := range pods {
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			for _, gate := range pod.Spec.ReadinessGates {
				if gate.ConditionType == bootstrapReadyGate {
					if err := releaseBootstrapPodGate(ctx, kubeClient, pod); err != nil {
						return false
					}
				}
			}
		}
		active = pods
		return true
	}, 2*time.Minute, time.Second, "Expected %d active running Pods (selector=%s)", expectedCount, labelSelector)
	return active
}

func containerEnvMap(container corev1.Container) map[string]corev1.EnvVar {
	envByName := make(map[string]corev1.EnvVar, len(container.Env))
	for _, env := range container.Env {
		envByName[env.Name] = env
	}
	return envByName
}

func assertModelExpressContainerEnv(t *testing.T, container corev1.Container, expectedRevision string) {
	t.Helper()
	assertModelExpressContainerEnvWithReadyURL(t, container, expectedRevision, bootstrapE2EReadyURL)
}

func assertModelExpressContainerEnvWithReadyURL(t *testing.T, container corev1.Container, expectedRevision, expectedReadyURL string) {
	t.Helper()
	envByName := containerEnvMap(container)
	assert.Equal(t, expectedRevision, envByName[plugins.EnvModelRevision].Value)
	assert.Equal(t, bootstrapE2EServerAddress, envByName[plugins.EnvServerAddress].Value)
	assert.Equal(t, expectedReadyURL, envByName[plugins.EnvArtifactReadyURL].Value)
	assert.Equal(t, "1", envByName[plugins.EnvP2PMetadata].Value)
	assert.Equal(t, "1", envByName[plugins.EnvArtifactTransfer].Value)
	assert.Equal(t, "1", envByName[plugins.EnvPoolRegistration].Value)
	assert.Equal(t, "1", envByName[plugins.EnvVMMArena].Value)
	if assert.NotNil(t, envByName[plugins.EnvWorkerHost].ValueFrom) &&
		assert.NotNil(t, envByName[plugins.EnvWorkerHost].ValueFrom.FieldRef) {
		assert.Equal(t, "status.podIP", envByName[plugins.EnvWorkerHost].ValueFrom.FieldRef.FieldPath)
	}
}

func listActiveModelServingPods(ctx context.Context, kubeClient *kubernetes.Clientset, labelSelector string) ([]corev1.Pod, error) {
	podList, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	active := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.DeletionTimestamp == nil {
			active = append(active, pod)
		}
	}
	return active, nil
}

func assertBootstrapHeldAtPodCount(
	t *testing.T,
	ctx context.Context,
	kthenaClient *clientset.Clientset,
	kubeClient *kubernetes.Clientset,
	msName, labelSelector string,
	expectedPods int,
	message string,
) {
	t.Helper()
	for range 2 {
		var generation int64
		updateModelServingWithRetry(t, ctx, kthenaClient, msName, func(ms *workload.ModelServing) {
			ms.Spec.RevisionHistoryLimit = ptr.To(ptr.Deref(ms.Spec.RevisionHistoryLimit, 10) + 1)
			generation = ms.Generation + 1
		})
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(context.Context) (bool, error) {
			pods, listErr := listActiveModelServingPods(ctx, kubeClient, labelSelector)
			if listErr != nil {
				return false, listErr
			}
			if len(pods) != expectedPods {
				return false, fmt.Errorf("%s: got %d active Pods, want %d", message, len(pods), expectedPods)
			}
			ms, getErr := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, msName, metav1.GetOptions{})
			if getErr != nil {
				return false, getErr
			}
			return ms.Status.ObservedGeneration >= generation, nil
		})
		require.NoError(t, err, message)
	}
}

func releaseBootstrapPodGate(ctx context.Context, kubeClient *kubernetes.Clientset, pod corev1.Pod) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == bootstrapReadyGate && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
		patch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]string{"resourceVersion": current.ResourceVersion},
			"status": map[string]interface{}{"conditions": []corev1.PodCondition{{
				Type: bootstrapReadyGate, Status: corev1.ConditionTrue, Reason: "E2EReady", LastTransitionTime: metav1.Now(),
			}}},
		})
		if err != nil {
			return err
		}
		_, err = kubeClient.CoreV1().Pods(testNamespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
		return err
	})
}

func createValidBootstrapModelServingForWebhookTest() *workload.ModelServing {
	ms := createBasicModelServing("valid-bootstrap-ms-"+utils.RandomString(5), 2, 1)
	ms.Spec.BootstrapAccelerateStrategy = &workload.BootstrapAccelerateStrategy{
		Roles: []workload.BootstrapRole{
			{Name: "prefill", InitialSeedReplicas: ptr.To(intstr.FromInt32(1))},
		},
		ModelExpress: &workload.ModelExpressConfig{
			EngineContainers: []string{"test-container"},
			ReadyURL:         bootstrapE2EReadyURL,
		},
	}
	return ms
}

func createInvalidBootstrapModelServingUnknownRole() *workload.ModelServing {
	ms := createValidBootstrapModelServingForWebhookTest()
	ms.Name = "invalid-bootstrap-role-" + utils.RandomString(5)
	ms.Spec.BootstrapAccelerateStrategy.Roles[0].Name = "nonexistent"
	return ms
}

func createInvalidBootstrapModelServingManagedPlugin() *workload.ModelServing {
	ms := createValidBootstrapModelServingForWebhookTest()
	ms.Name = "invalid-bootstrap-plugin-" + utils.RandomString(5)
	ms.Spec.Plugins = []workload.PluginSpec{
		{Name: plugins.ModelExpressPluginName, Type: workload.PluginTypeBuiltIn},
	}
	return ms
}

func createInvalidBootstrapModelServingMissingEngineContainer() *workload.ModelServing {
	ms := createValidBootstrapModelServingForWebhookTest()
	ms.Name = "invalid-bootstrap-container-" + utils.RandomString(5)
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"missing-engine"}
	return ms
}

func createInvalidBootstrapModelServingInvalidSeed() *workload.ModelServing {
	ms := createValidBootstrapModelServingForWebhookTest()
	ms.Name = "invalid-bootstrap-seed-" + utils.RandomString(5)
	ms.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromInt32(0))
	return ms
}

func createInvalidBootstrapModelServingInvalidReadyURL() *workload.ModelServing {
	ms := createValidBootstrapModelServingForWebhookTest()
	ms.Name = "invalid-bootstrap-url-" + utils.RandomString(5)
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = "grpc://127.0.0.1:8001"
	return ms
}
