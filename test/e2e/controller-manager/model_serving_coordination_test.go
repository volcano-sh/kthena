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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	controllerutils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const coordinationReadyGate corev1.PodConditionType = "e2e.kthena.io/coordinated-ready"

func TestModelServingCoordinatedDependencyStartup(t *testing.T) {
	r := newCoordinatedE2E(t, "test-coordinated-dependency", 2, 2, 1, "100%", true)
	r.start()
	r.wait("dependency can start but caller cannot", 0, 1, 0, 0)
	r.release("decode", true)
	r.waitEntryOnly("decode")
	r.hold("entry readiness alone must not start prefill", 0, 1, 0, 0)

	r.release("decode", false)
	r.wait("complete decode readiness admits prefill", 1, 1, 0, 1)
	// The last old decode still serves the old prefill. Its local U1 budget is
	// available, but coordination must retain that old dependency path.
	r.hold("retain old decode while old prefill remains", 1, 1, 0, 1)
	r.finish()
}

func TestModelServingCoordinatedProportionalProgress(t *testing.T) {
	r := newCoordinatedE2E(t, "test-coordinated-proportional", 4, 2, 2, "25%", false)
	r.start()
	r.wait("ceil rounding initially permits one target per Role", 1, 1, 0, 0)
	r.release("prefill", false)
	r.wait("fast Role is Ready but slowest progress is still zero", 1, 1, 1, 0)
	r.hold("Ready fast capacity cannot bypass slowest progress", 1, 1, 1, 0)

	r.release("decode", false)
	// Ready progress is 1/4 and 1/2. The 25% skew permits cumulative starts
	// ceil((25%+25%)*4)=2 and ceil((25%+25%)*2)=1, not equal counts.
	r.wait("normalized progress admits 2/4 and 1/2 starts", 2, 1, 1, 1)
	r.release("prefill", true)
	r.waitEntryOnly("prefill")
	r.hold("an unready worker must not advance proportional progress", 2, 1, 1, 1)

	r.release("prefill", false)
	r.wait("50% Ready progress permits 3/4 and 2/2 starts", 3, 2, 2, 1)
	r.finish()
}

type coordinatedE2E struct {
	t        *testing.T
	ctx      context.Context
	client   *clientset.Clientset
	kube     *kubernetes.Clientset
	ms       *workload.ModelServing
	revision string
	replicas map[string]int
	original map[string]types.UID
	expected int
}

type coordinatedSnapshot struct {
	targets map[string][]corev1.Pod
	started map[string]int
	ready   map[string]int
}

func newCoordinatedE2E(t *testing.T, name string, prefill, decode, prefillUnavailable int32, skew string, dependency bool) *coordinatedE2E {
	t.Helper()
	ctx, client, kube := setupControllerManagerE2ETest(t)
	ms := createBasicModelServing(name, 1, 0, createRole("prefill", prefill, 1), createRole("decode", decode, 1))
	ms.Spec.SchedulerName = "volcano"
	ms.Spec.RecoveryPolicy = workload.NoneRestartPolicy
	coordination := &workload.RoleCoordination{MaxSkew: ptr.To(intstr.FromString(skew))}
	if dependency {
		coordination.Dependencies = []workload.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}
	}
	ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: workload.RoleRollingUpdate, RoleCoordination: coordination}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		unavailable := int32(1)
		if role.Name == "prefill" {
			unavailable = prefillUnavailable
		}
		role.MaxUnavailable = ptr.To(intstr.FromInt32(unavailable))
		role.MaxSurge = ptr.To(intstr.FromInt(0))
		for _, template := range []*workload.PodTemplateSpec{&role.EntryTemplate, role.WorkerTemplate} {
			template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
			template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ROLLOUT_VERSION", Value: "A"}}
		}
	}
	createAndWaitForModelServing(t, ctx, client, ms)
	ms, err := client.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	r := &coordinatedE2E{t: t, ctx: ctx, client: client, kube: kube, ms: ms,
		replicas: map[string]int{"prefill": int(prefill), "decode": int(decode)},
		original: map[string]types.UID{}, expected: int(prefill+decode) * 2}
	require.Eventually(t, func() bool {
		pods, err := r.pods()
		if err != nil || len(pods) != r.expected {
			return false
		}
		for _, pod := range pods {
			if pod.DeletionTimestamp != nil || !utils.IsPodReady(pod) || !controllerutils.IsOwnedByModelServingWithUID(&pod, ms.UID) {
				return false
			}
			r.original[pod.Name] = pod.UID
		}
		return len(r.original) == r.expected
	}, 3*time.Minute, time.Second, "Initial Roles must have complete Ready entry/worker capacity")
	return r
}

