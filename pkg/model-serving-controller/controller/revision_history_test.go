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
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func testRevisionPlugin(value string) workloadv1alpha1.PluginSpec {
	return workloadv1alpha1.PluginSpec{
		Name: plugins.DemoPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn,
		Scope:  &workloadv1alpha1.PluginScope{Roles: []string{"decode"}},
		Config: &apiextensionsv1.JSON{Raw: []byte(fmt.Sprintf(`{"annotations":{"revision-test":"%s"}}`, value))},
	}
}

func TestReadyPodWithMissingHistoryRemainsObserved(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	c, err := NewModelServingController(client, kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	ms := semanticRevisionTestModelServing("ready-history-missing", "image")
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{testRevisionPlugin("current")}
	groupName := ms.Name + "-0"
	pod := utils.GenerateEntryPod(ms.Spec.Template.Roles[0], ms, groupName, "decode-0", "missing", "old-role-hash")
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	require.ErrorContains(t, c.handleReadyPod(ms, groupName, pod), "resolve ready Pod revision")
	roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, "decode")
	require.NoError(t, err)
	require.Len(t, roles, 1)
	require.Equal(t, "missing", roles[0].Revision)
	require.Equal(t, datastore.RoleRunning, roles[0].Status)
	ctx := c.withRevisionHistory(context.Background(), ms)
	require.NoError(t, c.manageRollingUpdate(ctx, ms, "desired"))
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), groupName))
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "events" {
			continue // RevisionUnresolved warnings are expected, workload writes are not.
		}
		require.NotEqual(t, "delete", action.GetVerb())
		require.NotEqual(t, "create", action.GetVerb())
	}
}

func TestProtectedRecoveryIgnoresDesiredComparisonCache(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	c, err := NewModelServingController(client, kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	old := semanticRevisionTestModelServing("protected-cache", "old-image")
	old.Spec.Template.Roles[0].WorkerReplicas = 1
	old.Spec.Template.Roles[0].WorkerTemplate = old.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	recordTestRevision(t, client, old, "old")
	groupName := old.Name + "-0"
	entry := utils.GenerateEntryPod(old.Spec.Template.Roles[0], old, groupName, "decode-0", "old", "legacy-typed-role-hash")
	entry, err = client.CoreV1().Pods(old.Namespace).Create(context.Background(), entry, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(entry))
	current := old.DeepCopy()
	current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "new-image"
	ctx := c.withRevisionHistory(context.Background(), current)
	require.Equal(t, templateDifferent, c.compareRoleTemplate(ctx, current,
		datastore.ServingGroup{Name: groupName, Revision: "old"}, current.Spec.Template.Roles[0], datastore.Role{Revision: "old"}))
	require.NoError(t, c.CreatePodsByRole(ctx, current.Spec.Template.Roles[0], current, 0, 0, "old", "unused"))
	pods, err := client.CoreV1().Pods(old.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2, "the existing legacy entry must not block historical worker recreation")
	for _, pod := range pods.Items {
		require.Equal(t, "old-image", pod.Spec.Containers[0].Image)
		require.Equal(t, "old", utils.ObjectRevision(&pod))
	}
}

func TestLegacyPluginBaselinePreventsFirstRollButDetectsLaterChanges(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			ms := semanticRevisionTestModelServing("plugin-migration", "image:v1")
			ms.Spec.SchedulerName = "volcano"
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: strategy}
			prefill := *ms.Spec.Template.Roles[0].DeepCopy()
			prefill.Name = "prefill"
			ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, prefill)
			ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{testRevisionPlugin("original")}
			client := kubefake.NewSimpleClientset()
			original, err := utils.CreateControllerRevision(context.Background(), client, ms, "legacy", ms.Spec.Template.Roles)
			require.NoError(t, err)
			c := &ModelServingController{kubeClientSet: client}
			ctx := c.withRevisionHistory(context.Background(), ms)
			revision, err := c.revisionHistory(ctx, ms).desiredRevision(ctx)
			require.NoError(t, err)
			require.Equal(t, "legacy", revision)
			group := datastore.ServingGroup{Name: ms.Name + "-0", Revision: "legacy"}
			require.Equal(t, templateEquivalent, c.compareServingGroupTemplate(ctx, ms, group, revision))
			for _, desired := range ms.Spec.Template.Roles {
				require.Equal(t, templateEquivalent, c.compareRoleTemplate(ctx, ms, group, desired,
					datastore.Role{Revision: "legacy", RoleTemplateHash: "pre-upgrade-hash"}))
			}
			// A fresh cache models a controller restart: do not adopt the edit as
			// another compatibility baseline.
			ms = ms.DeepCopy()
			ms.Generation++
			ms.Spec.Plugins[0] = testRevisionPlugin("changed")
			ctx = c.withRevisionHistory(context.Background(), ms)
			revision, err = c.revisionHistory(ctx, ms).desiredRevision(ctx)
			require.NoError(t, err)
			require.NotEqual(t, "legacy", revision)
			require.Equal(t, templateDifferent, c.compareServingGroupTemplate(ctx, ms, group, revision))
			require.Equal(t, templateDifferent, c.compareRoleTemplate(ctx, ms, group, ms.Spec.Template.Roles[0], datastore.Role{Revision: "legacy"}))
			require.Equal(t, templateEquivalent, c.compareRoleTemplate(ctx, ms, group, ms.Spec.Template.Roles[1], datastore.Role{Revision: "legacy"}))
			unchanged, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, original.Data, unchanged.Data)
		})
	}
}

