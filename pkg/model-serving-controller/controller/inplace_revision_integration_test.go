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
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type inPlaceHistoryFixture struct {
	c            *ModelServingController
	old, current *workloadv1alpha1.ModelServing
	group        string
	uids         map[string]types.UID
}

func newInPlaceHistoryFixture(t *testing.T, strategy workloadv1alpha1.RolloutStrategyType) *inPlaceHistoryFixture {
	t.Helper()
	f := &inPlaceHistoryFixture{c: newAppliedHistoryController(t)}
	f.old = appliedHistoryWorkload(strategy, "image:v1", 1)
	f.current = appliedHistoryWorkload(strategy, "image:v2", 1)
	for _, ms := range []*workloadv1alpha1.ModelServing{f.old, f.current} {
		ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
		ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1))}
		ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
		ms.Spec.Template.Roles[0].WorkerTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
	}
	f.current.Status.CurrentRevision = "old"
	f.group = utils.GenerateServingGroupName(f.old.Name, 0)
	recordTestRevision(t, f.c.kubeClientSet, f.old, "old")
	f.uids = seedAppliedRole(t, f.c, f.old, f.old.Spec.Template.Roles[0], 0, 0, "old", -1)
	require.NoError(t, f.c.store.UpdateRoleStatus(utils.GetNamespaceName(f.old), f.group, "decode", "decode-0", datastore.RoleRunning))
	require.NoError(t, f.c.store.UpdateServingGroupStatus(utils.GetNamespaceName(f.old), f.group, datastore.ServingGroupRunning))
	_, err := f.c.modelServingClient.WorkloadV1alpha1().ModelServings(f.current.Namespace).Create(context.Background(), f.current, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.c.modelServingsInformer.GetIndexer().Add(f.current))
	return f
}

func (f *inPlaceHistoryFixture) sync(t *testing.T) {
	t.Helper()
	require.NoError(t, f.c.syncModelServing(context.Background(), namespacedKey(f.current.Namespace, f.current.Name)))
}

func (f *inPlaceHistoryFixture) pods(t *testing.T) []corev1.Pod {
	t.Helper()
	list, err := f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	return list.Items
}

func (f *inPlaceHistoryFixture) runPatchedContainers(t *testing.T) {
	t.Helper()
	for _, pod := range f.pods(t) {
		if !inplace.StateOf(&pod).InProgress() {
			continue
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "main", Image: pod.Spec.Containers[0].Image, ContainerID: "containerd://target-" + pod.Name,
			ImageID: "sha256:target", Ready: true, RestartCount: 1,
		}}
		updated, err := f.c.kubeClientSet.CoreV1().Pods(pod.Namespace).UpdateStatus(context.Background(), &pod, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.True(t, inplace.Completed(updated))
		require.NoError(t, f.c.podsInformer.GetIndexer().Update(updated))
	}
}

func (f *inPlaceHistoryFixture) requireUnitRevision(t *testing.T, revision string) {
	t.Helper()
	roles, err := f.c.store.GetRoleList(utils.GetNamespaceName(f.current), f.group, "decode")
	require.NoError(t, err)
	require.Len(t, roles, 1)
	require.Equal(t, revision, roles[0].Revision)
	if f.current.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate {
		got, found := f.c.store.GetServingGroupRevision(utils.GetNamespaceName(f.current), f.group)
		require.True(t, found)
		require.Equal(t, revision, got)
	}
}

