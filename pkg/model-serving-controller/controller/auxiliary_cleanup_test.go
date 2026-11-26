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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	volcanolister "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"
)

func auxiliaryFixture(t *testing.T) (*ModelServingController, *kubefake.Clientset, *workload.ModelServing, *corev1.Service) {
	t.Helper()
	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "default", UID: types.UID("ms-uid")},
		Spec: workload.ModelServingSpec{
			Replicas: ptr.To[int32](0),
			Plugins:  []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
		},
	}
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

func TestGroupCompletionAllowsDeletingRecreatedRoleInstance(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	nsn := utils.GetNamespaceName(ms)
	c.store.AddRole(nsn, "cleanup-0", "infer", "infer-0", "v1", "hash")
	c.store.EnsureServingGroupInstanceID(nsn, "cleanup-0", "group-1")
	c.store.EnsureRoleInstanceID(nsn, "cleanup-0", "infer", "infer-0", "role-1")
	svc.Labels[workload.GroupInstanceIDLabelKey] = "group-1"
	svc.Labels[workload.RoleInstanceIDLabelKey] = "role-1"
	_, err := client.CoreV1().Services(ms.Namespace).Update(context.Background(), svc, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.deleteServingGroup(context.Background(), ms, "cleanup-0"))

	c.store.AddRole(nsn, "cleanup-0", "infer", "infer-0", "v2", "hash2")
	c.store.EnsureServingGroupInstanceID(nsn, "cleanup-0", "group-2")
	c.store.EnsureRoleInstanceID(nsn, "cleanup-0", "infer", "infer-0", "role-2")
	newLabels := map[string]string{
		workload.ModelServingNameLabelKey: ms.Name,
		workload.GroupNameLabelKey:        "cleanup-0",
		workload.GroupInstanceIDLabelKey:  "group-2",
		workload.RoleLabelKey:             "infer",
		workload.RoleIDKey:                "infer-0",
		workload.RoleInstanceIDLabelKey:   "role-2",
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: ms.Namespace, UID: "new-uid", Labels: newLabels, OwnerReferences: svc.OwnerReferences}}
	_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.DeleteRole(context.Background(), ms, "cleanup-0", "infer", "infer-0"))
	_, err = client.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestRoleDeletePreservesReplacementServiceInstance(t *testing.T) {
	c, client, ms, svc := auxiliaryFixture(t)
	nsn := utils.GetNamespaceName(ms)
	svc.Labels[workload.GroupInstanceIDLabelKey] = "group-1"
	svc.Labels[workload.RoleInstanceIDLabelKey] = "role-old"
	_, err := client.CoreV1().Services(ms.Namespace).Update(context.Background(), svc, metav1.UpdateOptions{})
	require.NoError(t, err)
	c.store.AddRole(nsn, "cleanup-0", "infer", "infer-0", "v1", "hash")
	c.store.EnsureServingGroupInstanceID(nsn, "cleanup-0", "group-1")
	c.store.EnsureRoleInstanceID(nsn, "cleanup-0", "infer", "infer-0", "role-old")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: ms.Namespace, UID: "original-uid", Labels: svc.Labels, OwnerReferences: svc.OwnerReferences}}
	_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	client.PrependReactor("delete", "pods", func(kt.Action) (bool, runtime.Object, error) {
		replacement := svc.DeepCopy()
		replacement.UID = "replacement-service"
		replacement.Labels[workload.RoleInstanceIDLabelKey] = "role-new"
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
			nsn := utils.GetNamespaceName(ms)
			svc.Labels[workload.GroupInstanceIDLabelKey] = "group-old"
			svc.Labels[workload.RoleInstanceIDLabelKey] = "role-old"
			_, err := client.CoreV1().Services(ms.Namespace).Update(context.Background(), svc, metav1.UpdateOptions{})
			require.NoError(t, err)
			c.store.AddRole(nsn, "cleanup-0", "infer", "infer-0", "v1", "hash")
			c.store.EnsureServingGroupInstanceID(nsn, "cleanup-0", "group-old")
			c.store.EnsureRoleInstanceID(nsn, "cleanup-0", "infer", "infer-0", "role-old")
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "member", Namespace: ms.Namespace, UID: "original", Labels: map[string]string{
				workload.ModelServingNameLabelKey: ms.Name,
				workload.GroupNameLabelKey:        "cleanup-0",
				workload.GroupInstanceIDLabelKey:  "group-old",
				workload.RoleLabelKey:             "infer",
				workload.RoleIDKey:                "infer-0",
				workload.RoleInstanceIDLabelKey:   "role-old",
			}, OwnerReferences: svc.OwnerReferences}}
			_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
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
			replacementPod := pod.DeepCopy()
			replacementPod.UID = "replacement"
			replacementPod.Labels[workload.GroupInstanceIDLabelKey] = "group-new"
			replacementPod.Labels[workload.RoleInstanceIDLabelKey] = "role-new"
			_, err = client.CoreV1().Pods(ms.Namespace).Create(context.Background(), replacementPod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(replacementPod))
			denied = false
			require.NoError(t, c.finishPendingDeletions(context.Background(), ms))
			actual, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), replacementPod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, types.UID("replacement"), actual.UID)
			service, err := client.CoreV1().Services(ms.Namespace).Get(context.Background(), svc.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, svc.UID, service.UID)
			if sg {
				require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(nsn, "cleanup-0"))
			} else {
				require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(nsn, "cleanup-0", "infer", "infer-0"))
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