func TestPartitionRecoveryRestoresHistoricalPluginChain(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			client := kubefake.NewSimpleClientset()
			c, err := NewModelServingController(client, kthenafake.NewSimpleClientset(), nil, apiextfake.NewSimpleClientset())
			require.NoError(t, err)
			ms := semanticRevisionTestModelServing("plugin-recovery", "image:old")
			ms.Spec.SchedulerName = "volcano"
			ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{testRevisionPlugin("old")}
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: strategy}
			partition := intstr.FromInt32(1)
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				ms.Spec.Replicas = ptr.To[int32](2)
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{Partition: &partition}
			} else {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
				ms.Spec.Template.Roles[0].Partition = &partition
			}
			recordTestRevision(t, client, ms, "old")
			ms = ms.DeepCopy()
			ms.Status.CurrentRevision = "old"
			ms.Spec.Plugins[0] = testRevisionPlugin("new")
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:new"
			recordTestRevision(t, client, ms, "new")
			original := ms.DeepCopy()
			ctx := c.withRevisionHistory(context.Background(), ms)
			if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
				c.store.AddServingGroup(utils.GetNamespaceName(ms), 1, "new")
				err = c.scaleUpServingGroups(ctx, ms, []datastore.ServingGroup{{Name: ms.Name + "-1", Revision: "new"}}, 2, "new")
				require.NoError(t, err)
			} else {
				c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "old")
				c.store.AddRole(utils.GetNamespaceName(ms), ms.Name+"-0", "decode", "decode-1", "new", "new-role")
				c.scaleUpRoles(ctx, ms, ms.Name+"-0", ms.Spec.Template.Roles[0], []datastore.Role{{Name: "decode-1", Revision: "new"}}, 2, 0, "new")
				require.NoError(t, c.revisionHistory(ctx, ms).errors())
			}
			pods, err := client.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 1)
			pod := pods.Items[0]
			require.Equal(t, "old", utils.ObjectRevision(&pod))
			require.Equal(t, "image:old", pod.Spec.Containers[0].Image)
			require.Equal(t, "old", pod.Annotations["revision-test"])
			require.Equal(t, "volcano", pod.Spec.SchedulerName)
			require.Equal(t, original, ms, "historical rendering must not mutate desired spec")
		})
	}
}

// Seed the persisted snapshot that production sync records before reconciling.
// Tests may choose a historical identity to exercise hash drift independently
// of the snapshot contents.
func recordTestRevision(t *testing.T, client kubernetes.Interface, ms *workloadv1alpha1.ModelServing, revision string) {
	t.Helper()
	data, err := utils.BuildRevisionData(ms)
	require.NoError(t, err)
	_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Create(context.Background(), &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: utils.GenerateControllerRevisionName(ms.Name, revision), Namespace: ms.Namespace,
			Labels:          map[string]string{utils.ControllerRevisionLabelKey: ms.Name, utils.ControllerRevisionRevisionLabelKey: revision},
			Annotations:     map[string]string{utils.ControllerRevisionDataVersionAnnotation: utils.ControllerRevisionDataVersionV1},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))},
		},
		Revision: 1, Data: runtime.RawExtension{Raw: data},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func semanticRevisionTestModelServing(name, image string) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			UID:       types.UID(name + "-uid"),
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name:     "decode",
				Replicas: ptr.To[int32](1),
				EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: image}},
				}},
			}}},
		},
	}
}

