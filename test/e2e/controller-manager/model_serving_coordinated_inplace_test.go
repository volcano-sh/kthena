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

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	controllerutils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// The gate is present in A, so A -> B changes only container images and must use
// InPlace. A patched instance retains its old revision labels until completion;
// those labels must not grant another target start while its worker is unready.
func TestModelServingCoordinatedInPlaceProgress(t *testing.T) {
	r := newCoordinatedInPlaceE2E(t)
	r.setHighestGates(corev1.ConditionFalse, false)
	r.poll("both highest old entry/worker pairs lose readiness", func() (bool, error) {
		s, err := r.snapshotInPlace()
		if err != nil {
			return false, err
		}
		for _, pod := range s.pods {
			if r.ordinal(pod) == 3 && utils.IsPodReady(pod) {
				return false, nil
			}
		}
		return true, nil
	})

	updateModelServingWithRetry(t, r.ctx, r.client, r.ms.Name, func(ms *workload.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			role := &ms.Spec.Template.Roles[i]
			role.EntryTemplate.Spec.Containers[0].Image = nginxAlpineImage
			role.WorkerTemplate.Spec.Containers[0].Image = nginxAlpineImage
		}
		r.revision = controllerutils.ModelServingRevision(ms)
	})
	require.NotEqual(t, r.sourceRevision, r.revision)
	r.poll("only highest instances run patched images with source labels", func() (bool, error) {
		s, err := r.snapshotInPlace()
		if err != nil {
			return false, err
		}
		if !r.held(s, false) {
			return false, nil
		}
		for _, pod := range s.pods {
			if r.ordinal(pod) != 3 {
				continue
			}
			state := inplace.StateOf(&pod)
			if state == nil || !state.InProgress() || state.Revision != r.revision || !inplace.Updating(&pod) ||
				controllerutils.ObjectRevision(&pod) != r.sourceRevision || !r.runtimeOnTarget(pod) {
				return false, nil
			}
		}
		return true, nil
	})
	r.holdInPlace("old labels of an in-flight patch must reserve the formal start", false)

	r.setHighestGates(corev1.ConditionTrue, true)
	r.poll("highest entries become Ready while required workers remain unready", func() (bool, error) {
		s, err := r.snapshotInPlace()
		return err == nil && r.held(s, true), err
	})
	r.holdInPlace("entry readiness cannot release the next coordinated patch", true)

	r.setHighestGates(corev1.ConditionTrue, false)
	r.poll("complete target instances release the next ordinal", func() (bool, error) {
		s, err := r.snapshotInPlace()
		if err != nil {
			return false, err
		}
		for _, role := range []string{"prefill", "decode"} {
			if s.started[role] < 2 {
				return false, nil
			}
			name := controllerutils.GeneratePodName(r.ms.Name+"-0", controllerutils.GenerateRoleID(role, 2), 0)
			if s.pods[name].Spec.Containers[0].Image != nginxAlpineImage {
				return false, nil
			}
		}
		return true, nil
	})

	// Later instances kept their True gates. Keep releasing any target member
	// as it appears, and verify actual runtime state, identity and status together.
	r.poll("all original entry and worker Pods converge in place", func() (bool, error) {
		s, err := r.snapshotInPlace()
		if err != nil {
			return false, err
		}
		for _, pod := range s.pods {
			if pod.Spec.Containers[0].Image == nginxAlpineImage {
				if err := r.releasePod(pod); err != nil {
					return false, err
				}
			}
		}
		if s.ready["prefill"] != 4 || s.ready["decode"] != 4 {
			return false, nil
		}
		ms, err := r.client.WorkloadV1alpha1().ModelServings(testNamespace).Get(r.ctx, r.ms.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return ms.Status.ObservedGeneration == ms.Generation && ms.Status.CurrentRevision == r.revision && ms.Status.UpdateRevision == r.revision &&
			ms.Status.Replicas == 1 && ms.Status.AvailableReplicas == 1 && ms.Status.UpdatedReplicas == 1, nil
	})
}

type coordinatedInPlaceE2E struct {
	*coordinatedE2E
	sourceRevision string
	containers     map[string]string
}

type coordinatedInPlaceSnapshot struct {
	pods    map[string]corev1.Pod
	started map[string]int
	ready   map[string]int
}