func TestInPlaceIntegrationUsesResolvedHistoryAcrossRestart(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, identity := range []string{"semantic-alias", "hash-collision"} {
			t.Run(fmt.Sprintf("%s/%s", strategy, identity), func(t *testing.T) {
				f := newInPlaceHistoryFixture(t, strategy)
				raw := utils.ModelServingRevision(f.current)
				var collisionData []byte
				if identity == "semantic-alias" {
					recordTestRevision(t, f.c.kubeClientSet, f.current, "retained-target-identity")
				} else {
					wrong := f.current.DeepCopy()
					wrong.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "unrelated-image"
					recordTestRevision(t, f.c.kubeClientSet, wrong, raw)
					cr, err := utils.GetControllerRevision(context.Background(), f.c.kubeClientSet, f.current, raw)
					require.NoError(t, err)
					cr.OwnerReferences[0].UID = "previous-owner"
					collisionData = append([]byte(nil), cr.Data.Raw...)
					_, err = f.c.kubeClientSet.AppsV1().ControllerRevisions(cr.Namespace).Update(context.Background(), cr, metav1.UpdateOptions{})
					require.NoError(t, err)
				}
				f.sync(t)
				resolved, err := f.c.revisionHistory(context.Background(), f.current).desiredRevision(context.Background())
				require.NoError(t, err)
				require.NotEqual(t, raw, resolved)
				if identity == "semantic-alias" {
					require.Equal(t, "retained-target-identity", resolved)
				}
				for _, pod := range f.pods(t) {
					state := inplace.StateOf(&pod)
					require.NotNil(t, state)
					require.Equal(t, resolved, state.Revision, "Start must use the persisted identity chosen by desiredRevision")
					require.Equal(t, "old", utils.ObjectRevision(&pod))
					require.False(t, state.Done)
				}
				f.runPatchedContainers(t)
				// Rebuild informer/store observations over the same API objects.
				restarted := newAppliedHistoryController(t)
				restarted.kubeClientSet, restarted.modelServingClient = f.c.kubeClientSet, f.c.modelServingClient
				require.NoError(t, restarted.modelServingsInformer.GetIndexer().Add(f.current))
				hash, err := utils.RoleRevisionHash(f.old, "decode")
				require.NoError(t, err)
				restarted.store.AddServingGroup(utils.GetNamespaceName(f.current), 0, "old")
				restarted.store.AddRole(utils.GetNamespaceName(f.current), f.group, "decode", "decode-0", "old", hash)
				for _, pod := range f.pods(t) {
					require.NoError(t, restarted.podsInformer.GetIndexer().Add(pod.DeepCopy()))
				}
				f.c = restarted
				f.sync(t)
				f.requireUnitRevision(t, resolved)
				expectedHash, err := utils.RoleRevisionHash(f.current, "decode")
				require.NoError(t, err)
				for _, pod := range f.pods(t) {
					require.Equal(t, f.uids[pod.Name], pod.UID)
					require.Equal(t, resolved, utils.ObjectRevision(&pod))
					require.Equal(t, expectedHash, utils.ObjectRoleTemplateHash(&pod), "completion uses the same Role identity as ordinary creation")
					require.Equal(t, resolved, inplace.StateOf(&pod).Revision)
					require.True(t, inplace.StateOf(&pod).Done)
				}
				if identity == "hash-collision" {
					cr, err := utils.GetControllerRevision(context.Background(), f.c.kubeClientSet, f.current, raw)
					require.NoError(t, err)
					require.NotNil(t, cr)
					require.Equal(t, types.UID("previous-owner"), cr.OwnerReferences[0].UID)
					require.Equal(t, collisionData, cr.Data.Raw, "a colliding foreign revision must neither be adopted nor rewritten")
				}
			})
		}
	}
}