func TestDesiredRevisionReusesSemanticHistoryAfterHashDrift(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("semantic-upgrade", "image:v1")
	legacyHashInput := ms.DeepCopy().Spec.Template.Roles
	for i := range legacyHashInput {
		legacyHashInput[i].Replicas = nil
		legacyHashInput[i].RollingUpdateConfiguration = workloadv1alpha1.RollingUpdateConfiguration{}
	}
	legacyHash := utils.Revision(legacyHashInput)
	require.NotEqual(t, legacyHash, utils.ModelServingRevision(ms),
		"test requires the legacy struct hash and serialized hash to differ")
	ms.Status.CurrentRevision = legacyHash
	ms.Status.UpdateRevision = legacyHash
	historical := ms.DeepCopy().Spec.Template.Roles
	*historical[0].Replicas = 7

	client := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(ctx, client, ms, legacyHash, historical)
	require.NoError(t, err)
	controller := &ModelServingController{kubeClientSet: client}
	ctx = controller.withRevisionHistory(ctx, ms)

	got, err := controller.revisionHistory(ctx, ms).desiredRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, legacyHash, got)
	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 2, "migration adds one immutable baseline without changing the legacy identity")
	gotAgain, err := controller.revisionHistory(context.Background(), ms).desiredRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, legacyHash, gotAgain)
	listAgain, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, listAgain.Items, 2, "subsequent reconciles must reuse the baseline")
}

func TestDesiredRevisionCreatesHistoryForRealTemplateChange(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("semantic-change", "image:v2")
	ms.Status.UpdateRevision = "old-hash"
	historical := ms.DeepCopy().Spec.Template.Roles
	historical[0].EntryTemplate.Spec.Containers[0].Image = "image:v1"

	client := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(ctx, client, ms, "old-hash", historical)
	require.NoError(t, err)
	controller := &ModelServingController{kubeClientSet: client}
	ctx = controller.withRevisionHistory(ctx, ms)

	got, err := controller.revisionHistory(ctx, ms).desiredRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, utils.ModelServingRevision(ms), got)
	require.NotEqual(t, "old-hash", got)
	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 3, "legacy data, its baseline, and the genuinely changed revision")
}

func TestRoleTemplateHashUsesSemanticHistoryFallback(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("semantic-role", "image:v1")
	client := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(ctx, client, ms, "legacy-hash", ms.Spec.Template.Roles)
	require.NoError(t, err)
	controller := &ModelServingController{kubeClientSet: client}
	ctx = controller.withRevisionHistory(ctx, ms)

	got, ok := controller.roleTemplateHashForComparison(ctx, ms,
		datastore.ServingGroup{Name: "semantic-role-0", Revision: "legacy-hash"},
		"decode",
		datastore.Role{Name: "decode-0", Revision: "legacy-hash", RoleTemplateHash: "legacy-role-hash"},
	)
	require.True(t, ok)
	expected, err := utils.RoleRevisionHash(ms, "decode")
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

func TestServingGroupComparisonUsesEachObservedRoleRevision(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("partial-role", "prefill:v1")
	ms.Spec.Template.Roles[0].Name = "prefill"
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
	decode := *ms.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	decode.EntryTemplate.Spec.Containers[0].Image = "decode:v2"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, decode)

	oldRoles := ms.DeepCopy().Spec.Template.Roles
	oldRoles[1].EntryTemplate.Spec.Containers[0].Image = "decode:v1"
	client := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(ctx, client, ms, "decode-old", oldRoles)
	require.NoError(t, err)
	controller := &ModelServingController{kubeClientSet: client, store: datastore.New()}
	key := utils.GetNamespaceName(ms)
	groupName := utils.GenerateServingGroupName(ms.Name, 0)
	controller.store.AddServingGroup(key, 0, "target-revision")
	controller.store.AddRole(key, groupName, "decode", "decode-0", "decode-old", "legacy-decode-hash")
	ctx = controller.withRevisionHistory(ctx, ms)

	require.Equal(t, templateDifferent, controller.compareServingGroupTemplate(ctx, ms,
		datastore.ServingGroup{Name: groupName, Revision: "target-revision"}, "target-revision"),
		"a target ServingGroup revision must not hide a partially updated Role")
}

