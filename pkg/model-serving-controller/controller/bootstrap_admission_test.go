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

// bootstrapModelServing enables bootstrap acceleration for the prefill role of
// a standard ModelServing.
func bootstrapModelServing(name string, replicas, roleReplicas int32) *workloadv1alpha1.ModelServing {
	ms := createStandardModelServing(name, replicas, roleReplicas)
	ms.UID = types.UID(name + "-uid")
	ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
		Roles: []workloadv1alpha1.BootstrapRole{{Name: "prefill"}},
		ModelExpress: &workloadv1alpha1.ModelExpressConfig{
			EngineContainers: []string{"prefill-container"},
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

func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	return names
}

// markRoleReady makes every Pod of a role replica Ready and waits for the
// controller to record the replica as Running.
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

func TestBootstrapCreatesSeedThenRemainingGroups(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-seed", 3, 1)
	h, recorder := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1, "only the seed group is created while no source is ready")
	assert.Equal(t, ms.Name+"-0", groups[0].Name)
	pods := h.listPods(ms)
	require.Len(t, pods, 1)
	seed := pods[0]

	env := map[string]string{}
	for _, variable := range seed.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	assert.Equal(t, bootstrapTestServerAddress, env[plugins.EnvServerAddress])
	assert.Equal(t, utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])+"-0", env[plugins.EnvModelRevision])
	assert.Equal(t, "http://127.0.0.1:8000/health", env[plugins.EnvArtifactReadyURL])
	assert.Equal(t, "1", env[plugins.EnvP2PMetadata])
	events := drainEvents(recorder)
	require.Len(t, filterEvents(events, "BootstrapBatchCreated"), 1)
	assert.Contains(t, filterEvents(events, "BootstrapBatchCreated")[0], "Created 1 replica(s) of Role prefill")

	// Nothing more while the seed is still starting, even across reconciles.
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	assert.Len(t, groups, 1)
	assert.Empty(t, filterEvents(drainEvents(recorder), "BootstrapBatchCreated"))

	h.markRoleReady(ms, ms.Name+"-0", "prefill", "prefill-0")
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	assert.Len(t, groups, 3, "every remaining group is created once a source is ready")
	assert.Len(t, h.listPods(ms), 3)
	batch := filterEvents(drainEvents(recorder), "BootstrapBatchCreated")
	require.Len(t, batch, 1)
	assert.Contains(t, batch[0], "Created 2 replica(s) of Role prefill")
	assert.Contains(t, batch[0], "with 1 ready peer-to-peer source(s)")
}

func TestBootstrapCreatesGangCoreThenRemainingReplicas(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-gang", 1, 4)
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 2}}
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)
	group := ms.Name + "-0"

	h.sync(ms)
	roles, err := c.store.GetRoleList(key, group, "prefill")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"prefill-0", "prefill-1"}, roleNames(roles), "the gang core is created together and nothing else")
	assert.Len(t, h.listPods(ms), 2)
	assert.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, group))

	h.markRoleReady(ms, group, "prefill", "prefill-1")
	h.sync(ms)
	roles, err = c.store.GetRoleList(key, group, "prefill")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"prefill-0", "prefill-1", "prefill-2", "prefill-3"}, roleNames(roles))
	assert.Len(t, h.listPods(ms), 4)
}

func TestBootstrapAdmissionCountsNonzeroOrdinals(t *testing.T) {
	for _, ordinal := range []int{0, 7} {
		t.Run(fmt.Sprintf("existing-ordinal-%d", ordinal), func(t *testing.T) {
			ms := bootstrapModelServing("bootstrap-core-count", 1, 4)
			ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 2}}
			h, _ := newBootstrapTestController(t, ms)
			group := ms.Name + "-0"
			key := utils.GetNamespaceName(ms)
			revision := utils.ModelServingRevision(ms)
			role := ms.Spec.Template.Roles[0]
			hash := utils.CalRoleTemplateHash(role)
			h.controller.store.AddServingGroup(key, 0, revision)
			h.controller.store.AddRole(key, group, role.Name, utils.GenerateRoleID(role.Name, ordinal), revision, hash)
			admitter, err := h.controller.newBootstrapAdmitter(ms)
			require.NoError(t, err)
			require.NotNil(t, admitter)
			assert.True(t, admitter.AdmitRole(role), "one existing replica leaves budget for one more regardless of its ordinal")
			assert.False(t, admitter.AdmitRole(role), "the existing replica and the admission exhaust the seed budget")
			summary := admitter.Summary()
			require.Len(t, summary, 1)
			assert.Equal(t, 1, summary[0].Admitted)
		})
	}
}

