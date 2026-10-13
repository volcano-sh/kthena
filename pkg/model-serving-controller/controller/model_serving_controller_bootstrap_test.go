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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/bootstrap"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func newBootstrapTestController(t *testing.T, gang bool) (*ModelServingController, *kubefake.Clientset, *record.FakeRecorder) {
	t.Helper()
	kubeClient := kubefake.NewSimpleClientset()
	c, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	c.podGroupManager = &fakePodGroupManager{hasCRD: gang}
	recorder := record.NewFakeRecorder(100)
	c.recorder = recorder
	return c, kubeClient, recorder
}

func bootstrapTestRole(name string, replicas int32) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:     name,
		Replicas: ptr.To(replicas),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "engine"}}},
		},
	}
}

func bootstrapTestModelServing(replicas int32, strategy *workloadv1alpha1.BootstrapAccelerateStrategy, roles ...workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-uid"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:                    ptr.To(replicas),
			SchedulerName:               "volcano",
			BootstrapAccelerateStrategy: strategy,
			Template:                    workloadv1alpha1.ServingGroup{Roles: roles},
		},
	}
}

// syncBootstrapReplicas runs the replica steps of one reconcile.
func syncBootstrapReplicas(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) {
	t.Helper()
	admitter, err := c.newBootstrapAdmitter(ms)
	require.NoError(t, err)
	revision := utils.ModelServingRevision(ms)
	require.NoError(t, c.syncServingGroupReplicas(context.Background(), ms, revision, admitter))
	require.NoError(t, c.syncRoleReplicas(context.Background(), ms, revision, admitter))
	c.reportBootstrapAdmission(ms, admitter)
}

// roleCounts returns the number of stored replicas per role across ServingGroups.
func roleCounts(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		return counts
	}
	for _, group := range groups {
		for _, role := range ms.Spec.Template.Roles {
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, role.Name)
			require.NoError(t, err)
			counts[role.Name] += len(roles)
		}
	}
	return counts
}

func setAllRolesRunning(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) {
	t.Helper()
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	for _, group := range groups {
		for _, role := range ms.Spec.Template.Roles {
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, role.Name)
			require.NoError(t, err)
			for _, r := range roles {
				require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group.Name, role.Name, r.Name, datastore.RoleRunning))
			}
		}
	}
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

func TestBootstrapNewServingGroupAdmission(t *testing.T) {
	tests := []struct {
		name       string
		gang       bool
		strategy   *workloadv1alpha1.BootstrapAccelerateStrategy
		roles      []workloadv1alpha1.Role
		wantGroups int
		wantRoles  map[string]int
	}{
		{
			name:       "disabled creates everything",
			roles:      []workloadv1alpha1.Role{bootstrapTestRole("decode", 2)},
			wantGroups: 3,
			wantRoles:  map[string]int{"decode": 6},
		},
		{
			name:       "seed without gang creates one replica",
			strategy:   &workloadv1alpha1.BootstrapAccelerateStrategy{},
			roles:      []workloadv1alpha1.Role{bootstrapTestRole("decode", 2)},
			wantGroups: 1,
			wantRoles:  map[string]int{"decode": 1},
		},
		{
			name:       "seed with gang creates the gang minimum",
			gang:       true,
			strategy:   &workloadv1alpha1.BootstrapAccelerateStrategy{},
			roles:      []workloadv1alpha1.Role{bootstrapTestRole("decode", 2)},
			wantGroups: 1,
			wantRoles:  map[string]int{"decode": 2},
		},
		{
			name:       "out-of-scope roles are created in full",
			strategy:   &workloadv1alpha1.BootstrapAccelerateStrategy{Roles: []string{"decode"}},
			roles:      []workloadv1alpha1.Role{bootstrapTestRole("prefill", 2), bootstrapTestRole("decode", 2)},
			wantGroups: 1,
			wantRoles:  map[string]int{"prefill": 2, "decode": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, kubeClient, _ := newBootstrapTestController(t, tt.gang)
			ms := bootstrapTestModelServing(3, tt.strategy, tt.roles...)
			admitter, err := c.newBootstrapAdmitter(ms)
			require.NoError(t, err)
			require.NoError(t, c.syncServingGroupReplicas(context.Background(), ms, utils.ModelServingRevision(ms), admitter))

			groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
			require.NoError(t, err)
			assert.Len(t, groups, tt.wantGroups)
			assert.Equal(t, tt.wantRoles, roleCounts(t, c, ms))

			pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			wantPods := 0
			for _, count := range tt.wantRoles {
				wantPods += count
			}
			assert.Len(t, pods.Items, wantPods)
		})
	}
}

func TestBootstrapBatchesGrowWithRunningReplicas(t *testing.T) {
	c, kubeClient, recorder := newBootstrapTestController(t, false)
	ms := bootstrapTestModelServing(3, &workloadv1alpha1.BootstrapAccelerateStrategy{}, bootstrapTestRole("decode", 2))

	// No source: the seed creates one replica in ServingGroup 0.
	syncBootstrapReplicas(t, c, ms)
	assert.Equal(t, map[string]int{"decode": 1}, roleCounts(t, c, ms))
	events := drainEvents(recorder)
	assert.Contains(t, strings.Join(events, "\n"), "BootstrapBatchCreated")

	// Nothing more is created while the seed is starting.
	syncBootstrapReplicas(t, c, ms)
	assert.Equal(t, map[string]int{"decode": 1}, roleCounts(t, c, ms))

	// One running source admits one replica; new ServingGroups go first.
	setAllRolesRunning(t, c, ms)
	syncBootstrapReplicas(t, c, ms)
	assert.Equal(t, map[string]int{"decode": 2}, roleCounts(t, c, ms))
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	assert.Len(t, groups, 2)

	// Two running sources admit two more replicas.
	setAllRolesRunning(t, c, ms)
	syncBootstrapReplicas(t, c, ms)
	assert.Equal(t, map[string]int{"decode": 4}, roleCounts(t, c, ms))

	setAllRolesRunning(t, c, ms)
	syncBootstrapReplicas(t, c, ms)
	assert.Equal(t, map[string]int{"decode": 6}, roleCounts(t, c, ms))

	// Every created role records the configuration hash of its pods.
	wantHash, err := bootstrap.ConfigHash(ms, "decode")
	require.NoError(t, err)
	groups, err = c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	for _, group := range groups {
		roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, "decode")
		require.NoError(t, err)
		for _, role := range roles {
			assert.Equal(t, wantHash, role.BootstrapConfigHash, "role %s/%s", group.Name, role.Name)
		}
	}
	pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, pods.Items, 6)
	for _, pod := range pods.Items {
		assert.Equal(t, wantHash, pod.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey], "pod %s", pod.Name)
	}
}

