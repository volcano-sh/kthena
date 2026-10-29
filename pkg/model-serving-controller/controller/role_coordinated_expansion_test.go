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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type expansionPodGroupManager struct {
	fakePodGroupManager
	informer cache.SharedIndexInformer
}

func (m *expansionPodGroupManager) GetPodGroupInformer() cache.SharedIndexInformer {
	return m.informer
}

func coordinatedExpansionFixture(t *testing.T, names []string, dependencies []api.RoleRolloutDependency, before, after int32, customize ...func(*api.ModelServing)) (*ModelServingController, *api.ModelServing, *schedulingv1beta1.PodGroup) {
	t.Helper()
	old := createStandardModelServing("expansion", 1, before)
	old.UID = "expansion-owner"
	template := old.Spec.Template.Roles[0]
	old.Spec.Template.Roles = nil
	for _, name := range names {
		role := *template.DeepCopy()
		role.Name = name
		role.MaxUnavailable = ptr.To(intstr.FromInt(0))
		role.MaxSurge = ptr.To(intstr.FromInt(1))
		old.Spec.Template.Roles = append(old.Spec.Template.Roles, role)
	}
	old.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RoleCoordination: &api.RoleCoordination{
		MaxSkew: ptr.To(intstr.FromString("50%")), Dependencies: dependencies,
	}}
	for _, apply := range customize {
		apply(old)
	}
	ms := old.DeepCopy()
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].Replicas = ptr.To(after)
		ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "target:B"
	}
	ms.Status.CurrentRevision = utils.ModelServingRevision(old)
	ms.Status.UpdateRevision = utils.ModelServingRevision(ms)
	c := newRevisionTestController(t, ms)
	t.Cleanup(c.workqueue.ShutDown)
	for _, version := range []*api.ModelServing{old, ms} {
		require.NoError(t, c.persistCoordinatedRoleRevision(context.Background(), version, utils.ModelServingRevision(version)))
	}
	pg := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: ms.Namespace, Name: "expansion-0", UID: "pg-owner",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, api.ModelServingKind)},
	}}
	for _, role := range old.Spec.Template.Roles {
		pg.Spec.SubGroupPolicy = append(pg.Spec.SubGroupPolicy, schedulingv1beta1.SubGroupPolicySpec{Name: role.Name, MinSubGroups: ptr.To(before)})
		for ordinal := 0; ordinal < int(before); ordinal++ {
			addExpansionInstance(t, c, old, pg.Name, role.Name, ordinal, true)
		}
	}
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &schedulingv1beta1.PodGroup{}, 0, cache.Indexers{})
	require.NoError(t, informer.GetIndexer().Add(pg))
	c.podGroupManager = &expansionPodGroupManager{informer: informer}
	return c, ms, pg
}

func addExpansionInstance(t *testing.T, c *ModelServingController, ms *api.ModelServing, group, name string, ordinal int, ready bool) {
	t.Helper()
	for _, role := range ms.Spec.Template.Roles {
		if role.Name != name {
			continue
		}
		id, revision := utils.GenerateRoleID(name, ordinal), utils.ModelServingRevision(ms)
		hash := utils.CalRoleTemplateHash(role)
		pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, group, id, revision, hash)
		if ordinal >= roleReplicas(role) {
			setSurgeScope(pod, surgeRole)
		}
		pod.UID = types.UID(pod.Name + "-" + revision)
		pod.Status.Phase = corev1.PodRunning
		if ready {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		c.store.AddRunningPodToServingGroup(utils.GetNamespaceName(ms), group, pod.Name, revision, hash, name, id)
		if ready {
			require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, name, id, datastore.RoleRunning))
		}
		return
	}
	t.Fatalf("Role %s is missing", name)
}

func expansionMinima(t *testing.T, c *ModelServingController, ms *api.ModelServing, group string) map[string]int32 {
	t.Helper()
	original := ms.DeepCopy()
	projected := ms.DeepCopy()
	require.NoError(t, c.stageCoordinatedExpansionGang(context.Background(), ms, projected, group))
	require.Equal(t, original, ms, "the API object, budgets and desired replicas must remain unchanged")
	if projected.Spec.Template.GangPolicy == nil {
		return nil
	}
	return projected.Spec.Template.GangPolicy.MinRoleReplicas
}