func (r *coordinatedE2E) pods() ([]corev1.Pod, error) {
	pods, err := r.kube.CoreV1().Pods(testNamespace).List(r.ctx, metav1.ListOptions{LabelSelector: modelServingLabelSelector(r.ms.Name)})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (r *coordinatedE2E) start() {
	r.t.Helper()
	updateModelServingWithRetry(r.t, r.ctx, r.client, r.ms.Name, func(ms *workload.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			role := &ms.Spec.Template.Roles[i]
			for _, template := range []*workload.PodTemplateSpec{&role.EntryTemplate, role.WorkerTemplate} {
				template.Spec.Containers[0].Env[0].Value = "B"
				template.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: coordinationReadyGate}}
			}
		}
		r.revision = controllerutils.ModelServingRevision(ms)
	})
	require.NotEqual(r.t, controllerutils.ModelServingRevision(r.ms), r.revision)
}

// A stable snapshot requires every formal instance's entry and worker. This
// prevents a missing worker or a terminating old Pod from looking like progress.
func (r *coordinatedE2E) snapshot() (*coordinatedSnapshot, error) {
	pods, err := r.pods()
	if err != nil {
		return nil, err
	}
	if len(pods) != r.expected {
		return nil, fmt.Errorf("Pod count %d, want %d", len(pods), r.expected)
	}
	s := &coordinatedSnapshot{targets: map[string][]corev1.Pod{}, started: map[string]int{}, ready: map[string]int{}}
	byInstance := map[string][]corev1.Pod{}
	for _, pod := range pods {
		if _, exists := r.original[pod.Name]; !exists || pod.DeletionTimestamp != nil || !controllerutils.IsOwnedByModelServingWithUID(&pod, r.ms.UID) {
			return nil, fmt.Errorf("unexpected, deleting or foreign Pod %s", pod.Name)
		}
		byInstance[pod.Labels[workload.RoleIDKey]] = append(byInstance[pod.Labels[workload.RoleIDKey]], pod)
	}
	for id, members := range byInstance {
		if len(members) != 2 {
			return nil, fmt.Errorf("instance %s has %d members, want entry and worker", id, len(members))
		}
		role := members[0].Labels[workload.RoleLabelKey]
		if members[0].Labels[workload.RevisionLabelKey] != r.revision {
			for _, pod := range members {
				if pod.UID != r.original[pod.Name] || !utils.IsPodReady(pod) {
					return nil, fmt.Errorf("old Pod %s lost identity or availability", pod.Name)
				}
			}
			continue
		}
		s.started[role]++
		ready := true
		for _, pod := range members {
			if pod.Labels[workload.RevisionLabelKey] != r.revision || pod.Status.Phase != corev1.PodRunning || pod.UID == r.original[pod.Name] {
				return nil, fmt.Errorf("target instance %s has incomplete membership", id)
			}
			for _, container := range pod.Spec.Containers {
				versionB := false
				for _, env := range container.Env {
					versionB = versionB || (env.Name == "ROLLOUT_VERSION" && env.Value == "B")
				}
				if container.Image != nginxImage || !versionB {
					return nil, fmt.Errorf("target Pod %s does not contain template B", pod.Name)
				}
			}
			s.targets[role] = append(s.targets[role], pod)
			ready = ready && utils.IsPodReady(pod)
		}
		if ready {
			s.ready[role]++
		}
	}
	return s, nil
}

func (r *coordinatedE2E) matches(s *coordinatedSnapshot, prefill, decode, prefillReady, decodeReady int) bool {
	return s.started["prefill"] == prefill && s.started["decode"] == decode &&
		s.ready["prefill"] == prefillReady && s.ready["decode"] == decodeReady
}

func (r *coordinatedE2E) wait(message string, prefill, decode, prefillReady, decodeReady int) {
	r.t.Helper()
	require.Eventually(r.t, func() bool {
		s, err := r.snapshot()
		return err == nil && r.matches(s, prefill, decode, prefillReady, decodeReady)
	}, 3*time.Minute, time.Second, message)
	r.t.Logf("%s: started=%d/%d, Ready=%d/%d", message, prefill, decode, prefillReady, decodeReady)
}