func TestInPlaceIntegrationRequiresObservedWholeUnit(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, phase := range []string{"complete", "repair"} {
			t.Run(fmt.Sprintf("%s/%s", strategy, phase), func(t *testing.T) {
				f := newInPlaceHistoryFixture(t, strategy)
				recordTestRevision(t, f.c.kubeClientSet, f.current, "target-alias")
				pods := f.pods(t)
				for i := range pods {
					pod := &pods[i]
					if pod.Name == utils.GeneratePodName(f.group, "decode-0", 1) {
						require.NoError(t, f.c.podsInformer.GetIndexer().Delete(pod))
						continue
					}
					_, err := inplace.Start(context.Background(), f.c.kubeClientSet, pod, map[string]string{"main": "image:v2"}, "target-alias", time.Now())
					require.NoError(t, err)
				}
				f.runPatchedContainers(t) // Only the entry is patched; worker stays API-only.
				if phase == "repair" {
					entry, err := f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), utils.GeneratePodName(f.group, "decode-0", 0), metav1.GetOptions{})
					require.NoError(t, err)
					require.NoError(t, inplace.Complete(context.Background(), f.c.kubeClientSet, entry, "target-alias", ""))
					entry, err = f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), entry.Name, metav1.GetOptions{})
					require.NoError(t, err)
					require.NoError(t, f.c.podsInformer.GetIndexer().Update(entry))
				}
				f.sync(t)
				f.requireUnitRevision(t, "old")
				entry, err := f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), utils.GeneratePodName(f.group, "decode-0", 0), metav1.GetOptions{})
				require.NoError(t, err)
				if phase == "complete" {
					require.Equal(t, "old", utils.ObjectRevision(entry), "informer lag must not flip the only observed member")
					require.False(t, inplace.StateOf(entry).Done)
				}
				worker, err := f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), utils.GeneratePodName(f.group, "decode-0", 1), metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, "old", utils.ObjectRevision(worker))
				require.NoError(t, f.c.podsInformer.GetIndexer().Add(worker))
				f.sync(t)
				worker, err = f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), worker.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NotNil(t, inplace.StateOf(worker), "the late historical worker must join the same in-place update")
				require.Equal(t, "target-alias", inplace.StateOf(worker).Revision)
				f.runPatchedContainers(t)
				f.sync(t)
				f.requireUnitRevision(t, "target-alias")
				for _, pod := range f.pods(t) {
					require.Equal(t, f.uids[pod.Name], pod.UID)
					require.Equal(t, "target-alias", utils.ObjectRevision(&pod))
					require.True(t, inplace.StateOf(&pod).Done)
				}
			})
		}
	}
}

func TestInPlaceIntegrationRejectsSubstituteMembers(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, phase := range []string{"complete", "repair"} {
			for _, invalid := range []string{"extra", "foreign", "terminating"} {
				t.Run(fmt.Sprintf("%s/%s/%s", strategy, phase, invalid), func(t *testing.T) {
					f := newInPlaceHistoryFixture(t, strategy)
					recordTestRevision(t, f.c.kubeClientSet, f.current, "target-alias")
					entry, err := f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), utils.GeneratePodName(f.group, "decode-0", 0), metav1.GetOptions{})
					require.NoError(t, err)
					_, err = inplace.Start(context.Background(), f.c.kubeClientSet, entry, map[string]string{"main": "image:v2"}, "target-alias", time.Now())
					require.NoError(t, err)
					f.runPatchedContainers(t)
					entry, err = f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), entry.Name, metav1.GetOptions{})
					require.NoError(t, err)
					if phase == "repair" {
						require.NoError(t, inplace.Complete(context.Background(), f.c.kubeClientSet, entry, "target-alias", ""))
						entry, err = f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), entry.Name, metav1.GetOptions{})
						require.NoError(t, err)
						require.NoError(t, f.c.podsInformer.GetIndexer().Update(entry))
					}
					worker, err := f.c.kubeClientSet.CoreV1().Pods(f.current.Namespace).Get(context.Background(), utils.GeneratePodName(f.group, "decode-0", 1), metav1.GetOptions{})
					require.NoError(t, err)
					require.NoError(t, f.c.podsInformer.GetIndexer().Delete(worker))
					worker.Labels[workloadv1alpha1.RevisionLabelKey] = "target-alias"
					worker.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: `{"revision":"target-alias","done":true}`}
					worker.Spec.Containers[0].Image = "image:v2"
					switch invalid {
					case "extra":
						worker.Name = utils.GeneratePodName(f.group, "decode-0", 2)
						worker.UID = "extra-worker"
						worker.ResourceVersion = ""
						worker, err = f.c.kubeClientSet.CoreV1().Pods(worker.Namespace).Create(context.Background(), worker, metav1.CreateOptions{})
					case "foreign":
						worker.OwnerReferences[0].UID = "foreign-owner"
						worker, err = f.c.kubeClientSet.CoreV1().Pods(worker.Namespace).Update(context.Background(), worker, metav1.UpdateOptions{})
					case "terminating":
						now := metav1.Now()
						worker.DeletionTimestamp = &now
						worker, err = f.c.kubeClientSet.CoreV1().Pods(worker.Namespace).Update(context.Background(), worker, metav1.UpdateOptions{})
					}
					require.NoError(t, err)
					require.NoError(t, f.c.podsInformer.GetIndexer().Add(worker))
					f.sync(t)
					f.requireUnitRevision(t, "old")
					if phase == "complete" {
						entry, err = f.c.kubeClientSet.CoreV1().Pods(entry.Namespace).Get(context.Background(), entry.Name, metav1.GetOptions{})
						require.NoError(t, err)
						require.Equal(t, "old", utils.ObjectRevision(entry), "a substitute member must not authorize Complete")
					}
				})
			}
		}
	}
}

