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

func assertModelExpressContainerEnv(t *testing.T, container corev1.Container, expectedRevision string) {
	t.Helper()
	envByName := make(map[string]corev1.EnvVar, len(container.Env))
	for _, env := range container.Env {
		envByName[env.Name] = env
	}
	assert.Equal(t, expectedRevision, envByName[plugins.EnvModelRevision].Value)
	assert.Equal(t, bootstrapE2EServerAddress, envByName[plugins.EnvServerAddress].Value)
	assert.Equal(t, bootstrapE2EReadyURL, envByName[plugins.EnvArtifactReadyURL].Value)
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