func TestRestartMidDeleteFinishesDeletionInsteadOfRefillingEntry(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-ms", Namespace: "default", UID: "ms-uid"},
		Spec: workload.ModelServingSpec{
			Replicas:       ptr.To[int32](1),
			RecoveryPolicy: workload.RoleRecreate,
			Plugins:        []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
			Template: workload.ServingGroup{
				Roles: []workload.Role{{
					Name:           "infer",
					Replicas:       ptr.To[int32](1),
					WorkerReplicas: 1,
					EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
				}},
			},
		},
	}
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset(ms)
	leader1, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, leader1.modelServingsInformer.GetIndexer().Add(ms))

	require.NoError(t, leader1.syncModelServing(ctx, "default/restart-ms"))
	pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2)
	oldGroupID := pods.Items[0].Labels[workload.GroupInstanceIDLabelKey]
	oldRoleID := pods.Items[0].Labels[workload.RoleInstanceIDLabelKey]
	require.NotEmpty(t, oldGroupID)
	require.NotEmpty(t, oldRoleID)
	for i := range pods.Items {
		pod := pods.Items[i].DeepCopy()
		pod.UID = types.UID(pod.Name + "-gen1")
		_, err = kubeClient.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.NoError(t, leader1.podsInformer.GetIndexer().Add(pod))
	}

	// Simulate partial Role deletion where entry (-0) is deleted and worker (-1) fails before controller crash.
	failWorker := true
	kubeClient.PrependReactor("delete", "pods", func(a kt.Action) (bool, runtime.Object, error) {
		name := a.(kt.DeleteAction).GetName()
		if failWorker && strings.HasSuffix(name, "-1") {
			return true, nil, apierrors.NewServiceUnavailable("transient apiserver failure")
		}
		return false, nil, nil
	})
	require.ErrorContains(t, leader1.DeleteRole(ctx, ms, "restart-ms-0", "infer", "infer-0"), "transient apiserver failure")
	failWorker = false

	// Start a fresh controller with empty memory (no sync.Map, no Status persistence).
	leader2, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, leader2.modelServingsInformer.GetIndexer().Add(ms))
	remainingPods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, remainingPods.Items, 1, "only worker should remain after crashed mid-delete")
	for i := range remainingPods.Items {
		require.NoError(t, leader2.podsInformer.GetIndexer().Add(&remainingPods.Items[i]))
	}

	leader2.syncAll()
	require.Equal(t, datastore.RoleDeleting, leader2.store.GetRoleStatus(utils.GetNamespaceName(ms), "restart-ms-0", "infer", "infer-0"),
		"restart must recover RoleDeleting rather than refilling missing entry Pod into damaged instance")

	// First reconcile finishes deleting the surviving worker and old Headless Services.
	require.NoError(t, leader2.syncModelServing(ctx, "default/restart-ms"))
	afterDeletePods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, afterDeletePods.Items)
	require.NoError(t, leader2.podsInformer.GetIndexer().Delete(&remainingPods.Items[0]))

	// Next reconcile finalizes the Deleting store state, and the following reconcile recreates a fresh Role instance.
	require.NoError(t, leader2.syncModelServing(ctx, "default/restart-ms"))
	require.NoError(t, leader2.syncModelServing(ctx, "default/restart-ms"))
	recreatedPods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, recreatedPods.Items, 2)
	newRoleID := recreatedPods.Items[0].Labels[workload.RoleInstanceIDLabelKey]
	require.NotEmpty(t, newRoleID)
	require.NotEqual(t, oldRoleID, newRoleID, "recreated Role must carry a distinct instance ID")
}

