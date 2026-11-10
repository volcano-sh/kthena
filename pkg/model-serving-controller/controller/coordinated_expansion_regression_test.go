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
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

// Production's existing boundary allows ordinary expansion through the boolean
// dependency gate. Its observed formal targets then consume the coordinator's
// remaining old-replacement allowance; creation is not individually skew-limited.
func TestCoordinatedExpansionCountsFormalStartsAgainstLaterReplacement(t *testing.T) {
	for _, method := range []api.UpdateMethod{api.RecreateUpdateMethod, api.InPlaceUpdateMethod} {
		t.Run(string(method), func(t *testing.T) {
			f := newCoordinatedInPlaceFixture(t)
			f.ms.Spec.RolloutStrategy.UpdateMethod = method
			require.Empty(t, f.ms.Spec.RolloutStrategy.RoleCoordination.Dependencies)
			require.Nil(t, f.ms.Spec.Template.GangPolicy)
			for i := range f.ms.Spec.Template.Roles {
				f.ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](8)
				// U8/S0 leaves healthy deletion credit. The maxSkew assertion
				// must not pass merely because unready expansion spent U3.
				f.ms.Spec.Template.Roles[i].MaxUnavailable = ptr.To(intstr.FromInt(8))
			}
			require.Equal(t, f.newRev, utils.ModelServingRevision(f.ms), "replica expansion does not change the target template's revision")
			_, err := f.controller.modelServingClient.WorkloadV1alpha1().ModelServings(f.ms.Namespace).Update(context.Background(), f.ms, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, f.controller.modelServingsInformer.GetIndexer().Update(f.ms))
			historical, err := f.controller.roleSpecsFromRevision(f.ms, f.oldRev)
			require.NoError(t, err)
			for _, role := range historical {
				require.EqualValues(t, 4, *role.Replicas)
			}

			before, err := f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			original := make(map[string]*corev1.Pod)
			for i := range before.Items {
				original[before.Items[i].Name] = before.Items[i].DeepCopy()
			}
			// An actual manager must retain the full default Gang without
			// dependencies, even when the published previous minimum was four.
			pg := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
				Namespace: f.ms.Namespace, Name: f.group,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.ms, api.ModelServingKind)},
			}, Spec: schedulingv1beta1.PodGroupSpec{MinMember: 16}}
			for _, role := range f.ms.Spec.Template.Roles {
				pg.Spec.SubGroupPolicy = append(pg.Spec.SubGroupPolicy, schedulingv1beta1.SubGroupPolicySpec{
					Name: role.Name, MinSubGroups: ptr.To[int32](4), SubGroupSize: ptr.To[int32](2),
				})
			}
			crd := &apiextv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "podgroups.scheduling.volcano.sh"},
				Spec: apiextv1.CustomResourceDefinitionSpec{Versions: []apiextv1.CustomResourceDefinitionVersion{{
					Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Properties: map[string]apiextv1.JSONSchemaProps{
						"spec": {Properties: map[string]apiextv1.JSONSchemaProps{"subGroupPolicy": {}}},
					}}},
				}}}}
			volcano := volcanofake.NewSimpleClientset(pg)
			f.controller.podGroupManager = podgroupmanager.NewManager(f.controller.kubeClientSet, volcano, apiextfake.NewSimpleClientset(crd), nil)
			require.NoError(t, f.controller.podGroupManager.GetPodGroupInformer().GetIndexer().Add(pg.DeepCopy()))
			client := f.controller.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			require.NoError(t, f.controller.syncModelServing(context.Background(), f.ms.Namespace+"/"+f.ms.Name))
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "pods" {
					continue
				}
				if creation, ok := action.(clienttesting.CreateAction); ok {
					_, existed := original[creation.GetObject().(*corev1.Pod).Name]
					require.False(t, existed, "ordinary expansion must not recreate an old member")
				} else {
					require.Contains(t, []string{"get", "list", "watch"}, action.GetVerb(), "old Pods cannot be patched or deleted before skew permits replacement")
				}
			}
			pods, err := f.controller.kubeClientSet.CoreV1().Pods(f.ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 32)
			for _, role := range f.ms.Spec.Template.Roles {
				starts, targetReady, oldEntries := 0, 0, 0
				for i := range pods.Items {
					pod := &pods.Items[i]
					if utils.GetRoleName(pod) != role.Name || pod.Labels[api.EntryLabelKey] != utils.Entry {
						continue
					}
					require.True(t, utils.IsOwnedByModelServingWithUID(pod, f.ms.UID))
					require.Nil(t, pod.DeletionTimestamp)
					require.NotContains(t, pod.Annotations, surgeAnnotation, "new ordinals below N=8 are formal expansion, not temporary surge")
					target := utils.ObjectRevision(pod) == f.newRev
					if target {
						starts++
						if utils.IsPodRunningAndReady(pod) {
							targetReady++
						}
					} else {
						oldEntries++
					}
				}
				t.Logf("role=%s desired=8 oldDesired=4 targetReady=%d ordinaryExpansionStarts=%d oldEntries=%d", role.Name, targetReady, starts, oldEntries)
				require.Zero(t, targetReady, "no kubelet progress occurs during this single reconciliation")
				require.Equal(t, 4, starts)
				require.Equal(t, 4, oldEntries)
			}
			for name, previous := range original {
				actual, err := client.CoreV1().Pods(f.ms.Namespace).Get(context.Background(), name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, previous.UID, actual.UID)
				require.Equal(t, previous.Spec.Containers[0].Image, actual.Spec.Containers[0].Image)
				require.Equal(t, f.oldRev, utils.ObjectRevision(actual))
				require.Equal(t, utils.ObjectRoleTemplateHash(previous), utils.ObjectRoleTemplateHash(actual))
				require.Nil(t, actual.DeletionTimestamp)
				require.False(t, inplace.InProgress(actual))
			}
			actualPG, err := volcano.SchedulingV1beta1().PodGroups(f.ms.Namespace).Get(context.Background(), f.group, metav1.GetOptions{})
			require.NoError(t, err)
			require.EqualValues(t, 32, actualPG.Spec.MinMember)
			for _, subgroup := range actualPG.Spec.SubGroupPolicy {
				require.EqualValues(t, 8, *subgroup.MinSubGroups)
				require.EqualValues(t, 2, *subgroup.SubGroupSize)
			}

			inspect := func(ready, remaining int) {
				t.Helper()
				ctx := withCoordinatedRound(context.Background(), f.ms)
				require.NoError(t, f.controller.refreshCoordinatedAvailabilityInRound(ctx, f.ms))
				policy, err := f.controller.resolveRoleRolloutPolicy(ctx, f.ms, f.newRev)
				require.NoError(t, err)
				for _, role := range f.ms.Spec.Template.Roles {
					instances, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), f.group, role.Name)
					require.NoError(t, err)
					population, err := f.controller.stableRolePopulationInRound(ctx, f.ms, f.group, role, 4, instances, nil)
					require.NoError(t, err)
					state := f.controller.resolveRoleRolloutState(ctx, f.ms, datastore.ServingGroup{Name: f.group, Revision: f.oldRev}, role, 4, instances, 0, true, nil, population)
					require.Equal(t, 8, state.versionTotal)
					require.Equal(t, 4, state.versionStarted)
					require.Equal(t, ready, state.versionReady)
					limits, exists := policy.group(f.group).role(role.Name)
					require.True(t, exists)
					require.Equal(t, remaining, limits.remainingDeletions)
					_, unavailable := f.controller.outdatedRolesInRound(ctx, f.ms, datastore.ServingGroup{Name: f.group, Revision: f.oldRev}, role, instances)
					budget, err := roleRolloutBudget(role, instances, unavailable)
					require.NoError(t, err)
					require.Greater(t, budget.healthy, 0)
					require.Greater(t, budget.total, 0, "local U8 credit must not mask the coordinated limit")
					t.Logf("role=%s targetReady=%d/%d formalStarted=%d remainingOldReplacement=%d localHealthyCredit=%d localTotalCredit=%d", role.Name, state.versionReady, state.versionTotal, state.versionStarted, limits.remainingDeletions, budget.healthy, budget.total)
				}
			}
			inspect(0, 0)

			// At 3/8 target Ready for both Roles, the coordinator allows five
			// cumulative starts: ceil((3/8+25%)*8)=5. Four expansion starts have
			// already spent four, so exactly one old instance may now advance.
			for _, role := range f.ms.Spec.Template.Roles {
				for ordinal := 4; ordinal <= 6; ordinal++ {
					for member := 0; member < 2; member++ {
						pod := f.pod(t, role.Name, ordinal, member)
						pod.Status.Phase = corev1.PodRunning
						pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
						_, err := client.CoreV1().Pods(f.ms.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
						require.NoError(t, err)
					}
				}
			}
			f.syncPodCache(t)
			inspect(3, 1)
			client.ClearActions()
			require.NoError(t, f.controller.syncModelServing(context.Background(), f.ms.Namespace+"/"+f.ms.Name))
			mutations := 0
			deletedRoles := map[string]int{}
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "pods" || action.GetVerb() == "get" || action.GetVerb() == "list" || action.GetVerb() == "watch" {
					continue
				}
				if method == api.RecreateUpdateMethod && action.GetVerb() == "patch" {
					patch := action.(clienttesting.PatchAction)
					previous, exists := original[patch.GetName()]
					require.True(t, exists)
					_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(previous))
					require.Equal(t, 3, ordinal)
					assertDeletionScopePatch(t, action, utils.DeletionScopeRole, previous.UID)
					continue
				}
				mutations++
				if method == api.RecreateUpdateMethod {
					deletion, ok := action.(clienttesting.DeleteAction)
					require.True(t, ok, "Recreate deletes each original member with its UID")
					previous, exists := original[deletion.GetName()]
					require.True(t, exists)
					_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(previous))
					require.Equal(t, 3, ordinal, "only the highest old ordinal may advance")
					require.NotNil(t, deletion.GetDeleteOptions().Preconditions)
					require.Equal(t, previous.UID, *deletion.GetDeleteOptions().Preconditions.UID)
					deletedRoles[utils.GetRoleID(previous)]++
				} else {
					patch, ok := action.(clienttesting.PatchAction)
					require.True(t, ok, "InPlace must preserve the admitted Role's Pod identities")
					previous, exists := original[patch.GetName()]
					require.True(t, exists)
					_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(previous))
					require.Equal(t, 3, ordinal)
				}
			}
			if method == api.RecreateUpdateMethod {
				require.Equal(t, 4, mutations)
				require.Equal(t, map[string]int{"prefill-3": 2, "decode-3": 2}, deletedRoles, "two members together spend each Role's single replacement allowance")
			} else {
				require.Equal(t, 4, mutations, "entry and worker patches reserve one Role start together")
			}
			for name, previous := range original {
				actual, err := client.CoreV1().Pods(f.ms.Namespace).Get(context.Background(), name, metav1.GetOptions{})
				_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(previous))
				if method == api.RecreateUpdateMethod && ordinal == 3 {
					require.True(t, apierrors.IsNotFound(err), "both selected members were deleted")
					continue
				}
				require.NoError(t, err)
				require.Equal(t, previous.UID, actual.UID)
				if method == api.InPlaceUpdateMethod && ordinal == 3 {
					require.Equal(t, "img:v2", actual.Spec.Containers[0].Image)
					require.True(t, inplace.InProgress(actual))
					require.Equal(t, f.newRev, inplace.StateOf(actual).Revision)
				} else {
					require.Equal(t, previous.Spec.Containers[0].Image, actual.Spec.Containers[0].Image)
					require.False(t, inplace.InProgress(actual))
				}
				if ordinal < 3 {
					require.Equal(t, datastore.RoleRunning, f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, utils.GetRoleName(previous), utils.GetRoleID(previous)))
				}
			}
		})
	}
}
