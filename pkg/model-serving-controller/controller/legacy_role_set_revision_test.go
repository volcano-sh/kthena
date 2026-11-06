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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestLegacyPersistedRoleSetCleanup(t *testing.T) {
	for _, change := range []string{"removed", "renamed"} {
		for _, unknownSibling := range []bool{false, true} {
			for _, obsoleteDeleting := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/unknown-sibling-%t/already-deleting-%t", change, unknownSibling, obsoleteDeleting), func(t *testing.T) {
					c := newAppliedHistoryController(t)
					old := appliedHistoryWorkload(workloadv1alpha1.RoleRollingUpdate, "image:v1", 1)
					obsolete := *old.Spec.Template.Roles[0].DeepCopy()
					obsolete.Name = "prefill"
					old.Spec.Template.Roles = append(old.Spec.Template.Roles, obsolete)
					current := old.DeepCopy()
					current.Spec.Template.Roles = current.Spec.Template.Roles[:1]
					if change == "renamed" {
						renamed := *obsolete.DeepCopy()
						renamed.Name = "new-prefill"
						current.Spec.Template.Roles = append(current.Spec.Template.Roles, renamed)
					}
					current.Status.CurrentRevision = "old"
					recordTestRevision(t, c.kubeClientSet, old, "old")
					recordTestRevision(t, c.kubeClientSet, current, "target")
					group := utils.GenerateServingGroupName(current.Name, 0)
					key := utils.GetNamespaceName(current)
					decodeRevision := "old"
					if unknownSibling {
						decodeRevision = "missing"
					}
					uids := map[string]types.UID{}
					for _, fixture := range []struct {
						workload *workloadv1alpha1.ModelServing
						role     workloadv1alpha1.Role
						revision string
					}{{old, old.Spec.Template.Roles[0], decodeRevision}, {old, obsolete, "old"}} {
						for name, uid := range seedAppliedRole(t, c, fixture.workload, fixture.role, 0, 0, fixture.revision, -1) {
							uids[name] = uid
						}
						require.NoError(t, c.store.UpdateRoleStatus(key, group, fixture.role.Name, utils.GenerateRoleID(fixture.role.Name, 0), datastore.RoleRunning))
					}
					if change == "renamed" {
						renamed := current.Spec.Template.Roles[1]
						for name, uid := range seedAppliedRole(t, c, current, renamed, 0, 0, "target", -1) {
							uids[name] = uid
						}
						require.NoError(t, c.store.UpdateRoleStatus(key, group, renamed.Name, utils.GenerateRoleID(renamed.Name, 0), datastore.RoleRunning))
					}
					if obsoleteDeleting {
						require.NoError(t, c.store.UpdateRoleStatus(key, group, obsolete.Name, "prefill-0", datastore.RoleDeleting))
					}
					require.NoError(t, c.store.UpdateServingGroupRevision(key, group, "old"))
					require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
					// Seed a legacy persisted state directly. Current admission still
					// forbids online changes to the Role-name set; this tests existing
					// controller cleanup when old Role records survive that state.
					_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
					client := c.kubeClientSet.(*kubefake.Clientset)
					client.ClearActions()
					err = c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name))
					if unknownSibling {
						require.ErrorContains(t, err, "ControllerRevision is missing", "cleanup must retain the unknown desired Role's retry")
					} else {
						require.NoError(t, err)
					}
					deletions := 0
					for _, action := range client.Actions() {
						if action.GetResource().Resource != "pods" || action.GetVerb() != "delete" {
							continue
						}
						deletion := action.(kubetesting.DeleteAction)
						require.Contains(t, []string{utils.GeneratePodName(group, "prefill-0", 0), utils.GeneratePodName(group, "prefill-0", 1)}, deletion.GetName(), "cleanup may target only removed historical Role members")
						require.Equal(t, uids[deletion.GetName()], *deletion.GetDeleteOptions().Preconditions.UID)
						deletions++
					}
					require.Equal(t, 2, deletions, "unfinished cleanup resumes from the same observed member UIDs")
					obsoleteRoles, err := c.store.GetRoleList(key, group, "prefill")
					require.NoError(t, err)
					require.Len(t, obsoleteRoles, 1)
					require.Equal(t, datastore.RoleDeleting, obsoleteRoles[0].Status)
					decodeRoles, err := c.store.GetRoleList(key, group, "decode")
					require.NoError(t, err)
					require.Len(t, decodeRoles, 1)
					require.Equal(t, decodeRevision, decodeRoles[0].Revision)
					require.NotEqual(t, datastore.RoleDeleting, decodeRoles[0].Status)
					if unknownSibling {
						revision, found := c.store.GetServingGroupRevision(key, group)
						require.True(t, found)
						require.Equal(t, "old", revision, "unknown plus equivalent Roles cannot advance the group identity")
					}
					pods, err := client.CoreV1().Pods(current.Namespace).List(context.Background(), metav1.ListOptions{})
					require.NoError(t, err)
					for _, pod := range pods.Items {
						require.Equal(t, uids[pod.Name], pod.UID, "survivors retain their actual Pod identity")
					}
				})
			}
		}
	}
}
