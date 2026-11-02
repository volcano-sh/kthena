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
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func historicalPluginFixture(t *testing.T) (*ModelServingController, *kubefake.Clientset, *workload.ModelServing) {
	t.Helper()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "history", Namespace: "default", UID: "ms-uid"}, Spec: workload.ModelServingSpec{
		Replicas: ptr.To[int32](1), SchedulerName: "volcano",
		Plugins:  []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
		Template: workload.ServingGroup{Roles: []workload.Role{{Name: "infer", Replicas: ptr.To[int32](1), EntryTemplate: workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "current"}}}}}}},
	}}
	client := kubefake.NewSimpleClientset()
	c, err := NewModelServingController(client, kthenafake.NewSimpleClientset(ms), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	return c, client, ms
}

func historicalPluginRevision(t *testing.T, ms *workload.ModelServing, revision string, v1 bool) *appsv1.ControllerRevision {
	t.Helper()
	var raw []byte
	var err error
	annotations := map[string]string{}
	if v1 {
		raw, err = utils.BuildRevisionData(ms)
		annotations[utils.ControllerRevisionDataVersionAnnotation] = utils.ControllerRevisionDataVersionV1
	} else {
		raw, err = json.Marshal(map[string]interface{}{"data": ms.Spec.Template.Roles})
	}
	require.NoError(t, err)
	return &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: utils.GenerateControllerRevisionName(ms.Name, revision), Namespace: ms.Namespace, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}, Data: runtime.RawExtension{Raw: raw}}
}

func TestHistoricalRoleSyncUsesAppliedLayout(t *testing.T) {
	for _, mode := range []workload.RolloutStrategyType{workload.ServingGroupRollingUpdate, workload.RoleRollingUpdate} {
		for _, v1 := range []bool{false, true} {
			for _, oldWorker := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/v1=%t/oldWorker=%t", mode, v1, oldWorker), func(t *testing.T) {
					c, client, ms := historicalPluginFixture(t)
					ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode}
					old := ms.DeepCopy()
					old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
					if oldWorker {
						old.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
					} else {
						ms.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
					}
					_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), historicalPluginRevision(t, old, "old", v1), metav1.CreateOptions{})
					require.NoError(t, err)
					current := utils.ModelServingRevision(ms)
					sgRev, roleRev := "old", current
					if mode == workload.RoleRollingUpdate {
						sgRev, roleRev = current, "old"
					}
					c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, sgRev)
					c.store.AddRole(utils.GetNamespaceName(ms), "history-0", "infer", "infer-0", roleRev, utils.CalRoleTemplateHash(old.Spec.Template.Roles[0]))
					pod := utils.GenerateEntryPod(old.Spec.Template.Roles[0], old, "history-0", "infer-0", "old", "old-hash")
					pod.UID = "entry-uid"
					_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
					client.ClearActions()
					require.NoError(t, c.syncRoleReplicas(context.Background(), ms, current, nil))
					services, err := client.CoreV1().Services(ms.Namespace).List(context.Background(), metav1.ListOptions{})
					require.NoError(t, err)
					if oldWorker {
						require.Len(t, services.Items, 2)
					} else {
						require.Empty(t, services.Items)
					}
					for _, a := range client.Actions() {
						require.False(t, a.Matches("create", "pods"), "sync must preserve healthy Pod")
					}
					actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, pod, actual)
				})
			}
		}
	}
}