func TestBootstrapGangCoreDeficit(t *testing.T) {
	tests := []struct {
		name string
		// startingPeers are starting replicas of the same pool in another ServingGroup.
		startingPeers int
		wantCreated   bool
	}{
		{name: "deficit fits the budget", wantCreated: true},
		{name: "deficit exceeds the budget", startingPeers: 2, wantCreated: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, kubeClient, _ := newBootstrapTestController(t, true)
			role := bootstrapTestRole("decode", 3)
			ms := bootstrapTestModelServing(2, &workloadv1alpha1.BootstrapAccelerateStrategy{}, role)
			ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"decode": 3}}
			key := utils.GetNamespaceName(ms)
			revision := utils.ModelServingRevision(ms)
			roleHash := utils.CalRoleTemplateHash(role)
			configHash, err := bootstrap.ConfigHash(ms, "decode")
			require.NoError(t, err)
			addRole := func(groupOrdinal, roleOrdinal int) {
				groupName := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
				roleID := utils.GenerateRoleID("decode", roleOrdinal)
				c.store.AddRole(key, groupName, "decode", roleID, revision, roleHash)
				c.store.ObserveRoleBootstrapConfigHash(key, groupName, "decode", roleID, groupName+"-"+roleID, configHash)
			}
			c.store.AddServingGroup(key, 0, revision)
			addRole(0, 0)
			if tt.startingPeers > 0 {
				c.store.AddServingGroup(key, 1, revision)
				for i := 0; i < tt.startingPeers; i++ {
					addRole(1, i)
				}
			}

			admitter, err := c.newBootstrapAdmitter(ms)
			require.NoError(t, err)
			c.createGangCoreDeficit(context.Background(), ms, utils.GenerateServingGroupName(ms.Name, 0), 0, ms.Spec.Template.Roles, revision, admitter)

			roles, err := c.store.GetRoleList(key, utils.GenerateServingGroupName(ms.Name, 0), "decode")
			require.NoError(t, err)
			pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			pools := admitter.Pools()
			require.Len(t, pools, 1)
			if tt.wantCreated {
				assert.Len(t, roles, 3)
				assert.Len(t, pods.Items, 2)
				assert.Equal(t, 2, pools[0].Created)
			} else {
				assert.Len(t, roles, 1)
				assert.Empty(t, pods.Items)
				assert.Equal(t, 2, pools[0].Deferred)
			}
		})
	}
}

func TestBootstrapPodEventsTrackConfigHash(t *testing.T) {
	c, kubeClient, _ := newBootstrapTestController(t, false)
	ms := bootstrapTestModelServing(1, &workloadv1alpha1.BootstrapAccelerateStrategy{}, bootstrapTestRole("decode", 1))
	ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	syncBootstrapReplicas(t, c, ms)

	wantHash, err := bootstrap.ConfigHash(ms, "decode")
	require.NoError(t, err)
	pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	pod := &pods.Items[0]
	roleHash := func() string {
		roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), utils.GenerateServingGroupName(ms.Name, 0), "decode")
		require.NoError(t, err)
		require.Len(t, roles, 1)
		return roles[0].BootstrapConfigHash
	}
	withHash := func(hash string) *corev1.Pod {
		p := pod.DeepCopy()
		p.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey] = hash
		return p
	}

	assert.Equal(t, wantHash, roleHash())
	c.updatePod(nil, withHash("other"))
	assert.Equal(t, "other", roleHash())
	c.deletePod(withHash("other"))
	assert.Equal(t, "", roleHash())
	// The replacement Pod restores the hash of the retained role.
	c.updatePod(nil, withHash(wantHash))
	assert.Equal(t, wantHash, roleHash())
}

func TestBootstrapRenderFailureCreatesNothing(t *testing.T) {
	c, kubeClient, recorder := newBootstrapTestController(t, false)
	strategy := &workloadv1alpha1.BootstrapAccelerateStrategy{
		Roles:        []string{"decode"},
		ModelExpress: &workloadv1alpha1.ModelExpressConfig{EngineContainers: []string{"missing"}},
	}
	ms := bootstrapTestModelServing(1, strategy, bootstrapTestRole("prefill", 1), bootstrapTestRole("decode", 1))
	admitter, err := c.newBootstrapAdmitter(ms)
	require.NoError(t, err)

	err = c.syncServingGroupReplicas(context.Background(), ms, utils.ModelServingRevision(ms), admitter)
	require.Error(t, err)

	pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pods.Items)
	assert.Empty(t, roleCounts(t, c, ms))
	assert.Contains(t, strings.Join(drainEvents(recorder), "\n"), "PodRenderFailed")
}