func TestMissingHistoryPausesTemplateRolloutAndRequestsRetry(t *testing.T) {
	controller, err := NewModelServingController(
		kubefake.NewSimpleClientset(),
		kthenafake.NewSimpleClientset(),
		nil,
		apiextfake.NewSimpleClientset(),
	)
	require.NoError(t, err)
	ms := semanticRevisionTestModelServing("missing-history", "image:v2")
	key := utils.GetNamespaceName(ms)
	controller.store.AddServingGroup(key, 0, "missing-old-hash")
	require.NoError(t, controller.store.UpdateServingGroupStatus(key, "missing-history-0", datastore.ServingGroupRunning))
	ctx := controller.withRevisionHistory(context.Background(), ms)

	require.NoError(t, controller.manageRollingUpdate(ctx, ms, "new-hash"))
	require.Equal(t, datastore.ServingGroupRunning, controller.store.GetServingGroupStatus(key, "missing-history-0"))
	require.Error(t, controller.revisionHistory(ctx, ms).errors(), "unresolved history must be returned by sync for workqueue retry")
}

func TestScaleDownContinuesWhenDesiredRevisionCannotBePersisted(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kubeClient.PrependReactor("create", "controllerrevisions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected ControllerRevision persistence failure")
	})
	controller, err := NewModelServingController(
		kubeClient,
		kthenafake.NewSimpleClientset(),
		nil,
		apiextfake.NewSimpleClientset(),
	)
	require.NoError(t, err)
	ms := semanticRevisionTestModelServing("safe-scale-down", "image:v1")
	require.NoError(t, controller.modelServingsInformer.GetIndexer().Add(ms))
	key := utils.GetNamespaceName(ms)
	for groupOrdinal := 0; groupOrdinal < 2; groupOrdinal++ {
		groupName := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
		controller.store.AddServingGroup(key, groupOrdinal, "old-hash")
		require.NoError(t, controller.store.UpdateServingGroupStatus(key, groupName, datastore.ServingGroupRunning))
		for roleOrdinal := 0; roleOrdinal < 2; roleOrdinal++ {
			roleID := utils.GenerateRoleID("decode", roleOrdinal)
			controller.store.AddRole(key, groupName, "decode", roleID, "old-hash", "old-role-hash")
			require.NoError(t, controller.store.UpdateRoleStatus(key, groupName, "decode", roleID, datastore.RoleRunning))
		}
	}

	err = controller.syncModelServing(context.Background(), namespacedKey(ms.Namespace, ms.Name))
	require.ErrorContains(t, err, "injected ControllerRevision persistence failure")
	groups, err := controller.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	require.Len(t, groups, 1, "one ServingGroup must be scaled down even when revision persistence fails")
	for _, group := range groups {
		roles, err := controller.store.GetRoleList(key, group.Name, "decode")
		require.NoError(t, err)
		require.Len(t, roles, 1, "one Role replica must be scaled down even when revision persistence fails")
	}
}