func TestHistoricalPodCreationUsesV1PluginsAndScheduler(t *testing.T) {
	for _, oldPlugin := range []bool{false, true} {
		for _, sameRevision := range []bool{false, true} {
			t.Run(fmt.Sprintf("oldPlugin=%t/sameRevision=%t", oldPlugin, sameRevision), func(t *testing.T) {
				c, client, ms := historicalPluginFixture(t)
				ms.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "current-worker"}}}}
				old := ms.DeepCopy()
				old.Spec.SchedulerName = "historical-scheduler"
				old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
				old.Spec.Template.Roles[0].WorkerReplicas = 1
				old.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "old-worker"
				if oldPlugin {
					ms.Spec.Plugins = nil
				} else {
					old.Spec.Plugins = nil
				}
				revision := "old"
				if sameRevision {
					// The standalone revision name still hashes raw Roles. Keep
					// the same Role input; plugin/scheduler history is separate.
					ms.Spec.Template.Roles = old.Spec.Template.DeepCopy().Roles
					revision = utils.ModelServingRevision(ms)
				}
				_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), historicalPluginRevision(t, old, revision, true), metav1.CreateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.CreatePodsByRole(context.Background(), ms.Spec.Template.Roles[0], ms, 0, 0, revision, utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])))
				applied, err := c.modelServingForRevision(context.Background(), ms, revision)
				require.NoError(t, err)
				pods, err := client.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, 2)
				expectedHash := utils.CalRoleTemplateHash(applied.Spec.Template.Roles[0])
				if sameRevision {
					expectedHash = utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])
				}
				for _, pod := range pods.Items {
					require.Equal(t, "historical-scheduler", pod.Spec.SchedulerName)
					require.Equal(t, revision, utils.ObjectRevision(&pod))
					require.Equal(t, expectedHash, pod.Labels[workload.RoleTemplateHashLabelKey])
					require.Contains(t, []string{"old", "old-worker"}, pod.Spec.Containers[0].Image)
					if oldPlugin {
						require.Equal(t, plugins.DiscoveryServiceName("history-0-infer-0-0"), pod.Spec.Subdomain)
					} else {
						require.Empty(t, pod.Spec.Subdomain)
					}
				}
				services, err := client.CoreV1().Services(ms.Namespace).List(context.Background(), metav1.ListOptions{})
				require.NoError(t, err)
				if oldPlugin {
					require.Len(t, services.Items, 2)
				} else {
					require.Empty(t, services.Items)
				}
				if sameRevision {
					require.Equal(t, "old", ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)
					require.EqualValues(t, 1, ms.Spec.Template.Roles[0].WorkerReplicas)
				} else {
					require.Equal(t, "current", ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)
					require.Zero(t, ms.Spec.Template.Roles[0].WorkerReplicas)
				}
			})
		}
	}
}

func TestHistoricalContextRejectsUnusableHistoryBeforeEffects(t *testing.T) {
	for _, path := range []string{"sync", "create"} {
		for _, kind := range []string{"missing", "foreign-uid", "foreign-name", "foreign-kind", "foreign-api", "malformed", "missing-role", "unknown-version"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				c, client, ms := historicalPluginFixture(t)
				ms.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
				old := ms.DeepCopy()
				if kind == "missing-role" {
					old.Spec.Template.Roles[0].Name = "different"
				}
				cr := historicalPluginRevision(t, old, "old", true)
				switch kind {
				case "foreign-uid":
					cr.OwnerReferences[0].UID = "another"
				case "foreign-name":
					cr.OwnerReferences[0].Name = "another"
				case "foreign-kind":
					cr.OwnerReferences[0].Kind = "Another"
				case "foreign-api":
					cr.OwnerReferences[0].APIVersion = "another/v1"
				case "malformed":
					cr.Data.Raw = []byte(`{"spec":`)
				case "unknown-version":
					cr.Annotations[utils.ControllerRevisionDataVersionAnnotation] = "v99"
				}
				if kind != "missing" {
					_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), cr, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "old")
				c.store.AddRole(utils.GetNamespaceName(ms), "history-0", "infer", "infer-0", "old", "old-hash")
				client.ClearActions()
				var err error
				if path == "sync" {
					err = c.syncRoleReplicas(context.Background(), ms, utils.ModelServingRevision(ms), nil)
				} else {
					err = c.CreatePodsByRole(context.Background(), ms.Spec.Template.Roles[0], ms, 0, 0, "old", "old-hash")
				}
				require.Error(t, err)
				for _, a := range client.Actions() {
					require.NotContains(t, []string{"create", "update", "delete", "patch"}, a.GetVerb())
				}
			})
		}
	}
}

func TestHistoricalContextPreservesCurrentCountsAndLegacyPluginInputs(t *testing.T) {
	for _, v1 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v1=%t", v1), func(t *testing.T) {
			c, client, ms := historicalPluginFixture(t)
			old := ms.DeepCopy()
			old.Spec.Replicas = ptr.To[int32](4)
			old.Spec.Template.Roles[0].Replicas = ptr.To[int32](7)
			old.Spec.SchedulerName = "old-scheduler"
			old.Spec.Plugins = nil
			old.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
			_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), historicalPluginRevision(t, old, "old", v1), metav1.CreateOptions{})
			require.NoError(t, err)
			result, err := c.modelServingForRevision(context.Background(), ms, "old")
			require.NoError(t, err)
			require.Equal(t, int32(1), *result.Spec.Replicas)
			require.Equal(t, int32(1), *result.Spec.Template.Roles[0].Replicas)
			require.NotNil(t, result.Spec.Template.Roles[0].WorkerTemplate)
			if v1 {
				require.Empty(t, result.Spec.Plugins)
				require.Equal(t, "old-scheduler", result.Spec.SchedulerName)
			} else {
				require.Equal(t, ms.Spec.Plugins, result.Spec.Plugins)
				require.Equal(t, ms.Spec.SchedulerName, result.Spec.SchedulerName)
			}
		})
	}
}