func newCoordinatedInPlaceE2E(t *testing.T) *coordinatedInPlaceE2E {
	t.Helper()
	baseCtx, client, kube := setupControllerManagerE2ETest(t)
	ctx, cancel := context.WithTimeout(baseCtx, 5*time.Minute)
	t.Cleanup(cancel)
	ms := createBasicModelServing("test-coordinated-inplace", 1, 0, createRole("prefill", 4, 1), createRole("decode", 4, 1))
	ms.Spec.SchedulerName = "volcano"
	ms.Spec.RecoveryPolicy = workload.NoneRestartPolicy
	ms.Spec.RolloutStrategy = &workload.RolloutStrategy{
		Type: workload.RoleRollingUpdate, UpdateMethod: workload.InPlaceUpdateMethod,
		RoleCoordination: &workload.RoleCoordination{MaxSkew: ptr.To(intstr.FromString("25%"))},
	}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt(3)), ptr.To(intstr.FromInt(0))
		for _, template := range []*workload.PodTemplateSpec{&role.EntryTemplate, role.WorkerTemplate} {
			template.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
			template.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: coordinationReadyGate}}
		}
	}
	waitForWebhookReady(t, ctx, client, testNamespace)
	created, err := client.WorkloadV1alpha1().ModelServings(testNamespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := client.WorkloadV1alpha1().ModelServings(testNamespace).Delete(cleanupCtx, ms.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("Cleanup ModelServing %s: %v", ms.Name, err)
		}
	})
	r := &coordinatedInPlaceE2E{coordinatedE2E: &coordinatedE2E{
		t: t, ctx: ctx, client: client, kube: kube, ms: created, expected: 16,
		replicas: map[string]int{"prefill": 4, "decode": 4}, original: map[string]types.UID{},
	}, sourceRevision: controllerutils.ModelServingRevision(created), containers: map[string]string{}}
	r.revision = r.sourceRevision
	// createAndWaitForModelServing waits for Ready before returning; a gated A
	// must instead have its initial conditions released during creation.
	var lastProgress time.Time
	r.poll("all gated A entry and worker Pods become Ready", func() (bool, error) {
		pods, err := r.pods()
		if err != nil {
			return false, err
		}
		membersReady := len(pods) == r.expected
		ready, containersReady, gatesReady, scheduled := 0, 0, 0, 0
		phases := map[corev1.PodPhase]int{}
		for _, pod := range pods {
			if !controllerutils.IsOwnedByModelServingWithUID(&pod, created.UID) || pod.DeletionTimestamp != nil {
				return false, fmt.Errorf("unexpected owner or deletion of initial Pod %s", pod.Name)
			}
			phases[pod.Status.Phase]++
			for _, condition := range pod.Status.Conditions {
				if condition.Status != corev1.ConditionTrue {
					continue
				}
				switch condition.Type {
				case corev1.PodReady:
					ready++
				case corev1.ContainersReady:
					containersReady++
				case corev1.PodScheduled:
					scheduled++
				case coordinationReadyGate:
					gatesReady++
				}
			}
			if err := r.releasePod(pod); err != nil {
				return false, err
			}
			membersReady = membersReady && utils.IsPodReady(pod) && controllerutils.ObjectRevision(&pod) == r.sourceRevision
		}
		if time.Since(lastProgress) >= 15*time.Second {
			r.t.Logf("Initial A: Pods=%d/%d, Scheduled=%d, custom gate True=%d, ContainersReady=%d, PodReady=%d, phases=%v",
				len(pods), r.expected, scheduled, gatesReady, containersReady, ready, phases)
			lastProgress = time.Now()
		}
		if !membersReady {
			return false, nil
		}
		ms, err := client.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if ms.Status.AvailableReplicas != 1 || ms.Status.CurrentRevision != r.sourceRevision || ms.Status.UpdateRevision != r.sourceRevision || ms.Status.ObservedGeneration != ms.Generation {
			return false, nil
		}
		for _, pod := range pods {
			if len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].ContainerID == "" {
				return false, nil
			}
			r.original[pod.Name] = pod.UID
			r.containers[pod.Name] = pod.Status.ContainerStatuses[0].ContainerID
		}
		return true, nil
	})
	return r
}

// All assertions in polling callbacks return errors; no callback calls Fatal.
func (r *coordinatedInPlaceE2E) poll(message string, check func() (bool, error)) {
	r.t.Helper()
	var last string
	err := wait.PollUntilContextTimeout(r.ctx, time.Second, 3*time.Minute, true, func(context.Context) (bool, error) {
		ok, err := check()
		if err != nil {
			last = err.Error()
		}
		return ok, err
	})
	if err != nil {
		r.logInPlaceDiagnostics()
	}
	require.NoError(r.t, err, "%s; last observation: %s", message, last)
	r.t.Log(message)
}

