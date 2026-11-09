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

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func auxiliaryFixture(t *testing.T) (*ModelServingController, *kubefake.Clientset, *workload.ModelServing, *corev1.Service) {
	t.Helper()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "default", UID: types.UID("ms-uid")}, Spec: workload.ModelServingSpec{Replicas: ptr.To[int32](0)}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "cleanup-0-infer-0-0", Namespace: ms.Namespace, UID: types.UID("service-uid"), Labels: map[string]string{
		workload.ModelServingNameLabelKey: ms.Name, workload.GroupNameLabelKey: "cleanup-0", workload.RoleLabelKey: "infer", workload.RoleIDKey: "infer-0",
	}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone}}
	client := kubefake.NewSimpleClientset(service)
	c, err := NewModelServingController(client, kthenafake.NewSimpleClientset(ms), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	return c, client, ms, service
}

func TestCleanupRetrySurvivesImmediatePodCallback(t *testing.T) {
	for _, sg := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%t", sg), func(t *testing.T) {
			c, client, ms, svc := auxiliaryFixture(t)
			nsn := utils.GetNamespaceName(ms)
			c.store.AddRole(nsn, "cleanup-0", "infer", "infer-0", "v1", "hash")
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: ms.Namespace, UID: types.UID("original-uid"), Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}}
			_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			client.PrependReactor("delete", "pods", func(kt.Action) (bool, runtime.Object, error) {
				require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
				require.True(t, c.handleDeletionInProgress(ms, "cleanup-0", "infer", "infer-0"))
				// Even with all Pods already absent the callback must retain the identity.
				require.NotEqual(t, datastore.RoleNotFound, c.store.GetRoleStatus(nsn, "cleanup-0", "infer", "infer-0"))
				return false, nil, nil
			})
			denied := true
			client.PrependReactor("delete", "services", func(a kt.Action) (bool, runtime.Object, error) {
				require.Equal(t, svc.UID, *a.(kt.DeleteAction).GetDeleteOptions().Preconditions.UID)
				if denied {
					return true, nil, apierrors.NewForbidden(corev1.Resource("services"), svc.Name, fmt.Errorf("injected"))
				}
				return false, nil, nil
			})
			if sg {
				err = c.deleteServingGroup(context.Background(), ms, "cleanup-0")
			} else {
				err = c.DeleteRole(context.Background(), ms, "cleanup-0", "infer", "infer-0")
			}
			require.ErrorContains(t, err, "injected")
			require.NotEqual(t, datastore.RoleNotFound, c.store.GetRoleStatus(nsn, "cleanup-0", "infer", "infer-0"))
			denied = false
			require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
			require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(nsn, "cleanup-0", "infer", "infer-0"))
			_, err = client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
		})
	}
}