func TestRestartWithTerminatingPodWaitsBeforeCreatingReplacement(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "term-ms", Namespace: "default", UID: "ms-uid"},
		Spec: workload.ModelServingSpec{
			Replicas:       ptr.To[int32](1),
			RecoveryPolicy: workload.RoleRecreate,
			Plugins:        []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
			Template: workload.ServingGroup{
				Roles: []workload.Role{{
					Name:           "infer",
					Replicas:       ptr.To[int32](1),
					WorkerReplicas: 0,
					EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
				}},
			},
		},
	}
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset(ms)
	leader1, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, leader1.modelServingsInformer.GetIndexer().Add(ms))
	require.NoError(t, leader1.syncModelServing(ctx, "default/term-ms"))

	podName := "term-ms-0-infer-0-0"
	oldPod, err := kubeClient.CoreV1().Pods(ms.Namespace).Get(ctx, podName, metav1.GetOptions{})
	require.NoError(t, err)
	oldPod = oldPod.DeepCopy()
	oldPod.UID = "old-pod-uid"
	now := metav1.Now()
	oldPod.DeletionTimestamp = &now
	_, err = kubeClient.CoreV1().Pods(ms.Namespace).Update(ctx, oldPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Fresh controller starts while oldPod is still Terminating.
	leader2, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, leader2.modelServingsInformer.GetIndexer().Add(ms))
	require.NoError(t, leader2.podsInformer.GetIndexer().Add(oldPod))

	leader2.syncAll()
	require.Equal(t, datastore.RoleDeleting, leader2.store.GetRoleStatus(utils.GetNamespaceName(ms), "term-ms-0", "infer", "infer-0"))

	kubeClient.ClearActions()
	require.NoError(t, leader2.syncModelServing(ctx, "default/term-ms"))
	for _, action := range kubeClient.Actions() {
		require.False(t, action.GetVerb() == "create" && action.GetResource().Resource == "pods",
			"controller must not create a replacement Pod while the previous instance is still terminating")
	}
}

func TestRestartMidMultiRoleGroupDeleteRecoversWholeGroupDeleting(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-ms", Namespace: "default", UID: "ms-uid"},
		Spec: workload.ModelServingSpec{
			Replicas:       ptr.To[int32](1),
			RecoveryPolicy: workload.RoleRecreate,
			Template: workload.ServingGroup{
				Roles: []workload.Role{
					{
						Name:          "a-role",
						Replicas:      ptr.To[int32](1),
						EntryTemplate: workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					},
					{
						Name:          "b-role",
						Replicas:      ptr.To[int32](1),
						EntryTemplate: workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					},
				},
			},
		},
	}
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset(ms)
	leader1, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, leader1.modelServingsInformer.GetIndexer().Add(ms))
	require.NoError(t, leader1.syncModelServing(ctx, "default/multi-ms"))

	pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 2)
	for i := range pods.Items {
		pod := pods.Items[i].DeepCopy()
		pod.UID = types.UID(pod.Name + "-uid")
		_, err = kubeClient.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.NoError(t, leader1.podsInformer.GetIndexer().Add(pod))
	}

	// Simulate whole-group delete removing all Pods of a-role, then failing before deleting b-role.
	kubeClient.PrependReactor("delete", "pods", func(a kt.Action) (bool, runtime.Object, error) {
		if strings.Contains(a.(kt.DeleteAction).GetName(), "b-role") {
			return true, nil, apierrors.NewServiceUnavailable("injected crash before b-role")
		}
		return false, nil, nil
	})
	require.ErrorContains(t, leader1.deleteServingGroup(ctx, ms, "multi-ms-0"), "injected crash before b-role")

	// Restart into a fresh controller; only b-role's Pod remains, carrying the whole-group tombstone.
	leader2, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, leader2.modelServingsInformer.GetIndexer().Add(ms))
	remaining, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, remaining.Items, 1)
	require.Equal(t, deletingScopeServingGroup, remaining.Items[0].Annotations[workload.DeletingAnnotationKey])
	require.NoError(t, leader2.podsInformer.GetIndexer().Add(&remaining.Items[0]))

	leader2.syncAll()
	require.Equal(t, datastore.ServingGroupDeleting, leader2.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "multi-ms-0"),
		"whole-group deletion tombstone must recover ServingGroupDeleting even under RoleRecreate when only complete Roles survive")
}