func (r *coordinatedE2E) hold(message string, prefill, decode, prefillReady, decodeReady int) {
	r.t.Helper()
	// A short quiet window alone can pass while reconciliation is still busy.
	// Advance a non-template field and wait for two acknowledged generations:
	// even if an in-flight sync acknowledges the first one, the next sync must
	// have started after the readiness change. Budgets and revision stay fixed.
	for range 2 {
		var generation int64
		updateModelServingWithRetry(r.t, r.ctx, r.client, r.ms.Name, func(ms *workload.ModelServing) {
			ms.Spec.RevisionHistoryLimit = ptr.To(ptr.Deref(ms.Spec.RevisionHistoryLimit, 10) + 1)
			generation = ms.Generation + 1
			require.Equal(r.t, r.revision, controllerutils.ModelServingRevision(ms))
		})
		err := wait.PollUntilContextTimeout(r.ctx, time.Second, 3*time.Minute, true, func(context.Context) (bool, error) {
			s, err := r.snapshot()
			if err != nil {
				return false, fmt.Errorf("%s: %w", message, err)
			}
			if !r.matches(s, prefill, decode, prefillReady, decodeReady) {
				return false, fmt.Errorf("%s: unexpected starts %v, Ready %v", message, s.started, s.ready)
			}
			ms, err := r.client.WorkloadV1alpha1().ModelServings(testNamespace).Get(r.ctx, r.ms.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return ms.Status.ObservedGeneration >= generation, nil
		})
		require.NoError(r.t, err, "Controller must reconcile the held checkpoint: %s", message)
	}
	require.Never(r.t, func() bool {
		s, err := r.snapshot()
		return err != nil || !r.matches(s, prefill, decode, prefillReady, decodeReady)
	}, 10*time.Second, time.Second, message)
}

func (r *coordinatedE2E) release(role string, entryOnly bool) {
	r.t.Helper()
	s, err := r.snapshot()
	require.NoError(r.t, err)
	require.NotEmpty(r.t, s.targets[role])
	for _, pod := range s.targets[role] {
		if !entryOnly || pod.Labels[workload.EntryLabelKey] == controllerutils.Entry {
			require.NoError(r.t, r.releasePod(pod), "Release readiness for %s", pod.Name)
		}
	}
}

func (r *coordinatedE2E) releasePod(pod corev1.Pod) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := r.kube.CoreV1().Pods(testNamespace).Get(r.ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.UID != pod.UID {
			return fmt.Errorf("Pod %s changed UID before releasing readiness", pod.Name)
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == coordinationReadyGate && condition.Status == corev1.ConditionTrue {
				return nil
			}
		}
		patch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]string{"resourceVersion": current.ResourceVersion},
			"status": map[string]interface{}{"conditions": []corev1.PodCondition{{
				Type: coordinationReadyGate, Status: corev1.ConditionTrue, Reason: "E2EReady", LastTransitionTime: metav1.Now(),
			}}},
		})
		if err != nil {
			return err
		}
		_, err = r.kube.CoreV1().Pods(testNamespace).Patch(r.ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
		return err
	})
}

func (r *coordinatedE2E) waitEntryOnly(role string) {
	r.t.Helper()
	require.Eventually(r.t, func() bool {
		s, err := r.snapshot()
		if err != nil {
			return false
		}
		for _, pod := range s.targets[role] {
			if pod.Labels[workload.EntryLabelKey] != controllerutils.Entry && !utils.IsPodReady(pod) {
				entry := controllerutils.GeneratePodName(pod.Labels[workload.GroupNameLabelKey], pod.Labels[workload.RoleIDKey], 0)
				for _, candidate := range s.targets[role] {
					if candidate.Name == entry && utils.IsPodReady(candidate) {
						return true
					}
				}
			}
		}
		return false
	}, time.Minute, time.Second, "Target entry must be Ready while its worker is NotReady")
}

func (r *coordinatedE2E) finish() {
	r.t.Helper()
	err := wait.PollUntilContextTimeout(r.ctx, time.Second, 3*time.Minute, true, func(context.Context) (bool, error) {
		s, err := r.snapshot()
		if err != nil {
			return false, nil
		}
		for _, pods := range s.targets {
			for _, pod := range pods {
				if err := r.releasePod(pod); err != nil {
					return false, err
				}
			}
		}
		ms, err := r.client.WorkloadV1alpha1().ModelServings(testNamespace).Get(r.ctx, r.ms.Name, metav1.GetOptions{})
		if err != nil || ms.Status.ObservedGeneration != ms.Generation || ms.Status.CurrentRevision != r.revision || ms.Status.UpdateRevision != r.revision ||
			ms.Status.Replicas != 1 || ms.Status.AvailableReplicas != 1 || ms.Status.UpdatedReplicas != 1 {
			return false, nil
		}
		return r.matches(s, r.replicas["prefill"], r.replicas["decode"], r.replicas["prefill"], r.replicas["decode"]), nil
	})
	require.NoError(r.t, err, "Coordinated rollout must converge with all formal entry and worker Pods Ready")
}
