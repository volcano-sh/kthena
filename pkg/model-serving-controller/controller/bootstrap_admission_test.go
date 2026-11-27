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

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const bootstrapTestServerAddress = "modelexpress.mx:8001"

func bootstrapModelServing(name string, replicas, roleReplicas int32) *workloadv1alpha1.ModelServing {
	ms := createStandardModelServing(name, replicas, roleReplicas)
	ms.UID = types.UID(name + "-uid")
	ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
		ModelExpress: &workloadv1alpha1.ModelExpressConfig{
			EngineContainers: []string{"prefill-container"},
			ReadyURL:         "http://127.0.0.1:8000/health",
		},
	}
	return ms
}

func bootstrapPDModelServing(name string, replicas, prefillReplicas, decodeReplicas int32) *workloadv1alpha1.ModelServing {
	ms := createStandardModelServing(name, replicas, prefillReplicas)
	ms.UID = types.UID(name + "-uid")
	decodeRole := ms.Spec.Template.Roles[0].DeepCopy()
	decodeRole.Name = "decode"
	decodeRole.Replicas = ptr.To(decodeReplicas)
	decodeRole.EntryTemplate.Spec.Containers[0].Name = "decode-container"
	decodeRole.EntryTemplate.Spec.Containers[0].Image = "decode:v1"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, *decodeRole)
	ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
		InitialSeedReplicas: ptr.To(intstr.FromInt32(1)),
		ModelExpress: &workloadv1alpha1.ModelExpressConfig{
			EngineContainers: []string{"prefill-container", "decode-container"},
			ReadyURL:         "http://127.0.0.1:8000/health",
		},
	}
	return ms
}

func newBootstrapTestController(t *testing.T, ms *workloadv1alpha1.ModelServing) (*testControllerHarness, *record.FakeRecorder) {
	t.Helper()
	h := newTestController(t, ms)
	h.controller.modelExpressServerAddress = bootstrapTestServerAddress
	recorder := record.NewFakeRecorder(64)
	h.controller.recorder = recorder
	require.Eventually(t, func() bool {
		_, err := h.controller.modelServingLister.ModelServings(ms.Namespace).Get(ms.Name)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	return h, recorder
}

func (h *testControllerHarness) sync(ms *workloadv1alpha1.ModelServing) {
	h.t.Helper()
	require.NoError(h.t, h.controller.syncModelServing(context.Background(), namespacedKey(ms.Namespace, ms.Name)))
}

func (h *testControllerHarness) listPods(ms *workloadv1alpha1.ModelServing) []corev1.Pod {
	h.t.Helper()
	pods, err := h.kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name}.String(),
	})
	require.NoError(h.t, err)
	return pods.Items
}

func (h *testControllerHarness) markRoleReady(ms *workloadv1alpha1.ModelServing, group, roleName, roleID string) {
	h.t.Helper()
	pods, err := h.kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.Set{workloadv1alpha1.GroupNameLabelKey: group, workloadv1alpha1.RoleIDKey: roleID}.String(),
	})
	require.NoError(h.t, err)
	require.NotEmpty(h.t, pods.Items)
	for i := range pods.Items {
		pod := &pods.Items[i]
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		_, err := h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
		require.NoError(h.t, err)
	}
	require.Eventually(h.t, func() bool {
		return h.controller.store.GetRoleStatus(utils.GetNamespaceName(ms), group, roleName, roleID) == datastore.RoleRunning
	}, 2*time.Second, 10*time.Millisecond, "role %s/%s should become Running", group, roleID)
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func envMap(container corev1.Container) map[string]corev1.EnvVar {
	out := make(map[string]corev1.EnvVar, len(container.Env))
	for _, env := range container.Env {
		out[env.Name] = env
	}
	return out
}

func updateMSAndWait(t *testing.T, h *testControllerHarness, ms *workloadv1alpha1.ModelServing) {
	t.Helper()
	ms.Generation++
	updated, err := h.kthenaClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(context.Background(), ms, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := h.controller.modelServingLister.ModelServings(ms.Namespace).Get(ms.Name)
		return err == nil && got.Generation == updated.Generation
	}, 2*time.Second, 10*time.Millisecond)
}