// Diagnostics run before cleanup even when the scenario context has expired.
// Full conditions distinguish the external gate from scheduling/container
// startup failures; events and container states preserve their actual reasons.
func (r *coordinatedInPlaceE2E) logInPlaceDiagnostics() {
	diagnosticCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logJSON := func(label string, value interface{}) {
		raw, err := json.Marshal(value)
		if err != nil {
			r.t.Logf("%s: cannot marshal diagnostics: %v", label, err)
			return
		}
		r.t.Logf("%s: %s", label, raw)
	}
	pods, err := r.kube.CoreV1().Pods(testNamespace).List(diagnosticCtx, metav1.ListOptions{LabelSelector: modelServingLabelSelector(r.ms.Name)})
	names := map[string]bool{r.ms.Name: true, r.ms.Name + "-0": true}
	if err != nil {
		r.t.Logf("List diagnostic Pods: %v", err)
	} else {
		for _, pod := range pods.Items {
			names[pod.Name] = true
			logJSON("Pod "+pod.Name, map[string]interface{}{
				"uid": pod.UID, "resourceVersion": pod.ResourceVersion, "created": pod.CreationTimestamp,
				"deleting": pod.DeletionTimestamp, "labels": pod.Labels, "annotations": pod.Annotations,
				"node": pod.Spec.NodeName, "scheduler": pod.Spec.SchedulerName,
				"readinessGates": pod.Spec.ReadinessGates, "containers": pod.Spec.Containers,
				"initContainers": pod.Spec.InitContainers, "status": pod.Status,
			})
		}
	}
	ms, err := r.client.WorkloadV1alpha1().ModelServings(testNamespace).Get(diagnosticCtx, r.ms.Name, metav1.GetOptions{})
	if err != nil {
		r.t.Logf("Get diagnostic ModelServing: %v", err)
	} else {
		logJSON("ModelServing "+ms.Name, ms)
	}
	events, err := r.kube.CoreV1().Events(testNamespace).List(diagnosticCtx, metav1.ListOptions{})
	if err != nil {
		r.t.Logf("List diagnostic events: %v", err)
		return
	}
	for _, event := range events.Items {
		if names[event.InvolvedObject.Name] {
			logJSON("Event "+event.InvolvedObject.Name, event)
		}
	}
}

func (r *coordinatedInPlaceE2E) ordinal(pod corev1.Pod) int {
	_, ordinal := controllerutils.GetParentNameAndOrdinal(pod.Labels[workload.RoleIDKey])
	return ordinal
}

func (r *coordinatedInPlaceE2E) runtimeOnTarget(pod corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) != 1 {
		return false
	}
	status := pod.Status.ContainerStatuses[0]
	return status.ContainerID != "" && status.ContainerID != r.containers[pod.Name] &&
		(status.Image == nginxAlpineImage || strings.HasSuffix(status.Image, "/"+nginxAlpineImage))
}

func (r *coordinatedInPlaceE2E) snapshotInPlace() (*coordinatedInPlaceSnapshot, error) {
	pods, err := r.pods()
	if err != nil {
		return nil, err
	}
	if len(pods) != r.expected {
		return nil, fmt.Errorf("Pod count %d, want %d", len(pods), r.expected)
	}
	s := &coordinatedInPlaceSnapshot{pods: map[string]corev1.Pod{}, started: map[string]int{}, ready: map[string]int{}}
	for _, pod := range pods {
		uid, exists := r.original[pod.Name]
		if !exists || uid != pod.UID || pod.DeletionTimestamp != nil || !controllerutils.IsOwnedByModelServingWithUID(&pod, r.ms.UID) {
			return nil, fmt.Errorf("Pod %s was replaced, deleted, added or changed owner", pod.Name)
		}
		if len(pod.Spec.Containers) != 1 {
			return nil, fmt.Errorf("Pod %s has unexpected container membership", pod.Name)
		}
		s.pods[pod.Name] = pod
	}
	for _, role := range []string{"prefill", "decode"} {
		for ordinal := 0; ordinal < 4; ordinal++ {
			started, ready := false, true
			for member := 0; member < 2; member++ {
				name := controllerutils.GeneratePodName(r.ms.Name+"-0", controllerutils.GenerateRoleID(role, ordinal), member)
				pod, exists := s.pods[name]
				if !exists || pod.Labels[workload.RoleLabelKey] != role || pod.Labels[workload.RoleIDKey] != controllerutils.GenerateRoleID(role, ordinal) {
					return nil, fmt.Errorf("missing or misidentified member %s", name)
				}
				state := inplace.StateOf(&pod)
				switch pod.Spec.Containers[0].Image {
				case nginxImage:
					if controllerutils.ObjectRevision(&pod) != r.sourceRevision || state != nil {
						return nil, fmt.Errorf("source Pod %s has unexpected revision or update state", name)
					}
					ready = false
				case nginxAlpineImage:
					started = true
					if state == nil || state.Revision != r.revision {
						return nil, fmt.Errorf("target image of %s has no matching in-place admission", name)
					}
					ready = ready && utils.IsPodReady(pod) && r.runtimeOnTarget(pod) && state.Done && controllerutils.ObjectRevision(&pod) == r.revision
				default:
					return nil, fmt.Errorf("Pod %s has unexpected image %s", name, pod.Spec.Containers[0].Image)
				}
			}
			if started {
				s.started[role]++
			}
			if ready {
				s.ready[role]++
			}
		}
	}
	return s, nil
}