func TestCoordinatedExpansionGangLifecycle(t *testing.T) {
	for _, sizes := range [][2]int32{{1, 2}, {4, 6}} {
		c, ms, pg := coordinatedExpansionFixture(t, []string{"prefill", "decode"}, []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}, sizes[0], sizes[1])
		require.Equal(t, map[string]int32{"prefill": sizes[0]}, expansionMinima(t, c, ms, pg.Name))
		// A temporary surge cannot unlock a caller. Nor can an unready formal target.
		addExpansionInstance(t, c, ms, pg.Name, "decode", int(sizes[1]), true)
		addExpansionInstance(t, c, ms, pg.Name, "decode", int(sizes[0]), false)
		require.Equal(t, map[string]int32{"prefill": sizes[0]}, expansionMinima(t, c, ms, pg.Name))
		addExpansionInstance(t, c, ms, pg.Name, "decode", int(sizes[0]), true)
		require.Nil(t, expansionMinima(t, c, ms, pg.Name), "one complete formal target unlocks final Gang requirements")
		// Once the caller started, loss of dependency readiness must not reset its first-start gate.
		addExpansionInstance(t, c, ms, pg.Name, "prefill", int(sizes[0]), false)
		require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), pg.Name, "decode", utils.GenerateRoleID("decode", int(sizes[0])), datastore.RoleCreating))
		require.Nil(t, expansionMinima(t, c, ms, pg.Name))
	}
}

func TestCoordinatedExpansionGangDependencyGraphs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		roles        []string
		dependencies []api.RoleRolloutDependency
		ready        []string
		blocked      map[string]int32
	}{
		{"chain", []string{"a", "b", "c"}, []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b"}}, {Role: "b", DependsOn: []string{"c"}}}, nil, map[string]int32{"a": 4}},
		{"chain-closure", []string{"a", "b", "c"}, []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b"}}, {Role: "b", DependsOn: []string{"c"}}}, []string{"b"}, map[string]int32{"a": 4}},
		{"diamond", []string{"a", "b", "c", "d"}, []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b", "c"}}, {Role: "b", DependsOn: []string{"d"}}, {Role: "c", DependsOn: []string{"d"}}}, []string{"b", "d"}, map[string]int32{"a": 4}},
		{"diamond-ready", []string{"a", "b", "c", "d"}, []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b", "c"}}, {Role: "b", DependsOn: []string{"d"}}, {Role: "c", DependsOn: []string{"d"}}}, []string{"b", "c", "d"}, nil},
		{"shared-dependency", []string{"a", "b", "c"}, []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"c"}}, {Role: "b", DependsOn: []string{"c"}}}, nil, map[string]int32{"a": 4, "b": 4}},
		{"independent-roots", []string{"a", "b", "c", "d", "e"}, []api.RoleRolloutDependency{{Role: "a", DependsOn: []string{"c"}}, {Role: "b", DependsOn: []string{"d"}}}, []string{"c"}, map[string]int32{"b": 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ms, pg := coordinatedExpansionFixture(t, tc.roles, tc.dependencies, 4, 6)
			for _, role := range tc.ready {
				addExpansionInstance(t, c, ms, pg.Name, role, 4, true)
			}
			require.Equal(t, tc.blocked, expansionMinima(t, c, ms, pg.Name))
		})
	}
}

func TestCoordinatedExpansionGangScope(t *testing.T) {
	for _, mode := range []string{"no-coordination", "SG-mode", "no-dependencies", "other-scheduler", "explicit-minimum", "foreign-PG", "missing-PG", "new-SG", "deleting-PG", "no-subgroups", "fault-at-final-size", "pure-scale", "shrink"} {
		t.Run(mode, func(t *testing.T) {
			c, ms, pg := coordinatedExpansionFixture(t, []string{"prefill", "decode"}, []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}, 4, 6)
			var expected map[string]int32
			group := pg.Name
			switch mode {
			case "no-coordination":
				ms.Spec.RolloutStrategy.RoleCoordination = nil
			case "SG-mode":
				ms.Spec.RolloutStrategy.Type = api.ServingGroupRollingUpdate
			case "no-dependencies":
				ms.Spec.RolloutStrategy.RoleCoordination.Dependencies = nil
			case "other-scheduler":
				ms.Spec.SchedulerName = "default-scheduler"
			case "explicit-minimum":
				expected = map[string]int32{"prefill": 5}
				ms.Spec.Template.GangPolicy = &api.GangPolicy{MinRoleReplicas: expected}
			case "foreign-PG":
				pg.OwnerReferences[0].UID = "another-owner"
			case "missing-PG":
				require.NoError(t, c.podGroupManager.GetPodGroupInformer().GetIndexer().Delete(pg))
			case "new-SG":
				group = "expansion-1"
			case "deleting-PG":
				pg.DeletionTimestamp = ptr.To(metav1.Now())
			case "no-subgroups":
				pg.Spec.SubGroupPolicy = nil
			case "fault-at-final-size":
				for i := range pg.Spec.SubGroupPolicy {
					pg.Spec.SubGroupPolicy[i].MinSubGroups = ptr.To[int32](6)
				}
			case "pure-scale":
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "test-image:latest"
				}
			case "shrink":
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](3)
				}
			}
			require.Equal(t, expected, expansionMinima(t, c, ms, group))
		})
	}
}