func TestRestartMidRoleDeleteUnderNonePolicyRecoversDeletingWhilePreservingSinglePodRepair(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "none-ms", Namespace: "default", UID: "ms-uid"},
		Spec: workload.ModelServingSpec{
			Replicas:       ptr.To[int32](1),
			RecoveryPolicy: workload.NoneRestartPolicy,
			Template: workload.ServingGroup{
				Roles: []workload.Role{{
					Name:           "infer",
					Replicas:       ptr.To[int32](1),
					WorkerReplicas: 1,
					EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
				}},
			},
		},
	}

	t.Run("explicit controller teardown recovers RoleDeleting", func(t *testing.T) {
		kubeClient := kubefake.NewSimpleClientset()
		kthenaClient := kthenafake.NewSimpleClientset(ms)
		leader1, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
		require.NoError(t, err)
		require.NoError(t, leader1.modelServingsInformer.GetIndexer().Add(ms))
		require.NoError(t, leader1.syncModelServing(ctx, "default/none-ms"))

		pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		for i := range pods.Items {
			pod := pods.Items[i].DeepCopy()
			pod.UID = types.UID(pod.Name + "-uid")
			_, err = kubeClient.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, leader1.podsInformer.GetIndexer().Add(pod))
		}
		kubeClient.PrependReactor("delete", "pods", func(a kt.Action) (bool, runtime.Object, error) {
			if strings.HasSuffix(a.(kt.DeleteAction).GetName(), "-1") {
				return true, nil, apierrors.NewServiceUnavailable("fail worker")
			}
			return false, nil, nil
		})
		require.ErrorContains(t, leader1.DeleteRole(ctx, ms, "none-ms-0", "infer", "infer-0"), "fail worker")

		leader2, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
		require.NoError(t, err)
		require.NoError(t, leader2.modelServingsInformer.GetIndexer().Add(ms))
		remaining, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, remaining.Items, 1)
		require.NoError(t, leader2.podsInformer.GetIndexer().Add(&remaining.Items[0]))

		leader2.syncAll()
		require.Equal(t, datastore.RoleDeleting, leader2.store.GetRoleStatus(utils.GetNamespaceName(ms), "none-ms-0", "infer", "infer-0"))
	})

	t.Run("accidental entry loss without tombstone stays repairable", func(t *testing.T) {
		worker := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "none-ms-0-infer-0-1",
				Namespace:       ms.Namespace,
				UID:             "worker-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)},
				Labels: map[string]string{
					workload.ModelServingNameLabelKey: ms.Name,
					workload.GroupNameLabelKey:        "none-ms-0",
					workload.GroupInstanceIDLabelKey:  "gid-1",
					workload.RoleLabelKey:             "infer",
					workload.RoleIDKey:                "infer-0",
					workload.RoleInstanceIDLabelKey:   "rid-1",
				},
			},
		}
		kubeClient := kubefake.NewSimpleClientset(worker)
		kthenaClient := kthenafake.NewSimpleClientset(ms)
		leader2, err := NewModelServingController(kubeClient, kthenaClient, volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
		require.NoError(t, err)
		require.NoError(t, leader2.modelServingsInformer.GetIndexer().Add(ms))
		require.NoError(t, leader2.podsInformer.GetIndexer().Add(worker))

		leader2.syncAll()
		require.NotEqual(t, datastore.RoleDeleting, leader2.store.GetRoleStatus(utils.GetNamespaceName(ms), "none-ms-0", "infer", "infer-0"))
	})
}