func TestHistoricalPartialRepairPreservesObservedTemplateHash(t *testing.T) {
	c, client, ms := historicalPluginFixture(t)
	ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: workload.RoleRollingUpdate}
	old := ms.DeepCopy()
	old.Spec.Template.Roles[0].WorkerReplicas = 1
	old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
	old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: workload.GroupSizeEnv, Value: "user-overwritten"}}
	old.Spec.Template.Roles[0].WorkerTemplate = old.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	rawHash := utils.CalRoleTemplateHash(old.Spec.Template.Roles[0])
	_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), historicalPluginRevision(t, old, "old", true), metav1.CreateOptions{})
	require.NoError(t, err)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, utils.ModelServingRevision(ms))
	c.store.AddRole(utils.GetNamespaceName(ms), "history-0", "infer", "infer-0", "old", rawHash)
	pod := utils.GenerateEntryPod(old.Spec.Template.Roles[0], old, "history-0", "infer-0", "old", rawHash)
	pod.UID = "entry-uid"
	pod.Spec.Hostname = pod.Name
	pod.Spec.Subdomain = plugins.DiscoveryServiceName(pod.Name)
	_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	client.ClearActions()
	require.NoError(t, c.syncRoleReplicas(context.Background(), ms, utils.ModelServingRevision(ms), nil))
	pods, err := client.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2)
	actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, pod, actual)
	worker, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), "history-0-infer-0-1", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, rawHash, worker.Labels[workload.RoleTemplateHashLabelKey])
	require.Equal(t, "old", worker.Spec.Containers[0].Image)
	gets := 0
	for _, a := range client.Actions() {
		if a.Matches("get", "controllerrevisions") {
			gets++
		}
	}
	require.Equal(t, 1, gets, "repair and sync share the same resolved historical context")
}

func TestHistoricalRoleSyncUsesV1PluginList(t *testing.T) {
	for _, oldPlugin := range []bool{false, true} {
		t.Run(fmt.Sprintf("oldPlugin=%t", oldPlugin), func(t *testing.T) {
			c, client, ms := historicalPluginFixture(t)
			ms.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
			old := ms.DeepCopy()
			if oldPlugin {
				ms.Spec.Plugins = nil
			} else {
				old.Spec.Plugins = nil
			}
			_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), historicalPluginRevision(t, old, "old", true), metav1.CreateOptions{})
			require.NoError(t, err)
			c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "old")
			c.store.AddRole(utils.GetNamespaceName(ms), "history-0", "infer", "infer-0", "old", "old-hash")
			pod := utils.GenerateEntryPod(old.Spec.Template.Roles[0], old, "history-0", "infer-0", "old", "old-hash")
			_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			require.NoError(t, c.syncRoleReplicas(context.Background(), ms, utils.ModelServingRevision(ms), nil))
			services, err := client.CoreV1().Services(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			if oldPlugin {
				require.Len(t, services.Items, 2)
			} else {
				require.Empty(t, services.Items)
			}
		})
	}
}

func TestHistoricalPartitionCreationKeepsPodAndStoreHashAligned(t *testing.T) {
	for _, mode := range []workload.RolloutStrategyType{workload.ServingGroupRollingUpdate, workload.RoleRollingUpdate} {
		t.Run(string(mode), func(t *testing.T) {
			c, client, ms := historicalPluginFixture(t)
			partition := intstr.FromInt(1)
			ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode, RollingUpdateConfiguration: &workload.RollingUpdateConfiguration{Partition: &partition}}
			ms.Spec.Template.Roles[0].RollingUpdateConfiguration.Partition = &partition
			old := ms.DeepCopy()
			old.Spec.SchedulerName = "old-scheduler"
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old"
			old.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
			ms.Spec.Plugins = nil
			ms.Status.CurrentRevision = "old"
			cr := historicalPluginRevision(t, old, "old", true)
			_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), cr, metav1.CreateOptions{})
			require.NoError(t, err)
			if mode == workload.ServingGroupRollingUpdate {
				require.NoError(t, c.scaleUpServingGroups(context.Background(), ms, nil, 1, utils.ModelServingRevision(ms)))
			} else {
				c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, utils.ModelServingRevision(ms))
				require.NoError(t, c.scaleUpRoles(context.Background(), ms, "history-0", ms.Spec.Template.Roles[0], nil, 1, 0, utils.ModelServingRevision(ms), true))
			}
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), "history-0", "infer")
			require.NoError(t, err)
			require.Len(t, roles, 1)
			pod, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), "history-0-infer-0-0", metav1.GetOptions{})
			require.NoError(t, err)
			applied, err := utils.ApplyRevision(ms, cr)
			require.NoError(t, err)
			hash := utils.CalRoleTemplateHash(applied.Spec.Template.Roles[0])
			require.Equal(t, hash, pod.Labels[workload.RoleTemplateHashLabelKey])
			require.Equal(t, hash, roles[0].RoleTemplateHash)
			require.Equal(t, "old", roles[0].Revision)
			require.Equal(t, "old", pod.Spec.Containers[0].Image)
			require.Equal(t, "old-scheduler", pod.Spec.SchedulerName)
			require.Equal(t, plugins.DiscoveryServiceName(pod.Name), pod.Spec.Subdomain)
		})
	}
}