func TestBootstrapLeavesOutOfScopeRolesUngated(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-scope", 1, 2)
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
		Name:     "decode",
		Replicas: ptr.To[int32](3),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "decode-container", Image: "test-image:latest"}},
		}},
	})
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1, "decode": 3}}
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)
	group := ms.Name + "-0"

	h.sync(ms)
	prefill, err := c.store.GetRoleList(key, group, "prefill")
	require.NoError(t, err)
	assert.Equal(t, []string{"prefill-0"}, roleNames(prefill))
	decode, err := c.store.GetRoleList(key, group, "decode")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"decode-0", "decode-1", "decode-2"}, roleNames(decode), "out-of-scope roles are created whole")
	for _, pod := range h.listPods(ms) {
		if utils.GetRoleName(&pod) == "decode" {
			assert.Len(t, pod.Spec.Containers[0].Env, 2, "no MX environment on out-of-scope roles")
		}
	}
}

func TestBootstrapDisablingCreatesEverything(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-disable", 3, 1)
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1)

	updated := ms.DeepCopy()
	updated.Spec.BootstrapAccelerateStrategy = nil
	_, err = h.kthenaClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		current, err := c.modelServingLister.ModelServings(ms.Namespace).Get(ms.Name)
		return err == nil && current.Spec.BootstrapAccelerateStrategy == nil
	}, 2*time.Second, 10*time.Millisecond)

	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	assert.Len(t, groups, 3)
	for _, pod := range h.listPods(ms) {
		if pod.Name == ms.Name+"-0-prefill-0-0" {
			continue
		}
		assert.Len(t, pod.Spec.Containers[0].Env, 2, "new Pods are rendered without the strategy")
	}
}

// A ServingGroup whose gang core was only partially created must be repaired
// even though the seed budget it occupies defers every new group.
func TestBootstrapRepairsGangCoreDespiteDeferredGroups(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-repair", 2, 1)
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
		Name:     "decode",
		Replicas: ptr.To[int32](1),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "prefill-container", Image: "test-image:latest"}},
		}},
	})
	ms.Spec.BootstrapAccelerateStrategy.Roles = append(ms.Spec.BootstrapAccelerateStrategy.Roles, workloadv1alpha1.BootstrapRole{Name: "decode"})
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)
	group := ms.Name + "-0"
	revision := utils.ModelServingRevision(ms)

	// Group 0 exists with its prefill seed in flight; its decode was never created.
	c.store.AddServingGroup(key, 0, revision)
	prefill := ms.Spec.Template.Roles[0]
	c.store.AddRole(key, group, "prefill", "prefill-0", revision, utils.CalRoleTemplateHash(prefill))
	seedPod := utils.GenerateEntryPod(*prefill.DeepCopy(), ms, group, "prefill-0", revision, utils.CalRoleTemplateHash(prefill))
	chain, err := c.buildPluginChain(ms)
	require.NoError(t, err)
	require.NoError(t, c.createPod(context.Background(), ms, group, "prefill", "prefill-0", prefill.DeepCopy(), seedPod, true, chain, "entry"))
	require.Eventually(t, func() bool {
		pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/prefill/prefill-0", ms.Namespace, group))
		return err == nil && len(pods) == 1
	}, 2*time.Second, 10*time.Millisecond)

	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1, "group 1 waits for the prefill seed")
	decode, err := c.store.GetRoleList(key, group, "decode")
	require.NoError(t, err)
	assert.Equal(t, []string{"decode-0"}, roleNames(decode), "the incomplete core of group 0 is repaired")
	assert.ElementsMatch(t, []string{group + "-prefill-0-0", group + "-decode-0-0"}, podNames(h.listPods(ms)))
}