func TestInPlaceIntegrationCanonicalRoleRevertRecreatesAbandonedImage(t *testing.T) {
	c := newAppliedHistoryController(t)
	old := appliedHistoryWorkload(workloadv1alpha1.RoleRollingUpdate, "image:v1", 1)
	old.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
	old.Spec.Template.Roles[0].WorkerTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
	other := *old.Spec.Template.Roles[0].DeepCopy()
	other.Name = "prefill"
	old.Spec.Template.Roles = append(old.Spec.Template.Roles, other)
	old.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
	middle := old.DeepCopy()
	middle.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
	middle.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "image:v2"
	reverted := old.DeepCopy()
	reverted.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "image:v3"
	reverted.Spec.Template.Roles[1].WorkerTemplate.Spec.Containers[0].Image = "image:v3"
	for revision, ms := range map[string]*workloadv1alpha1.ModelServing{"old": old, "middle": middle, "reverted": reverted} {
		recordTestRevision(t, c.kubeClientSet, ms, revision)
	}
	group := utils.GenerateServingGroupName(old.Name, 0)
	for _, role := range old.Spec.Template.Roles {
		seedAppliedRole(t, c, old, role, 0, 0, "old", -1)
	}
	f := &inPlaceHistoryFixture{c: c, old: old, current: reverted, group: group}
	for _, pod := range f.pods(t) {
		if utils.GetRoleName(&pod) != "decode" {
			continue
		}
		canonicalHash, err := utils.RoleRevisionHash(old, "decode")
		require.NoError(t, err)
		require.Equal(t, canonicalHash, utils.ObjectRoleTemplateHash(&pod))
		_, err = inplace.Start(context.Background(), c.kubeClientSet, &pod, map[string]string{"main": "image:v2"}, "middle", time.Now())
		require.NoError(t, err)
	}
	f.runPatchedContainers(t)
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(reverted.Namespace).Create(context.Background(), reverted, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(reverted))
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()
	f.sync(t)
	require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(reverted), group, "decode", "decode-0"), "a semantic Role revert cannot leave the abandoned image permanently settled")
	deletes := 0
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "pods" && action.GetVerb() == "delete-collection" {
			selector := action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels
			pod := utils.GenerateEntryPod(old.Spec.Template.Roles[0], old, group, "decode-0", "old", "")
			require.True(t, selector.Matches(labels.Set(pod.Labels)))
			deletes++
		}
	}
	require.Equal(t, 1, deletes)
}

// This plugin's existing Pod mutations survive an image patch, but a changed
// configuration still changes the revision and must pass image-only eligibility.
type integrationInPlacePlugin struct{ plugins.Plugin }

func (*integrationInPlacePlugin) SupportsInPlaceUpdate() bool { return true }