func TestCurrentV1CreationKeepsPodAndStoreHashAligned(t *testing.T) {
	for _, path := range []string{"role-scale-up", "serving-group-create", "protected-role", "protected-group"} {
		t.Run(path, func(t *testing.T) {
			c, client, ms := historicalPluginFixture(t)
			ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: workload.RoleRollingUpdate}
			maxSurge := intstr.FromInt(1)
			ms.Spec.Template.Roles[0].RollingUpdateConfiguration.MaxSurge = &maxSurge
			ms.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: workload.GroupSizeEnv, Value: "overwritten"}}
			revision := utils.ModelServingRevision(ms)
			if path == "protected-role" || path == "protected-group" {
				partition := intstr.FromInt(1)
				ms.Status.CurrentRevision = revision
				ms.Spec.Template.Roles[0].RollingUpdateConfiguration.Partition = &partition
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workload.RollingUpdateConfiguration{Partition: &partition}
				if path == "protected-group" {
					ms.Spec.RolloutStrategy.Type = workload.ServingGroupRollingUpdate
				}
			}
			cr := historicalPluginRevision(t, ms, revision, true)
			_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), cr, metav1.CreateOptions{})
			require.NoError(t, err)
			c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, revision)
			if path == "role-scale-up" || path == "protected-role" {
				require.NoError(t, c.scaleUpRoles(context.Background(), ms, "history-0", ms.Spec.Template.Roles[0], nil, 1, 0, revision, true))
			} else if path == "protected-group" {
				require.NoError(t, c.scaleUpServingGroups(context.Background(), ms, nil, 1, revision))
			} else {
				require.NoError(t, c.CreatePodsForServingGroup(context.Background(), ms, 0, revision, ms.Spec.Template.Roles))
			}
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), "history-0", "infer")
			require.NoError(t, err)
			require.Len(t, roles, 1)
			pod, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), "history-0-infer-0-0", metav1.GetOptions{})
			require.NoError(t, err)
			applied, err := utils.ApplyRevision(ms, cr)
			require.NoError(t, err)
			hash := utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])
			require.NotEqual(t, utils.CalRoleTemplateHash(applied.Spec.Template.Roles[0]), hash, "fixture must expose normalization-induced hash differences")
			require.Equal(t, hash, pod.Labels[workload.RoleTemplateHashLabelKey])
			require.Equal(t, hash, roles[0].RoleTemplateHash)
			require.Equal(t, revision, roles[0].Revision)
			// Releasing partition must not turn a current v1 instance into an
			// outdated Role or allocate unnecessary surge capacity.
			ms.Spec.RolloutStrategy.Type = workload.RoleRollingUpdate
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration = nil
			ms.Spec.Template.Roles[0].RollingUpdateConfiguration.Partition = nil
			sg := datastore.ServingGroup{Name: "history-0", Revision: revision}
			require.False(t, c.hasUpdateableOutdatedRole(ms, sg.Name, ms.Spec.Template.Roles[0], roles))
			outdated, _ := c.outdatedRoles(ms, sg, ms.Spec.Template.Roles[0], roles)
			require.Empty(t, outdated)
			require.Empty(t, c.findOutdatedRolesInServingGroups(ms, []datastore.ServingGroup{sg}, revision))
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), sg.Name, "infer", "infer-0", datastore.RoleRunning))
			require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), sg.Name, datastore.ServingGroupRunning))
			_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(context.Background(), ms, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
			client.ClearActions()
			require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
			for _, action := range client.Actions() {
				require.False(t, action.Matches("create", "pods") || action.Matches("delete", "pods"), "current v1 instance must not surge or roll")
			}
			updated, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, revision, updated.Status.CurrentRevision)
			require.Equal(t, revision, updated.Status.UpdateRevision)
			changed := ms.DeepCopy()
			changed.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "genuinely-new-image"
			require.True(t, c.hasUpdateableOutdatedRole(changed, sg.Name, changed.Spec.Template.Roles[0], roles), "real template changes must remain outdated")
			pods, err := client.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
		})
	}
}