func TestBootstrapCreatesWholeSeedGroupThenRemainingGroups(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-seed", 3, 2)
	h, recorder := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	// Cold start admits exactly ServingGroup 0 with all 2 role replicas intact.
	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1, "only one seed ServingGroup is created while cold")
	assert.Equal(t, ms.Name+"-0", groups[0].Name)

	pods := h.listPods(ms)
	require.Len(t, pods, 2, "seed ServingGroup must create all role replicas intact")
	for _, pod := range pods {
		env := envMap(pod.Spec.Containers[0])
		hash := pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey]
		require.NotEmpty(t, hash)
		assert.Equal(t, fmt.Sprintf("%s/%s/%s-0", ms.Namespace, ms.Name, hash), env[plugins.EnvModelRevision].Value)
		assert.Equal(t, bootstrapTestServerAddress, env[plugins.EnvServerAddress].Value)
		assert.Equal(t, "http://127.0.0.1:8000/health", env[plugins.EnvArtifactReadyURL].Value)
		assert.Equal(t, "auto", env[plugins.EnvRDMANICPin].Value)
	}
	events := drainEvents(recorder)
	assert.True(t, strings.Contains(strings.Join(events, "\n"), "BootstrapBatchCreated Created 1 ServingGroup(s) for Role prefill"))

	// Repeated sync while seed group is still NotReady creates no additional groups.
	h.sync(ms)
	require.Len(t, h.listPods(ms), 2)

	// Once a single role replica in ServingGroup 0 becomes Ready, groups 1 and 2 are created in full.
	h.markRoleReady(ms, ms.Name+"-0", "prefill", "prefill-0")
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 3)
	require.Len(t, h.listPods(ms), 6)

	events = drainEvents(recorder)
	assert.True(t, strings.Contains(strings.Join(events, "\n"), "Created 2 ServingGroup(s) for Role prefill"))
	assert.True(t, strings.Contains(strings.Join(events, "\n"), "with 1 ready peer-to-peer source(s)"))
}

func TestBootstrapDisaggregatedPDWithVolcanoGang(t *testing.T) {
	ms := bootstrapPDModelServing("bootstrap-pd-gang", 3, 2, 2)
	ms.Spec.SchedulerName = "volcano"
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{
		MinRoleReplicas: map[string]int32{"prefill": 1, "decode": 2},
	}

	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	// Cold start creates ServingGroup 0 with its PodGroup and all 4 replicas (2P + 2D),
	// while ServingGroups 1 and 2 get neither PodGroup nor Pods.
	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, h.listPods(ms), 4)

	pgs, err := h.volcanoClient.SchedulingV1beta1().PodGroups(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pgs.Items, 1)
	assert.Equal(t, ms.Name+"-0", pgs.Items[0].Name)

	// Making only prefill Ready is not enough: decode is still cold.
	h.markRoleReady(ms, ms.Name+"-0", "prefill", "prefill-0")
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1, "remaining groups wait until decode also has a Ready source")
	pgs, err = h.volcanoClient.SchedulingV1beta1().PodGroups(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pgs.Items, 1)

	// Once decode also has a Ready replica, both remaining groups and their PodGroups are created in full.
	h.markRoleReady(ms, ms.Name+"-0", "decode", "decode-0")
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 3)
	require.Len(t, h.listPods(ms), 12)

	pgs, err = h.volcanoClient.SchedulingV1beta1().PodGroups(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pgs.Items, 3)
}