func TestScaleDownContinuesWhenComputedHistoryIsInvalid(t *testing.T) {
	for _, fault := range []string{"different-template", "foreign-owner", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			ms := semanticRevisionTestModelServing("invalid-history-"+fault, "image:v1")
			computed := utils.ModelServingRevision(ms)
			owner := ms.DeepCopy()
			roles := ms.DeepCopy().Spec.Template.Roles
			switch fault {
			case "different-template":
				roles[0].EntryTemplate.Spec.Containers[0].Image = "wrong-image"
			case "foreign-owner":
				owner.UID = "previous-owner"
			}

			kubeClient := kubefake.NewSimpleClientset()
			cr, err := utils.CreateControllerRevision(ctx, kubeClient, owner, computed, roles)
			require.NoError(t, err)
			if fault == "malformed" {
				cr.Data.Raw = []byte(`{"data":"not-role-templates"}`)
				_, err = kubeClient.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
				require.NoError(t, err)
			}

			controller, err := NewModelServingController(
				kubeClient,
				kthenafake.NewSimpleClientset(),
				nil,
				apiextfake.NewSimpleClientset(),
			)
			require.NoError(t, err)
			require.NoError(t, controller.modelServingsInformer.GetIndexer().Add(ms))
			key := utils.GetNamespaceName(ms)
			for groupOrdinal := 0; groupOrdinal < 2; groupOrdinal++ {
				groupName := utils.GenerateServingGroupName(ms.Name, groupOrdinal)
				controller.store.AddServingGroup(key, groupOrdinal, "old-hash")
				require.NoError(t, controller.store.UpdateServingGroupStatus(key, groupName, datastore.ServingGroupRunning))
				for roleOrdinal := 0; roleOrdinal < 2; roleOrdinal++ {
					roleID := utils.GenerateRoleID("decode", roleOrdinal)
					controller.store.AddRole(key, groupName, "decode", roleID, "old-hash", "old-role-hash")
					require.NoError(t, controller.store.UpdateRoleStatus(key, groupName, "decode", roleID, datastore.RoleRunning))
				}
			}

			err = controller.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
			require.Error(t, err)
			groups, err := controller.store.GetServingGroupByModelServing(key)
			require.NoError(t, err)
			require.Len(t, groups, 1)
			for _, group := range groups {
				instances, err := controller.store.GetRoleList(key, group.Name, "decode")
				require.NoError(t, err)
				require.Len(t, instances, 1)
			}
		})
	}
}

func TestPartitionSemanticHistoryUsesCurrentReplicaControls(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("partition-replicas", "image:v1")
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	partition := intstr.FromInt32(1)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
			Partition: &partition,
		},
	}
	historical := ms.DeepCopy().Spec.Template.Roles
	historical[0].Replicas = ptr.To[int32](1)
	kubeClient := kubefake.NewSimpleClientset()
	_, err := utils.CreateControllerRevision(ctx, kubeClient, ms, "legacy-hash", historical)
	require.NoError(t, err)
	controller := &ModelServingController{kubeClientSet: kubeClient, store: datastore.New()}
	controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "legacy-hash")

	roles, err := controller.rolesForServingGroupReadiness(ms, utils.GenerateServingGroupName(ms.Name, 0))
	require.NoError(t, err)
	require.Equal(t, int32(2), *roles[0].Replicas,
		"replica-only changes must not be pinned to an immutable historical template")
}

func TestRevisionFailureDoesNotBypassServingGroupPartition(t *testing.T) {
	ctx := context.Background()
	ms := semanticRevisionTestModelServing("partition-failure", "image:v1")
	partition := intstr.FromInt32(1)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
			Partition: &partition,
		},
	}
	computed := utils.ModelServingRevision(ms)
	kubeClient := kubefake.NewSimpleClientset()
	cr, err := utils.CreateControllerRevision(ctx, kubeClient, ms, computed, ms.Spec.Template.Roles)
	require.NoError(t, err)
	cr.Data.Raw = []byte(`{"data":"not-role-templates"}`)
	_, err = kubeClient.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
	require.NoError(t, err)
	controller, err := NewModelServingController(
		kubeClient,
		kthenafake.NewSimpleClientset(),
		nil,
		apiextfake.NewSimpleClientset(),
	)
	require.NoError(t, err)
	require.NoError(t, controller.modelServingsInformer.GetIndexer().Add(ms))
	key := utils.GetNamespaceName(ms)
	groupName := utils.GenerateServingGroupName(ms.Name, 0)
	controller.store.AddServingGroup(key, 0, computed)
	controller.store.AddRole(key, groupName, "decode", "decode-0", computed, "old-role-hash")
	controller.store.AddRole(key, groupName, "decode", "decode-1", computed, "old-role-hash")

	err = controller.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
	require.Error(t, err)
	roles, err := controller.store.GetRoleList(key, groupName, "decode")
	require.NoError(t, err)
	require.Len(t, roles, 2, "unresolved protected history must not use the current template to scale Roles")
}