// Sources are counted per role template: a changed template starts a new pool
// whose first replica is a seed even while old-revision replicas are serving.
func TestBootstrapAdmitterCountsSourcesPerTemplate(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-pools", 4, 1)
	c := newRevisionTestController(t, ms)
	c.modelExpressServerAddress = bootstrapTestServerAddress
	key := utils.GetNamespaceName(ms)
	oldRole := ms.Spec.Template.Roles[0]
	oldRevision, oldHash := utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(oldRole)
	for ordinal := 0; ordinal < 2; ordinal++ {
		group := utils.GenerateServingGroupName(ms.Name, ordinal)
		c.store.AddServingGroup(key, ordinal, oldRevision)
		// A legacy record without a hash resolves it from the ControllerRevision.
		hash := oldHash
		if ordinal == 1 {
			hash = ""
		}
		c.store.AddRole(key, group, "prefill", "prefill-0", oldRevision, hash)
		require.NoError(t, c.store.UpdateRoleStatus(key, group, "prefill", "prefill-0", datastore.RoleRunning))
	}

	admitter, err := c.newBootstrapAdmitter(ms)
	require.NoError(t, err)
	require.NotNil(t, admitter)
	for i := 0; i < 2; i++ {
		_, ok := admitter.AdmitGroup(ms.Spec.Template.Roles)
		assert.True(t, ok, "the current template has two ready sources")
	}

	updated := ms.DeepCopy()
	updated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test-image:v2"
	admitter, err = c.newBootstrapAdmitter(updated)
	require.NoError(t, err)
	_, ok := admitter.AdmitGroup(updated.Spec.Template.Roles)
	assert.True(t, ok, "the new template gets its seed")
	_, ok = admitter.AdmitGroup(updated.Spec.Template.Roles)
	assert.False(t, ok, "old-revision replicas are not sources for the new template")

	// Removing the strategy disables admission entirely.
	disabled := ms.DeepCopy()
	disabled.Spec.BootstrapAccelerateStrategy = nil
	admitter, err = c.newBootstrapAdmitter(disabled)
	require.NoError(t, err)
	assert.Nil(t, admitter)
}

func TestBootstrapInPlaceUpdateFallsBackToRecreate(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-inplace", 2, 1)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:         workloadv1alpha1.ServingGroupRollingUpdate,
		UpdateMethod: workloadv1alpha1.InPlaceUpdateMethod,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
	c := newRevisionTestController(t, ms)
	assert.False(t, c.updatesInPlace(ms), "MX_MODEL_REVISION cannot be patched in place")
	ms.Spec.BootstrapAccelerateStrategy = nil
	assert.True(t, c.updatesInPlace(ms))
}

func TestBootstrapMultiPodReplicaRequiresAllPodsReady(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-multipod", 3, 1)
	ms.Spec.Template.Roles[0].WorkerReplicas = 1
	ms.Spec.Template.Roles[0].WorkerTemplate = &workloadv1alpha1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker-container", Image: "test-image:latest"}},
		},
	}
	ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"prefill-container", "worker-container"}
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)
	group0 := ms.Name + "-0"
	roleHash := utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])

	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1, "only the seed group is created initially")
	pods := h.listPods(ms)
	require.Len(t, pods, 2, "seed group has entry and worker pods")

	revisionsByPod := make(map[string]string, len(pods))
	for _, pod := range pods {
		for _, env := range pod.Spec.Containers[0].Env {
			if env.Name == plugins.EnvModelRevision {
				revisionsByPod[pod.Name] = env.Value
			}
		}
	}
	assert.Equal(t, roleHash+"-0", revisionsByPod[group0+"-prefill-0-0"], "entry pod uses position 0")
	assert.Equal(t, roleHash+"-1", revisionsByPod[group0+"-prefill-0-1"], "worker pod uses position 1")

	// Mark only the entry pod Ready; the worker pod remains NotReady.
	entryPod, err := h.kubeClient.CoreV1().Pods(ms.Namespace).Get(context.Background(), group0+"-prefill-0-0", metav1.GetOptions{})
	require.NoError(t, err)
	entryPod.Status.Phase = corev1.PodRunning
	entryPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(context.Background(), entryPod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		cached, err := c.podsLister.Pods(ms.Namespace).Get(entryPod.Name)
		return err == nil && utils.IsPodRunningAndReady(cached)
	}, 2*time.Second, 10*time.Millisecond)

	h.sync(ms)
	assert.NotEqual(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group0, "prefill", "prefill-0"))
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	assert.Len(t, groups, 1, "entry readiness alone must not release remaining groups")

	// Mark all pods of the replica Ready; remaining groups should now be admitted.
	h.markRoleReady(ms, group0, "prefill", "prefill-0")
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	assert.Len(t, groups, 3)
	assert.Len(t, h.listPods(ms), 6)
}