func TestInPlaceIntegrationUsesTrustedSchedulerAndPluginHistory(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, format := range []string{"canonical", "pinned-legacy"} {
			for _, change := range []string{"scheduler", "plugin-this-role", "plugin-other-role"} {
				t.Run(fmt.Sprintf("%s/%s/%s", strategy, format, change), func(t *testing.T) {
					c := newAppliedHistoryController(t)
					c.pluginsRegistry = plugins.NewRegistry()
					c.pluginsRegistry.Register("integration-env", func(spec workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
						p, err := plugins.NewDemoPlugin(spec)
						return &integrationInPlacePlugin{Plugin: p}, err
					})
					old := appliedHistoryWorkload(strategy, "image:v1", 0)
					old.Spec.SchedulerName = "volcano"
					old.Spec.Template.Roles[0].EntryTemplate.Spec.RestartPolicy = corev1.RestartPolicyAlways
					other := *old.Spec.Template.Roles[0].DeepCopy()
					other.Name = "prefill"
					old.Spec.Template.Roles = append(old.Spec.Template.Roles, other)
					old.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
					old.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt(1))}
					scope := "decode"
					if change == "plugin-other-role" {
						scope = "prefill"
					}
					old.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
						Name: "integration-env", Type: workloadv1alpha1.PluginTypeBuiltIn,
						Scope:  &workloadv1alpha1.PluginScope{Roles: []string{scope}},
						Config: &apiextensionsv1.JSON{Raw: []byte(`{"env":[{"name":"PLUGIN_MODE","value":"old"}]}`)},
					}}
					if format == "canonical" {
						recordTestRevision(t, c.kubeClientSet, old, "old")
					} else {
						cr, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, old, "old", old.Spec.Template.Roles)
						require.NoError(t, err)
						_, err = utils.EnsureRevisionBaseline(context.Background(), c.kubeClientSet, old, cr)
						require.NoError(t, err, "pin legacy non-Role inputs before the desired spec changes")
					}
					require.NoError(t, c.CreatePodsForServingGroup(c.withRevisionHistory(context.Background(), old), old, 0, "old", old.Spec.Template.Roles))
					group := utils.GenerateServingGroupName(old.Name, 0)
					pods, err := c.kubeClientSet.CoreV1().Pods(old.Namespace).List(context.Background(), metav1.ListOptions{})
					require.NoError(t, err)
					for i := range pods.Items {
						pod := &pods.Items[i]
						pod.UID = types.UID("created-" + pod.Name)
						pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
						updated, err := c.kubeClientSet.CoreV1().Pods(old.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
						require.NoError(t, err)
						require.NoError(t, c.podsInformer.GetIndexer().Add(updated))
						require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(old), group, utils.GetRoleName(pod), utils.GetRoleID(pod), datastore.RoleRunning))
					}
					require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(old), group, datastore.ServingGroupRunning))
					current := old.DeepCopy()
					current.Status.CurrentRevision = "old"
					current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
					if change == "scheduler" {
						current.Spec.SchedulerName = "default-scheduler"
					} else {
						current.Spec.Plugins[0].Config.Raw = []byte(`{"env":[{"name":"PLUGIN_MODE","value":"new"}]}`)
					}
					_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(current.Namespace).Create(context.Background(), current, metav1.CreateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
					require.True(t, c.updatesInPlace(current), "fixture plugin supports in-place; fallback must be a semantic eligibility decision")
					client := c.kubeClientSet.(*kubefake.Clientset)
					client.ClearActions()
					require.NoError(t, c.syncModelServing(context.Background(), namespacedKey(current.Namespace, current.Name)))
					decode, err := c.kubeClientSet.CoreV1().Pods(current.Namespace).Get(context.Background(), utils.GeneratePodName(group, "decode-0", 0), metav1.GetOptions{})
					require.NoError(t, err)
					decodeDeleted := false
					for _, action := range client.Actions() {
						if action.GetResource().Resource == "pods" && action.GetVerb() == "delete-collection" {
							decodeDeleted = decodeDeleted || action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels.Matches(labels.Set(decode.Labels))
						}
					}
					require.Equal(t, strategy != workloadv1alpha1.RoleRollingUpdate || change != "plugin-other-role", decodeDeleted, "assert actual recreation selection, since fake DeleteCollection leaves tracker Pods intact")
					if strategy == workloadv1alpha1.RoleRollingUpdate && change == "plugin-other-role" {
						require.NotNil(t, inplace.StateOf(decode), "a plugin scoped to another Role must not block this Role's image-only update")
						require.True(t, inplace.StateOf(decode).InProgress())
						require.NotEqual(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(current), group, "decode", "decode-0"))
					} else {
						require.Nil(t, inplace.StateOf(decode), "scheduler or applicable plugin changes are not image-only")
						if strategy == workloadv1alpha1.ServingGroupRollingUpdate {
							require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(utils.GetNamespaceName(current), group))
						} else {
							require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(current), group, "decode", "decode-0"))
						}
					}
				})
			}
		}
	}
}