func TestBootstrapExistingGroupUngatedForIntraGroupScaleAndRoleCoordination(t *testing.T) {
	ms := bootstrapPDModelServing("bootstrap-coord", 1, 2, 2)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.RoleRollingUpdate,
		RoleCoordination: &workloadv1alpha1.RoleCoordination{
			MaxSkew: ptr.To(intstr.FromString("50%")),
			Dependencies: []workloadv1alpha1.RoleRolloutDependency{
				{Role: "prefill", DependsOn: []string{"decode"}},
			},
		},
	}

	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	// Initial single group is created in full.
	h.sync(ms)
	require.Len(t, h.listPods(ms), 4)

	// Even while all replicas are still cold (NotReady), scaling role replicas inside the
	// existing ServingGroup via scaleUpRoles must remain ungated.
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
	ms.Spec.Template.Roles[1].Replicas = ptr.To[int32](3)
	updateMSAndWait(t, h, ms)
	h.sync(ms)
	require.Len(t, h.listPods(ms), 6, "intra-group role scale-up inside an existing ServingGroup must not be gated")

	// Mark all replicas Ready, then trigger a coordinated RoleRollingUpdate on both roles.
	for i := 0; i < 3; i++ {
		h.markRoleReady(ms, ms.Name+"-0", "prefill", fmt.Sprintf("prefill-%d", i))
		h.markRoleReady(ms, ms.Name+"-0", "decode", fmt.Sprintf("decode-%d", i))
	}
	h.sync(ms)

	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v2"
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "decode:v2"
	ms.Spec.Template.Roles[1].MaxUnavailable = ptr.To(intstr.FromInt32(2))
	updateMSAndWait(t, h, ms)

	// First sync deletes outdated decode replicas (dependency) according to role rollout policy
	// (maxSkew=50% on 3 replicas permits 2 target starts, matching maxUnavailable=2).
	h.sync(ms)
	require.Eventually(t, func() bool {
		decodeRoles, err := c.store.GetRoleList(key, ms.Name+"-0", "decode")
		return err == nil && len(decodeRoles) == 1
	}, 2*time.Second, 10*time.Millisecond, "RoleRollingUpdate should delete 2 outdated decode replicas")

	// Next sync recreates both deleted decode replicas at v2 immediately (not throttled to 1 by bootstrap),
	// and still injects MX_SERVER_ADDRESS into the recreated v2 Pods.
	h.sync(ms)
	v2DecodePods := 0
	for _, pod := range h.listPods(ms) {
		if pod.Labels[workloadv1alpha1.RoleLabelKey] == "decode" && pod.Spec.Containers[0].Image == "decode:v2" {
			v2DecodePods++
			assert.Equal(t, bootstrapTestServerAddress, envMap(pod.Spec.Containers[0])[plugins.EnvServerAddress].Value)
		}
	}
	assert.Equal(t, 2, v2DecodePods, "both deleted decode replicas should be recreated without per-role bootstrap gating")
}

func TestBootstrapServingGroupRollingUpdateAndInPlaceFallback(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-rollout", 4, 1)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:         workloadv1alpha1.ServingGroupRollingUpdate,
		UpdateMethod: workloadv1alpha1.InPlaceUpdateMethod,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
			MaxUnavailable: ptr.To(intstr.FromInt32(2)),
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
		},
	}

	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	// Managed modelexpress plugin does not support InPlaceUpdatePlugin, so updatesInPlace falls back to recreate.
	assert.False(t, c.updatesInPlace(ms))

	// Converge v1 fleet to Ready.
	h.sync(ms)
	h.markRoleReady(ms, ms.Name+"-0", "prefill", "prefill-0")
	h.sync(ms)
	for i := 1; i < 4; i++ {
		h.markRoleReady(ms, fmt.Sprintf("%s-%d", ms.Name, i), "prefill", "prefill-0")
	}
	h.sync(ms)
	require.Len(t, h.listPods(ms), 4)

	// Update container image to v2.
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v2"
	updateMSAndWait(t, h, ms)

	// Rollout deletes groups 3 and 2 (maxUnavailable=2).
	h.sync(ms)
	require.Eventually(t, func() bool {
		return c.store.GetServingGroupStatus(key, ms.Name+"-3") == datastore.ServingGroupNotFound &&
			c.store.GetServingGroupStatus(key, ms.Name+"-2") == datastore.ServingGroupNotFound
	}, 2*time.Second, 10*time.Millisecond)

	// Next sync: v2 pool is cold with S=1, so only 1 replacement ServingGroup (ordinal 2) is created;
	// ordinal 3 stays deferred, and maxScaleDown sees len(groups)=3 so no more old groups are deleted.
	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 3)
	assert.Equal(t, datastore.ServingGroupNotFound, c.store.GetServingGroupStatus(key, ms.Name+"-3"))

	// Once the v2 seed group (ordinal 2) becomes Ready, ordinal 3 is created at v2 and rollout
	// immediately resumes normal maxUnavailable=2 pacing by deleting outdated group 1.
	h.markRoleReady(ms, ms.Name+"-2", "prefill", "prefill-0")
	h.sync(ms)
	assert.NotEqual(t, datastore.ServingGroupNotFound, c.store.GetServingGroupStatus(key, ms.Name+"-3"), "group 3 should be created once v2 seed is Ready")
	v2Pods := 0
	for _, pod := range h.listPods(ms) {
		if pod.Spec.Containers[0].Image == "prefill:v2" {
			v2Pods++
		}
	}
	assert.Equal(t, 2, v2Pods, "both groups 2 and 3 should now exist on v2")
}