func TestBootstrapPercentageInitialSeedReplicas(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-pct-seed", 4, 1)
	ms.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromString("50%"))
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)

	h.sync(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 2, "50% of 4 replicas admits 2 seed groups")

	// Once a single seed group becomes Ready, the pool has a Ready source and releases the rest.
	h.markRoleReady(ms, ms.Name+"-0", "prefill", "prefill-0")
	h.sync(ms)
	groups, err = c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	assert.Len(t, groups, 4)
}

func TestBootstrapRoleRollingUpdateStagesNewTemplateBySeed(t *testing.T) {
	ms := bootstrapModelServing("bootstrap-rollout", 1, 3)
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1}}
	ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt32(3))
	ms.Spec.Template.Roles[0].MaxSurge = ptr.To(intstr.FromInt32(0))
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.RoleRollingUpdate,
	}
	h, _ := newBootstrapTestController(t, ms)
	c := h.controller
	key := utils.GetNamespaceName(ms)
	group := ms.Name + "-0"

	// Cold start: seed first, then remaining 2 replicas.
	h.sync(ms)
	h.markRoleReady(ms, group, "prefill", "prefill-0")
	h.sync(ms)
	for _, roleID := range []string{"prefill-1", "prefill-2"} {
		h.markRoleReady(ms, group, "prefill", roleID)
	}
	h.sync(ms)
	roles, err := c.store.GetRoleList(key, group, "prefill")
	require.NoError(t, err)
	require.Len(t, roles, 3)

	// Update the prefill image so the new revision has a new role template hash.
	updated := ms.DeepCopy()
	updated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test-image:v2"
	newHash := utils.CalRoleTemplateHash(updated.Spec.Template.Roles[0])
	_, err = h.kthenaClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		current, err := c.modelServingLister.ModelServings(ms.Namespace).Get(ms.Name)
		return err == nil && current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image == "test-image:v2"
	}, 2*time.Second, 10*time.Millisecond)

	// Reconcile 1 deletes the 3 outdated v1 replicas (permitted by maxUnavailable=3).
	h.sync(updated)
	require.Eventually(t, func() bool {
		pods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, group))
		return err == nil && len(pods) == 0
	}, 2*time.Second, 10*time.Millisecond)

	// Reconcile 2 finalizes any remaining deleting roles and scales up the new template:
	// only 1 v2 seed replica may be created while v2 has no Ready source.
	h.sync(updated)
	roles, err = c.store.GetRoleList(key, group, "prefill")
	require.NoError(t, err)
	require.Len(t, roles, 1, "only the v2 seed replica is admitted before a v2 source is Ready")
	assert.Equal(t, "prefill-0", roles[0].Name)
	assert.Equal(t, newHash, roles[0].RoleTemplateHash)

	// Once the v2 seed is Ready, the remaining 2 v2 replicas are created together.
	h.markRoleReady(updated, group, "prefill", "prefill-0")
	h.sync(updated)
	roles, err = c.store.GetRoleList(key, group, "prefill")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"prefill-0", "prefill-1", "prefill-2"}, roleNames(roles))
}

func roleNames(roles []datastore.Role) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	return names
}

func filterEvents(events []string, reason string) []string {
	var matched []string
	for _, event := range events {
		if strings.Contains(event, reason) {
			matched = append(matched, event)
		}
	}
	return matched
}