func TestAcceptedServiceDeleteDoesNotWaitForFinalizer(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v1", "hash")
	client.PrependReactor("delete", "services", func(kt.Action) (bool, runtime.Object, error) { return true, nil, nil })
	require.NoError(t, c.DeleteRole(context.Background(), ms, "cleanup-0", "infer", "infer-0"))
	require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0"))
	_, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestOrphanCleanupAfterRestartRechecksCurrentIdentity(t *testing.T) {
	for _, mode := range []string{"orphan", "desired", "creating", "live-pod", "foreign-owner"} {
		t.Run(mode, func(t *testing.T) {
			c, client, ms, svc := auxiliaryFixture(t) // new controller, empty store/cleanup targets
			switch mode {
			case "desired":
				ms.Spec.Replicas = ptr.To[int32](1)
				ms.Spec.Template.Roles = []workload.Role{{Name: "infer", Replicas: ptr.To[int32](1)}}
			case "creating":
				c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v2", "hash")
			case "live-pod":
				_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-pod", Labels: svc.Labels}}, metav1.CreateOptions{})
				require.NoError(t, err)
			case "foreign-owner":
				svc.OwnerReferences[0].UID = "replacement-ms"
				_, err := client.CoreV1().Services(ms.Namespace).Update(context.Background(), svc, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			require.NoError(t, c.cleanupOrphanServices(context.Background(), ms))
			_, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
			if mode == "orphan" {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLegacyDNSDoesNotBlockRoleShrink(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	ms.Spec.Replicas = ptr.To[int32](1)
	role := workload.Role{Name: "infer", Replicas: ptr.To[int32](0), WorkerReplicas: 1, WorkerTemplate: &workload.PodTemplateSpec{}}
	ms.Spec.Template.Roles = []workload.Role{role}
	ms.Spec.Plugins = []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}}
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v1", "hash")
	_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "legacy-worker", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}, Spec: corev1.PodSpec{Subdomain: svc.Name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	chain, err := c.buildPluginChain(ms)
	require.NoError(t, err)
	client.PrependReactor("delete", "pods", func(kt.Action) (bool, runtime.Object, error) { return true, nil, nil })
	require.NoError(t, c.manageRoleReplicasPerGroup(context.Background(), ms, "cleanup-0", role, 0, "v1", true, chain))
	found := false
	for _, a := range client.Actions() {
		if a.Matches("delete", "pods") {
			found = true
		}
	}
	require.True(t, found, "explicit shrink must reach deletion before migration-dependent sync")
}

func TestGroupCompletionDiscardsPriorRoleDeletionTargets(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	key := auxiliaryCleanupKey{ms.UID, "cleanup-0", "infer", "infer-0"}
	c.podDeletions.Store(key, &podDeletionTargets{targets: map[string]types.UID{}, done: map[string]bool{}, namespace: ms.Namespace, ownerName: ms.Name, ownerUID: ms.UID})
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v1", "hash")
	require.NoError(t, c.deleteServingGroup(context.Background(), ms, "cleanup-0"))
	_, exists := c.podDeletions.Load(key)
	require.False(t, exists)
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v2", "hash2")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: ms.Namespace, UID: "new-uid", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}}
	_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.DeleteRole(context.Background(), ms, "cleanup-0", "infer", "infer-0"))
	_, err = client.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestCleanupCapturesServiceUIDBeforePodDeleteCallback(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v1", "hash")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: ms.Namespace, UID: "original-uid", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}}
	_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	client.PrependReactor("delete", "pods", func(kt.Action) (bool, runtime.Object, error) {
		replacement := svc.DeepCopy()
		replacement.UID = "replacement-service"
		// Mutate the tracker directly: fake actions execute while its client lock is held.
		require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("services"), replacement, ms.Namespace))
		return false, nil, nil
	})
	require.NoError(t, c.DeleteRole(context.Background(), ms, "cleanup-0", "infer", "infer-0"))
	actual, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, types.UID("replacement-service"), actual.UID)
}

func TestMigrationErrorDoesNotBlockAnotherRoleShrink(t *testing.T) {
	c, client, ms, _ := auxiliaryFixture(t)
	ms.Spec.Replicas = ptr.To[int32](1)
	blocked := workload.Role{Name: "blocked", Replicas: ptr.To[int32](1), WorkerReplicas: 1, WorkerTemplate: &workload.PodTemplateSpec{}}
	shrink := workload.Role{Name: "shrink", Replicas: ptr.To[int32](0), WorkerTemplate: &workload.PodTemplateSpec{}}
	ms.Spec.Template.Roles = []workload.Role{blocked, shrink}
	ms.Spec.Plugins = []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}}
	revision := utils.ModelServingRevision(ms)
	recordTestRevision(t, client, ms, revision)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, revision)
	for _, role := range []workload.Role{blocked, shrink} {
		c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", role.Name, role.Name+"-0", utils.ModelServingRevision(ms), "hash")
		for n := int32(0); n <= role.WorkerReplicas; n++ {
			name := utils.GeneratePodName("cleanup-0", role.Name+"-0", int(n))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ms.Namespace, UID: types.UID(name), Labels: map[string]string{workload.ModelServingNameLabelKey: ms.Name, workload.GroupNameLabelKey: "cleanup-0", workload.RoleLabelKey: role.Name, workload.RoleIDKey: role.Name + "-0", workload.EntryLabelKey: fmt.Sprint(n == 0), workload.RevisionLabelKey: revision}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}, Spec: corev1.PodSpec{Subdomain: utils.GeneratePodName("cleanup-0", role.Name+"-0", 0)}}
			_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		}
	}
	require.ErrorContains(t, c.syncRoleReplicas(c.withRevisionHistory(context.Background(), ms), ms, revision, nil), "MigrationRequired")
	_, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), "cleanup-0-shrink-0-0", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	_, err = client.CoreV1().Pods(ms.Namespace).Get(context.Background(), "cleanup-0-blocked-0-1", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestMigrationErrorDoesNotBlockRestartOrphanCleanup(t *testing.T) {
	c, client, ms, orphan := auxiliaryFixture(t)
	ms.Spec.Replicas = ptr.To[int32](1)
	ms.Spec.Template.Roles = []workload.Role{{Name: "blocked", Replicas: ptr.To[int32](1), WorkerReplicas: 1, WorkerTemplate: &workload.PodTemplateSpec{}}}
	ms.Spec.Plugins = []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}}
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "blocked", "blocked-0", utils.ModelServingRevision(ms), "hash")
	// Restart recovered the surviving Role from Pods; no store record remains for
	// this old auxiliary Service, which is outside the desired Role identities.
	for n := 0; n < 2; n++ {
		name := utils.GeneratePodName("cleanup-0", "blocked-0", n)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ms.Namespace, UID: types.UID(name), Labels: map[string]string{
			workload.ModelServingNameLabelKey: ms.Name, workload.GroupNameLabelKey: "cleanup-0", workload.RoleLabelKey: "blocked", workload.RoleIDKey: "blocked-0", workload.EntryLabelKey: fmt.Sprint(n == 0),
		}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}, Spec: corev1.PodSpec{Subdomain: "cleanup-0-blocked-0-0"}}
		_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	}
	require.ErrorContains(t, c.syncModelServing(context.Background(), "default/cleanup"), "MigrationRequired")
	_, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), orphan.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "migration errors must not starve independent orphan cleanup")
	for n := 0; n < 2; n++ {
		name := utils.GeneratePodName("cleanup-0", "blocked-0", n)
		pod, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, types.UID(name), pod.UID)
	}
}