func (r *coordinatedInPlaceE2E) held(s *coordinatedInPlaceSnapshot, entryReady bool) bool {
	for _, role := range []string{"prefill", "decode"} {
		if s.started[role] != 1 || s.ready[role] != 0 {
			return false
		}
	}
	for _, pod := range s.pods {
		if r.ordinal(pod) != 3 {
			if pod.Spec.Containers[0].Image != nginxImage || !utils.IsPodReady(pod) {
				return false
			}
			continue
		}
		isEntry := pod.Labels[workload.EntryLabelKey] == controllerutils.Entry
		if pod.Spec.Containers[0].Image != nginxAlpineImage || controllerutils.ObjectRevision(&pod) != r.sourceRevision ||
			utils.IsPodReady(pod) != (entryReady && isEntry) || !inplace.InProgress(&pod) {
			return false
		}
	}
	return true
}

func (r *coordinatedInPlaceE2E) holdInPlace(message string, entryReady bool) {
	r.t.Helper()
	for range 2 {
		var generation int64
		updateModelServingWithRetry(r.t, r.ctx, r.client, r.ms.Name, func(ms *workload.ModelServing) {
			ms.Spec.RevisionHistoryLimit = ptr.To(ptr.Deref(ms.Spec.RevisionHistoryLimit, 10) + 1)
			generation = ms.Generation + 1
			require.Equal(r.t, r.revision, controllerutils.ModelServingRevision(ms))
		})
		r.poll(message+": acknowledge a new reconcile", func() (bool, error) {
			s, err := r.snapshotInPlace()
			if err != nil {
				return false, err
			}
			if !r.held(s, entryReady) {
				return false, fmt.Errorf("%s: starts=%v, complete target Ready=%v", message, s.started, s.ready)
			}
			ms, err := r.client.WorkloadV1alpha1().ModelServings(testNamespace).Get(r.ctx, r.ms.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return ms.Status.ObservedGeneration >= generation, nil
		})
	}
	until := time.Now().Add(10 * time.Second)
	r.poll(message+": maintain the checkpoint", func() (bool, error) {
		s, err := r.snapshotInPlace()
		if err != nil {
			return false, err
		}
		if !r.held(s, entryReady) {
			return false, fmt.Errorf("%s: starts=%v, complete target Ready=%v", message, s.started, s.ready)
		}
		return time.Now().After(until), nil
	})
}

func (r *coordinatedInPlaceE2E) setHighestGates(status corev1.ConditionStatus, entryOnly bool) {
	r.t.Helper()
	pods, err := r.pods()
	require.NoError(r.t, err)
	count := 0
	for _, pod := range pods {
		if r.ordinal(pod) != 3 || (entryOnly && pod.Labels[workload.EntryLabelKey] != controllerutils.Entry) {
			continue
		}
		require.NoError(r.t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := r.kube.CoreV1().Pods(testNamespace).Get(r.ctx, pod.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if current.UID != r.original[pod.Name] {
				return fmt.Errorf("Pod %s changed UID before setting readiness", pod.Name)
			}
			patch, err := json.Marshal(map[string]interface{}{
				"metadata": map[string]string{"resourceVersion": current.ResourceVersion},
				"status": map[string]interface{}{"conditions": []corev1.PodCondition{{
					Type: coordinationReadyGate, Status: status, Reason: "E2EControlled", LastTransitionTime: metav1.Now(),
				}}},
			})
			if err != nil {
				return err
			}
			_, err = r.kube.CoreV1().Pods(testNamespace).Patch(r.ctx, pod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
			return err
		}))
		count++
	}
	want := 4
	if entryOnly {
		want = 2
	}
	require.Equal(r.t, want, count, "set gates on the original highest instances")
}
