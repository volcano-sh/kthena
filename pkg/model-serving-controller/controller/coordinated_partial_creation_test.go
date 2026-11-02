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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestCoordinatedPartialExpansionReservesOneStartAndRepairsItsWorker(t *testing.T) {
	for _, method := range []api.UpdateMethod{api.RecreateUpdateMethod, api.InPlaceUpdateMethod} {
		t.Run(string(method), func(t *testing.T) {
			f := newCoordinatedInPlaceFixture(t)
			f.controller.initialSync.Store(true)
			f.ms.Spec.RolloutStrategy.UpdateMethod = method
			for i := range f.ms.Spec.Template.Roles {
				f.ms.Spec.Template.Roles[i].Replicas = ptr.To[int32](8)
				f.ms.Spec.Template.Roles[i].MaxUnavailable = ptr.To(intstr.FromInt(8))
			}
			_, err := f.controller.modelServingClient.WorkloadV1alpha1().ModelServings(f.ms.Namespace).Update(context.Background(), f.ms, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, f.controller.modelServingsInformer.GetIndexer().Update(f.ms))
			client := f.controller.kubeClientSet.(*kubefake.Clientset)
			failedWorker := map[string]bool{}
			client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
				// Emulate API-assigned UIDs so a replay must preserve the actual
				// accepted incarnation, rather than succeeding with empty UIDs.
				if pod.UID == "" {
					pod.UID = types.UID(pod.Name + "-accepted")
				}
				name := utils.GetRoleName(pod)
				_, ordinal := utils.GetParentNameAndOrdinal(utils.GetRoleID(pod))
				if utils.ObjectRevision(pod) == f.newRev && ordinal == 4 && pod.Labels[api.EntryLabelKey] != utils.Entry && !failedWorker[name] {
					failedWorker[name] = true
					return true, nil, fmt.Errorf("injected one worker failure after accepting %s entry", name)
				}
				return false, nil, nil
			})
			oldReservations := func(name string) int {
				t.Helper()
				reserved := 0
				for ordinal := 0; ordinal < 4; ordinal++ {
					if method == api.RecreateUpdateMethod {
						if f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, name, utils.GenerateRoleID(name, ordinal)) == datastore.RoleDeleting {
							reserved++
						}
						continue
					}
					for member := 0; member < 2; member++ {
						state := inplace.StateOf(f.pod(t, name, ordinal, member))
						if state.InProgress() && state.Revision == f.newRev {
							reserved++
							break
						}
					}
				}
				return reserved
			}

			// Creation remains an ordinary expansion. Once its entry is
			// accepted, that partial formal target must consume one of the
			// two cumulative starts available to subsequent old replacements.
			require.NoError(t, f.controller.syncModelServing(context.Background(), f.ms.Namespace+"/"+f.ms.Name))
			acceptedUIDs := map[string]types.UID{}
			reservationsBeforeRepair := map[string]int{}
			for _, role := range f.ms.Spec.Template.Roles {
				require.True(t, failedWorker[role.Name])
				entry := f.pod(t, role.Name, 4, 0)
				require.NotEmpty(t, entry.UID)
				acceptedUIDs[role.Name] = entry.UID
				require.Equal(t, f.newRev, utils.ObjectRevision(entry))
				require.False(t, utils.IsPodRunningAndReady(entry))
				_, err := client.CoreV1().Pods(f.ms.Namespace).Get(context.Background(), utils.GeneratePodName(f.group, utils.GenerateRoleID(role.Name, 4), 1), metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err))
				// Normal post-startup Pending events do not rebuild missing Role
				// records. Accounting cannot depend on that event rescuing it.
				require.NoError(t, f.controller.podsInformer.GetIndexer().Update(entry))
				f.controller.addPod(entry)
				reservations := oldReservations(role.Name)
				reservationsBeforeRepair[role.Name] = reservations
				t.Logf("role=%s acceptedPartial=1 oldReplacementReservations=%d cumulativeStarts=%d allowedAtReady0=2", role.Name, reservations, 1+reservations)
				assert.Equal(t, datastore.RoleCreating, f.controller.store.GetRoleStatus(utils.GetNamespaceName(f.ms), f.group, role.Name, utils.GenerateRoleID(role.Name, 4)))
				assert.LessOrEqual(t, reservations, 1, "the accepted entry spends one start before an independent old replacement may be selected")
				assert.LessOrEqual(t, 1+reservations, 2)
				// A conservative planning snapshot may defer all old work until
				// the next round. A fresh observation must still account for the
				// accepted partial plus every old replacement already admitted.
				ctx := withCoordinatedRound(context.Background(), f.ms)
				instances, err := f.controller.store.GetRoleList(utils.GetNamespaceName(f.ms), f.group, role.Name)
				require.NoError(t, err)
				population, err := f.controller.stableRolePopulationInRound(ctx, f.ms, f.group, role, 4, instances, nil)
				require.NoError(t, err)
				state := f.controller.resolveRoleRolloutState(ctx, f.ms, datastore.ServingGroup{Name: f.group, Revision: f.oldRev}, role, 4, instances, 0, true, nil, population)
				assert.Equal(t, 1+reservations, state.versionStarted)
				assert.Zero(t, state.versionReady)
			}
			if t.Failed() {
				return
			}

			// The single injected failure has cleared. A cache delivery plus
			// normal reconciliation must finish the same ordinal and UID,
			// without spending another old-replacement start. Remaining ordinary
			// expansion may proceed under the existing creation boundary.
			f.syncPodCache(t)
			require.NoError(t, f.controller.syncModelServing(context.Background(), f.ms.Namespace+"/"+f.ms.Name))
			for _, role := range f.ms.Spec.Template.Roles {
				entry, worker := f.pod(t, role.Name, 4, 0), f.pod(t, role.Name, 4, 1)
				require.Equal(t, acceptedUIDs[role.Name], entry.UID)
				for _, pod := range []*corev1.Pod{entry, worker} {
					require.True(t, utils.IsOwnedByModelServingWithUID(pod, f.ms.UID))
					require.Equal(t, f.newRev, utils.ObjectRevision(pod))
					require.Equal(t, utils.CalRoleTemplateHash(role), utils.ObjectRoleTemplateHash(pod))
					require.Nil(t, pod.DeletionTimestamp)
					require.False(t, utils.IsPodRunningAndReady(pod), "repair has no invented kubelet Ready credit")
				}
				require.Equal(t, reservationsBeforeRepair[role.Name], oldReservations(role.Name), "worker replay cannot grant additional old replacements")
			}
		})
	}
}