func TestReplacementPodIsPreservedAndDiagnosedDuringCleanupRetry(t *testing.T) {
	for _, sg := range []bool{false, true} {
		t.Run(fmt.Sprint(sg), func(t *testing.T) {
			c, client, ms, svc := auxiliaryFixture(t)
			events := record.NewFakeRecorder(10)
			c.recorder = events
			c.store.AddRole(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0", "v1", "hash")
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "member", Namespace: ms.Namespace, UID: "original", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}}
			_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			denied := true
			client.PrependReactor("delete", "services", func(kt.Action) (bool, runtime.Object, error) {
				if denied {
					return true, nil, fmt.Errorf("injected")
				}
				return false, nil, nil
			})
			if sg {
				require.ErrorContains(t, c.deleteServingGroup(context.Background(), ms, "cleanup-0"), "injected")
			} else {
				require.ErrorContains(t, c.DeleteRole(context.Background(), ms, "cleanup-0", "infer", "infer-0"), "injected")
			}
			pod.UID = "replacement"
			_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			denied = false
			require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
			actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, types.UID("replacement"), actual.UID)
			service, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, svc.UID, service.UID)
			if sg {
				require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "cleanup-0"))
			} else {
				require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "cleanup-0", "infer", "infer-0"))
			}
			diagnosed := false
			for len(events.Events) > 0 {
				if strings.Contains(<-events.Events, "DeletionIdentityChanged") {
					diagnosed = true
				}
			}
			require.True(t, diagnosed, "safe waiting must be diagnosed instead of silently stuck or deleting the replacement")
		})
	}
}

// A worker sync can observe a deleted Pod live and recreate it while the
// informer callback is still handling that deletion. The recreated Pod must be
// part of the recovery instead of a replacement that leaves the unit Deleting.
func TestPodDeletionRecoveryIncludesPodRecreatedByInFlightSync(t *testing.T) {
	for _, policy := range []workload.RecoveryPolicy{workload.RoleRecreate, workload.ServingGroupRecreate} {
		t.Run(string(policy), func(t *testing.T) {
			c, client, ms, svc := auxiliaryFixture(t)
			ms.Spec.RecoveryPolicy = policy
			nsn := utils.GetNamespaceName(ms)
			c.store.AddRole(nsn, "cleanup-0", "infer", "infer-0", "v1", "hash")
			deleted := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "member", Namespace: ms.Namespace, UID: "deleted", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}}
			recreated := deleted.DeepCopy()
			recreated.UID = "recreated"
			syncCreated := false
			createBySync := func() {
				syncCreated = true
				require.NoError(t, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), recreated.DeepCopy(), ms.Namespace))
				require.NoError(t, c.podsInformer.GetIndexer().Add(recreated))
			}
			// Land the in-flight create right after the first deletion snapshot.
			client.PrependReactor("list", "pods", func(a kt.Action) (bool, runtime.Object, error) {
				if syncCreated {
					return false, nil, nil
				}
				handled, list, err := kt.ObjectReaction(client.Tracker())(a)
				createBySync()
				return handled, list, err
			})

			require.NoError(t, c.handleDeletedPod(ms, "cleanup-0", deleted))
			if !syncCreated {
				createBySync()
			}
			require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
			_, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), recreated.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err), "the Pod recreated by the in-flight sync must be recovered with its unit")

			require.NoError(t, c.podsInformer.GetIndexer().Delete(recreated))
			require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
			require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(nsn, "cleanup-0", "infer", "infer-0"))
		})
	}
}

