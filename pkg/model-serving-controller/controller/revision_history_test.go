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
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

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
	require.Len(t, list.Items, 1, "hash drift must not create another ControllerRevision")
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
	require.Len(t, list.Items, 2, "a real template change must persist a new ControllerRevision")
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
	require.Equal(t, utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0]), got)
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