func TestSurvivingDiscoveryOrLegacyServiceKeepsGroupAndRoleIDsConsistent(t *testing.T) {
	ctx := context.Background()

	t.Run("surviving discovery-only Service is recovered without deadlock", func(t *testing.T) {
		ms := &workload.ModelServing{
			ObjectMeta: metav1.ObjectMeta{Name: "disc-ms", Namespace: "default", UID: "ms-uid"},
			Spec: workload.ModelServingSpec{
				Replicas: ptr.To[int32](1),
				Plugins:  []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
				Template: workload.ServingGroup{
					Roles: []workload.Role{{
						Name:           "infer",
						Replicas:       ptr.To[int32](1),
						WorkerReplicas: 1,
						EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
						WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					}},
				},
			},
		}
		entryName := utils.GeneratePodName("disc-ms-0", "infer-0", 0)
		discoverySvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            plugins.DiscoveryServiceName(entryName),
				Namespace:       ms.Namespace,
				UID:             "disc-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)},
				Labels: map[string]string{
					workload.ModelServingNameLabelKey:     ms.Name,
					workload.GroupNameLabelKey:            "disc-ms-0",
					workload.GroupInstanceIDLabelKey:      "gid-old",
					workload.RoleLabelKey:                 "infer",
					workload.RoleIDKey:                    "infer-0",
					workload.RoleInstanceIDLabelKey:       "rid-old",
					plugins.HeadlessServicePluginLabelKey: plugins.HeadlessServicePluginName,
				},
			},
			Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		}
		kubeClient := kubefake.NewSimpleClientset(discoverySvc)
		c, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(ms), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
		require.NoError(t, err)
		require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))

		require.NoError(t, c.syncModelServing(ctx, "default/disc-ms"))
		pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, pods.Items, 2)
		for _, pod := range pods.Items {
			require.Equal(t, "gid-old", pod.Labels[workload.GroupInstanceIDLabelKey])
			require.Equal(t, "rid-old", pod.Labels[workload.RoleInstanceIDLabelKey])
		}
		storeGID, ok := c.store.GetServingGroupInstanceID(utils.GetNamespaceName(ms), "disc-ms-0")
		require.True(t, ok)
		require.Equal(t, "gid-old", storeGID)
	})

	t.Run("legacy unlabeled Service keeps store and Pods consistent for later deletion", func(t *testing.T) {
		ms := &workload.ModelServing{
			ObjectMeta: metav1.ObjectMeta{Name: "legacy-ms", Namespace: "default", UID: "ms-uid"},
			Spec: workload.ModelServingSpec{
				Replicas: ptr.To[int32](1),
				Plugins:  []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
				Template: workload.ServingGroup{
					Roles: []workload.Role{{
						Name:           "infer",
						Replicas:       ptr.To[int32](1),
						WorkerReplicas: 0,
						EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
						WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
					}},
				},
			},
		}
		entryName := utils.GeneratePodName("legacy-ms-0", "infer-0", 0)
		legacySvc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            entryName,
				Namespace:       ms.Namespace,
				UID:             "legacy-svc-uid",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)},
				Labels: map[string]string{
					workload.ModelServingNameLabelKey: ms.Name,
					workload.GroupNameLabelKey:        "legacy-ms-0",
					workload.RoleLabelKey:             "infer",
					workload.RoleIDKey:                "infer-0",
				},
			},
			Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		}
		kubeClient := kubefake.NewSimpleClientset(legacySvc)
		c, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(ms), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
		require.NoError(t, err)
		require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))

		require.NoError(t, c.syncModelServing(ctx, "default/legacy-ms"))
		storeGID, ok := c.store.GetServingGroupInstanceID(utils.GetNamespaceName(ms), "legacy-ms-0")
		require.True(t, ok)
		require.Empty(t, storeGID, "store must preserve empty legacy groupInstanceID to match Pods")
		require.NoError(t, c.deleteServingGroup(ctx, ms, "legacy-ms-0"))
		pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, pods.Items, "fenced group deletion must delete Pods matching legacy empty groupInstanceID")
	})

	t.Run("multi-role retry after entry Pod failure keeps one consistent group ID", func(t *testing.T) {
		ms := &workload.ModelServing{
			ObjectMeta: metav1.ObjectMeta{Name: "pair-ms", Namespace: "default", UID: "ms-uid"},
			Spec: workload.ModelServingSpec{
				Replicas: ptr.To[int32](1),
				Plugins:  []workload.PluginSpec{{Name: plugins.HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}},
				Template: workload.ServingGroup{
					Roles: []workload.Role{
						{
							Name:           "prefill",
							Replicas:       ptr.To[int32](1),
							EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
							WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
						},
						{
							Name:           "decode",
							Replicas:       ptr.To[int32](1),
							EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
							WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "img:v1"}}}},
						},
					},
				},
			},
		}
		kubeClient := kubefake.NewSimpleClientset()
		failFirstPod := true
		kubeClient.PrependReactor("create", "pods", func(kt.Action) (bool, runtime.Object, error) {
			if failFirstPod {
				failFirstPod = false
				return true, nil, apierrors.NewServiceUnavailable("transient entry Pod create failure")
			}
			return false, nil, nil
		})
		c, err := NewModelServingController(kubeClient, kthenafake.NewSimpleClientset(ms), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
		require.NoError(t, err)
		require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))

		require.ErrorContains(t, c.syncModelServing(ctx, "default/pair-ms"), "transient entry Pod create failure")
		require.NoError(t, c.syncModelServing(ctx, "default/pair-ms"))
		pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, pods.Items, 2)
		gid0 := pods.Items[0].Labels[workload.GroupInstanceIDLabelKey]
		gid1 := pods.Items[1].Labels[workload.GroupInstanceIDLabelKey]
		require.NotEmpty(t, gid0)
		require.Equal(t, gid0, gid1, "all Roles in the ServingGroup must share one authoritative groupInstanceID")
	})
}