func TestSparseOrphanCleanupRequiresStableSurvivingCapacity(t *testing.T) {
	for _, layer := range []string{"group", "role"} {
		for _, state := range []string{"stable", "deficient", "creating", "deleting", "pod-missing", "worker-missing", "terminating", "not-ready", "in-place", "foreign-pod", "target-creating", "target-pod"} {
			t.Run(layer+"/"+state, func(t *testing.T) {
				c, client, ms, orphan := auxiliaryFixture(t)
				ms.Spec.Replicas = ptr.To[int32](1)
				ms.Spec.Template.Roles = []workload.Role{{Name: "infer", Replicas: ptr.To[int32](1)}}
				group, id := "cleanup-1", "infer-0"
				if layer == "role" {
					group, id = "cleanup-0", "infer-1"
				}
				key := utils.GetNamespaceName(ms)
				c.store.AddRole(key, group, "infer", id, utils.ModelServingRevision(ms), "hash")
				require.NoError(t, c.store.UpdateRoleStatus(key, group, "infer", id, datastore.RoleRunning))
				require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: utils.GeneratePodName(group, id, 0), Namespace: ms.Namespace, UID: "survivor", Labels: map[string]string{workload.ModelServingNameLabelKey: ms.Name, workload.GroupNameLabelKey: group, workload.RoleLabelKey: "infer", workload.RoleIDKey: id}, OwnerReferences: orphan.OwnerReferences}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
				switch state {
				case "deficient":
					if layer == "group" {
						ms.Spec.Replicas = ptr.To[int32](2)
					} else {
						ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
					}
				case "creating":
					require.NoError(t, c.store.UpdateRoleStatus(key, group, "infer", id, datastore.RoleCreating))
					require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupCreating))
				case "deleting":
					require.NoError(t, c.store.UpdateRoleStatus(key, group, "infer", id, datastore.RoleDeleting))
					require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupDeleting))
				case "worker-missing":
					ms.Spec.Template.Roles[0].WorkerReplicas = 1
				case "terminating":
					pod.DeletionTimestamp = ptr.To(metav1.Now())
				case "not-ready":
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
				case "in-place":
					pod.Annotations = map[string]string{workload.InPlaceUpdateStateAnnotationKey: `{"revision":"target","patched":{"main":{"image":"next"}}}`}
					require.True(t, inplace.Updating(pod), "fixture must be logically unavailable despite stale Ready")
				case "foreign-pod":
					pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}
					pod.OwnerReferences[0].UID = "foreign"
				case "target-creating":
					c.store.AddRole(key, "cleanup-0", "infer", "infer-0", "new", "hash")
				case "target-pod":
					_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-target", Labels: orphan.Labels}}, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				if state != "pod-missing" {
					_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				client.PrependReactor("delete", "services", func(a kt.Action) (bool, runtime.Object, error) {
					require.Equal(t, orphan.UID, *a.(kt.DeleteAction).GetDeleteOptions().Preconditions.UID)
					return false, nil, nil
				})
				require.NoError(t, c.cleanupOrphanServices(context.Background(), ms))
				_, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), orphan.Name, metav1.GetOptions{})
				if state == "stable" {
					require.True(t, apierrors.IsNotFound(err))
				} else {
					require.NoError(t, err)
				}
				if state != "pod-missing" {
					actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, pod.UID, actual.UID)
				}
			})
		}
	}
}

func TestInPlaceFallbackPropagatesCleanupFailure(t *testing.T) {
	for _, path := range []string{"disabled", "decision-recreate", "resume-recreate"} {
		t.Run(path, func(t *testing.T) {
			c, client, ms, svc := auxiliaryFixture(t)
			ms.Spec.Replicas = ptr.To[int32](1)
			ms.Spec.Template.Roles = []workload.Role{{Name: "infer", Replicas: ptr.To[int32](1)}}
			ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: workload.RoleRollingUpdate, UpdateMethod: workload.InPlaceUpdateMethod}
			if path == "disabled" {
				ms.Spec.RolloutStrategy.UpdateMethod = workload.RecreateUpdateMethod
			}
			key := utils.GetNamespaceName(ms)
			c.store.AddRole(key, "cleanup-0", "infer", "infer-0", "old", "hash")
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: ms.Namespace, UID: "old-pod", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever}}
			_, err := client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
			denied := true
			client.PrependReactor("delete", "services", func(a kt.Action) (bool, runtime.Object, error) {
				require.Equal(t, svc.UID, *a.(kt.DeleteAction).GetDeleteOptions().Preconditions.UID)
				if denied {
					return true, nil, apierrors.NewForbidden(corev1.Resource("services"), svc.Name, fmt.Errorf("injected cleanup denial"))
				}
				return false, nil, nil
			})
			if path == "resume-recreate" {
				err = c.recreateInPlaceUnit(context.Background(), ms, &inPlaceUnit{groupName: "cleanup-0", roleName: "infer", roleID: "infer-0"})
			} else {
				err = c.updateRole(context.Background(), ms, "cleanup-0", "infer", "infer-0", utils.ModelServingRevision(ms))
			}
			require.ErrorContains(t, err, "injected cleanup denial")
			require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, "cleanup-0", "infer", "infer-0"))
			actual, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, svc.UID, actual.UID)
			denied = false
			require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
			_, err = client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err))
		})
	}
}