func TestBootstrapOutOfScopeRoleUpdateDoesNotThrottle(t *testing.T) {
	ms := bootstrapPDModelServing("bootstrap-scoped-rollout", 3, 1, 1)
	ms.Spec.BootstrapAccelerateStrategy.Roles = []string{"prefill"}
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"prefill-container"}
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
			MaxUnavailable: ptr.To(intstr.FromInt32(2)),
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
		},
	}

	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	// Converge v1 fleet (decode is out of scope, so only prefill readiness gates v1).
	h.sync(ms)
	h.markRoleReady(ms, ms.Name+"-0", "prefill", "prefill-0")
	h.sync(ms)
	for i := 0; i < 3; i++ {
		group := fmt.Sprintf("%s-%d", ms.Name, i)
		h.markRoleReady(ms, group, "prefill", "prefill-0")
		h.markRoleReady(ms, group, "decode", "decode-0")
	}
	h.sync(ms)

	// Change only out-of-scope decode image: prefill pool stays warm (R=1 > 0), so both
	// deleted groups are recreated immediately without seed throttling.
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "decode:v2"
	updateMSAndWait(t, h, ms)
	h.sync(ms)
	require.Eventually(t, func() bool {
		return c.store.GetServingGroupStatus(key, ms.Name+"-2") == datastore.ServingGroupNotFound &&
			c.store.GetServingGroupStatus(key, ms.Name+"-1") == datastore.ServingGroupNotFound
	}, 2*time.Second, 10*time.Millisecond)
	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 3, "changing only an out-of-scope role must not throttle ServingGroup creation")
}

func TestBootstrapPartiallyCreatedGroupCountsOnceInFlight(t *testing.T) {
	ms := bootstrapPDModelServing("bootstrap-partial", 3, 1, 1)
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)
	revision := utils.ModelServingRevision(ms)

	// Persist revision and seed datastore with group-0 having only prefill recorded (decode not created yet).
	require.NoError(t, c.ensureTargetRevision(context.Background(), ms, revision))
	c.store.AddServingGroup(key, 0, revision)
	prefillHash, err := utils.RoleRevisionHash(ms, "prefill")
	require.NoError(t, err)
	c.store.AddRole(key, ms.Name+"-0", "prefill", "prefill-0", revision, prefillHash)

	admitter, err := c.newBootstrapAdmitter(context.Background(), ms)
	require.NoError(t, err)
	require.NotNil(t, admitter)

	// Group 0 already occupies the single seed slot for both prefill and decode, so a new group is deferred.
	assert.False(t, admitter.AdmitGroup(ms))
}

func TestBootstrapEmptyOutdatedGroupResetIsGated(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-reset", 3, 1)
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	// Create v1 revision and leave two empty outdated groups (group 0, group 1) in datastore with no Pods.
	v1Rev := utils.ModelServingRevision(ms)
	require.NoError(t, c.ensureTargetRevision(context.Background(), ms, v1Rev))
	c.store.AddServingGroup(key, 0, v1Rev)
	c.store.AddServingGroup(key, 1, v1Rev)
	c.store.AddServingGroup(key, 2, v1Rev)

	// Move spec to v2; only 1 empty outdated group should be reset to v2 while v2 has R=0.
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v2"
	updateMSAndWait(t, h, ms)
	h.sync(ms)

	v2Pods := 0
	for _, pod := range h.listPods(ms) {
		if pod.Spec.Containers[0].Image == "prefill:v2" {
			v2Pods++
		}
	}
	assert.Equal(t, 1, v2Pods, "only S=1 empty outdated ServingGroup should reset to cold revision v2")
}

func TestBootstrapDisablePolicyAdmitsDeferredGroups(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-disable", 3, 1)
	h, _ := newBootstrapTestController(t, ms)

	h.sync(ms)
	require.Len(t, h.listPods(ms), 1)

	ms.Spec.BootstrapAccelerateStrategy = nil
	updateMSAndWait(t, h, ms)
	h.sync(ms)

	pods := h.listPods(ms)
	require.Len(t, pods, 3)
	for _, pod := range pods {
		if pod.Labels[workloadv1alpha1.GroupNameLabelKey] != ms.Name+"-0" {
			_, hasServer := envMap(pod.Spec.Containers[0])[plugins.EnvServerAddress]
			assert.False(t, hasServer, "groups created after disabling strategy must not receive MX env")
		}
	}
}