func TestRecreatedSinceDeletionShortCircuitsMatchingInstanceIDWithinSameSecond(t *testing.T) {
	c, _, ms, _ := auxiliaryFixture(t)
	ms.Spec.RecoveryPolicy = workload.RoleRecreate
	now := metav1.Now()
	surviving := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cleanup-0-infer-0-1",
			Namespace:         ms.Namespace,
			UID:               "worker-uid",
			CreationTimestamp: now,
			OwnerReferences:   []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)},
			Labels: map[string]string{
				workload.ModelServingNameLabelKey: ms.Name,
				workload.GroupNameLabelKey:        "cleanup-0",
				workload.GroupInstanceIDLabelKey:  "gid-1",
				workload.RoleLabelKey:             "infer",
				workload.RoleIDKey:                "infer-0",
				workload.RoleInstanceIDLabelKey:   "rid-1",
			},
		},
	}
	require.NoError(t, c.podsInformer.GetIndexer().Add(surviving))

	deleted := surviving.DeepCopy()
	deleted.Name = "cleanup-0-infer-0-0"
	deleted.UID = "entry-uid"
	deleted.DeletionTimestamp = &now
	require.False(t, c.recreatedSinceDeletion(ms, "cleanup-0", deleted),
		"a surviving Pod with the same non-empty roleInstanceID proves the Role was not recreated even within the same second")
}

func TestEmptyGroupRevisionSwitchDrainsOldPodGroupBeforeSwitchingID(t *testing.T) {
	ctx := context.Background()
	oldMS := appliedHistoryWorkload(workload.ServingGroupRollingUpdate, "image:v1", 0)
	newMS := appliedHistoryWorkload(workload.ServingGroupRollingUpdate, "image:v2", 1)
	oldMS.Spec.SchedulerName, newMS.Spec.SchedulerName = "volcano", "volcano"
	groupName := utils.GenerateServingGroupName(newMS.Name, 0)

	oldPG := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            groupName,
			Namespace:       newMS.Namespace,
			UID:             "old-pg-uid",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(newMS, workload.ModelServingKind)},
			Labels: map[string]string{
				workload.ModelServingNameLabelKey: newMS.Name,
				workload.GroupNameLabelKey:        groupName,
				workload.GroupInstanceIDLabelKey:  "gid-old",
			},
		},
	}

	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset(newMS)
	volcanoClient := volcanofake.NewSimpleClientset(oldPG)
	apiextClient := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

	c, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	require.NoError(t, err)
	pgMgr := podgroupmanager.NewManager(kubeClient, volcanoClient, apiextClient, nil)
	pgInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &schedulingv1beta1.PodGroup{}, 0, cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
	})
	require.NoError(t, pgInformer.GetIndexer().Add(oldPG))
	pgMgr.PodGroupInformer = pgInformer
	pgMgr.PodGroupLister = volcanolister.NewPodGroupLister(pgInformer.GetIndexer())
	c.podGroupManager = pgMgr

	recordTestRevision(t, kubeClient, oldMS, "old")
	recordTestRevision(t, kubeClient, newMS, "new")
	nsn := utils.GetNamespaceName(newMS)
	c.store.AddServingGroup(nsn, 0, "old")
	c.store.EnsureServingGroupInstanceID(nsn, groupName, "gid-old")

	// Empty group switching revisions while old PodGroup still exists must drain the old PodGroup
	// using its original instance ID and UID instead of creating Pods against it.
	require.NoError(t, c.syncRoleReplicas(c.withRevisionHistory(ctx, newMS), newMS, "new", nil))
	pods, err := kubeClient.CoreV1().Pods(newMS.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, pods.Items, "must not create Pods while old PodGroup is still draining")
	require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(nsn, groupName))
	_, err = volcanoClient.SchedulingV1beta1().PodGroups(newMS.Namespace).Get(ctx, groupName, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "old PodGroup must be deleted with its original instance ID and UID")
}