func TestCoordinatedExpansionGangPerGroupAndNewTarget(t *testing.T) {
	c, ms, pg := coordinatedExpansionFixture(t, []string{"prefill", "decode"}, []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}, 4, 6)
	addExpansionInstance(t, c, ms, "expansion-1", "decode", 4, true)
	require.Equal(t, map[string]int32{"prefill": 4}, expansionMinima(t, c, ms, pg.Name), "another group's Ready cannot unlock this group")
	addExpansionInstance(t, c, ms, pg.Name, "decode", 4, true)
	require.Nil(t, expansionMinima(t, c, ms, pg.Name))
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](8)
		ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "target:C"
	}
	require.Equal(t, map[string]int32{"prefill": 4}, expansionMinima(t, c, ms, pg.Name), "B Ready must not count as C Ready")
}

func TestCoordinatedExpansionGangManagerProjection(t *testing.T) {
	for _, coordinated := range []bool{true, false} {
		c, ms, pg := coordinatedExpansionFixture(t, []string{"prefill", "decode"}, []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}, 4, 6, func(old *api.ModelServing) {
			for i := range old.Spec.Template.Roles {
				r := &old.Spec.Template.Roles[i]
				r.WorkerReplicas = 1
				r.EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
				r.WorkerTemplate = r.EntryTemplate.DeepCopy()
				r.WorkerTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
			}
		})
		if !coordinated {
			ms.Spec.RolloutStrategy.RoleCoordination = nil
		}
		crd := &apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "podgroups.scheduling.volcano.sh"},
			Spec: apiextv1.CustomResourceDefinitionSpec{Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Properties: map[string]apiextv1.JSONSchemaProps{
					"spec": {Properties: map[string]apiextv1.JSONSchemaProps{"subGroupPolicy": {}}},
				}}},
			}}}}
		client := volcanofake.NewSimpleClientset(pg.DeepCopy())
		manager := podgroupmanager.NewManager(c.kubeClientSet, client, apiextfake.NewSimpleClientset(crd), nil)
		require.NoError(t, manager.GetPodGroupInformer().GetIndexer().Add(pg))
		c.podGroupManager = manager
		// The existing layout projection supplies one worker per instance. Keep
		// that projection while the real manager calculates all Gang fields.
		projected := ms.DeepCopy()
		for i := range projected.Spec.Template.Roles {
			r := &projected.Spec.Template.Roles[i]
			r.WorkerReplicas = 1
			r.EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
			r.WorkerTemplate = r.EntryTemplate.DeepCopy()
			r.WorkerTemplate.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("50m")
		}
		require.NoError(t, c.createOrUpdatePodGroupByServingGroup(context.Background(), projected, pg.Name))
		actual, err := client.SchedulingV1beta1().PodGroups(ms.Namespace).Get(context.Background(), pg.Name, metav1.GetOptions{})
		require.NoError(t, err)
		minimum := int32(12)
		if coordinated {
			minimum = 10
		}
		require.Equal(t, minimum*2, actual.Spec.MinMember)
		require.Equal(t, int64(minimum)*150, actual.Spec.MinResources.Cpu().MilliValue())
		minima := make(map[string]int32)
		for _, sub := range actual.Spec.SubGroupPolicy {
			minima[sub.Name] = *sub.MinSubGroups
		}
		require.Equal(t, map[string]int32{"prefill": minimum - 6, "decode": 6}, minima)
		require.Nil(t, ms.Spec.Template.GangPolicy)
	}
}

func TestCoordinatedPodGroupUsesAppliedWorkerLayoutUntilReplacement(t *testing.T) {
	c, ms, pg := coordinatedExpansionFixture(t, []string{"prefill", "decode"}, []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}, 1, 2)
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].WorkerReplicas = 1
		ms.Spec.Template.Roles[i].WorkerTemplate = ms.Spec.Template.Roles[i].EntryTemplate.DeepCopy()
	}
	before := ms.DeepCopy()
	projected, err := c.coordinatedPodGroupLayout(ms, pg.Name)
	require.NoError(t, err)
	for _, role := range projected.Spec.Template.Roles {
		require.Zero(t, role.WorkerReplicas)
		require.Equal(t, int32(2), *role.Replicas)
	}
	_, err = utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, utils.ModelServingRevision(ms), ms.Spec.Template.Roles)
	require.NoError(t, err)
	addExpansionInstance(t, c, ms, pg.Name, "decode", 1, true)
	projected, err = c.coordinatedPodGroupLayout(ms, pg.Name)
	require.NoError(t, err)
	require.Zero(t, projected.Spec.Template.Roles[1].WorkerReplicas)
	c.store.DeleteRole(utils.GetNamespaceName(ms), pg.Name, "decode", "decode-0")
	projected, err = c.coordinatedPodGroupLayout(ms, pg.Name)
	require.NoError(t, err)
	require.Equal(t, int32(1), projected.Spec.Template.Roles[1].WorkerReplicas)
	require.Zero(t, projected.Spec.Template.Roles[0].WorkerReplicas)
	require.Equal(t, before, ms)
	projected, err = c.coordinatedPodGroupLayout(ms, "uncreated-group-0")
	require.NoError(t, err)
	require.Equal(t, ms, projected)
}
