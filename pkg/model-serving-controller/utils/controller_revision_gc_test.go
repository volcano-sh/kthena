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

package utils

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func TestRevisionGCRetainsSourcesForLiveAndInPlaceReferences(t *testing.T) {
	ctx := context.Background()
	ms := revisionTestModelServing(revisionTestRole("decode", "image"))
	ms.ObjectMeta = metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "owner"}
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "latest", "latest"
	client := kubefake.NewSimpleClientset()
	names := map[string]string{}
	for _, revision := range []string{"old", "patch-target", "unused"} {
		cr, err := CreateControllerRevision(ctx, client, ms, revision, ms.Spec.Template.Roles)
		require.NoError(t, err)
		names[revision] = cr.Name
		if revision == "old" {
			alias := cr.DeepCopy()
			alias.Name += "-baseline"
			_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(ctx, alias, metav1.CreateOptions{})
			require.NoError(t, err)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "member", Namespace: ms.Namespace,
		Labels:          map[string]string{workload.RevisionLabelKey: "old"},
		Annotations:     map[string]string{workload.InPlaceUpdateStateAnnotationKey: `{"version":1,"phase":"Updating","targetRevision":"patch-target"}`},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)},
	}}
	_, err := client.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
	for revision, name := range names {
		_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
		if revision == "unused" {
			require.True(t, apierrors.IsNotFound(err), name)
		} else {
			require.NoError(t, err, name)
		}
	}
	_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, names["old"]+"-baseline", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "a matching revision label must not preserve an unreferenced alias")
	require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
	for _, name := range names {
		_, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(err), name)
	}
}

func TestRevisionGCRetainsLiveOwnedPodReferences(t *testing.T) {
	for _, state := range []string{"running", "terminating", "in-place", "missing-name-label", "missing-revision-label", "wrong-uid", "wrong-kind", "wrong-api", "wrong-name", "not-controller"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-uid"}, Status: workload.ModelServingStatus{CurrentRevision: "target", UpdateRevision: "target"}}
			client := kubefake.NewSimpleClientset()
			cr, err := CreateControllerRevision(ctx, client, ms, "old", []workload.Role{{Name: "decode"}})
			require.NoError(t, err)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "member", Namespace: ms.Namespace, UID: "pod", Labels: map[string]string{workload.ModelServingNameLabelKey: ms.Name, workload.RevisionLabelKey: "old"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}}
			preserve := true
			switch state {
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "in-place":
				pod.Annotations = map[string]string{workload.InPlaceUpdateStateAnnotationKey: `{"revision":"target","patched":{"main":{"image":"next"}}}`}
			case "missing-name-label":
				delete(pod.Labels, workload.ModelServingNameLabelKey)
			case "missing-revision-label":
				delete(cr.Labels, ControllerRevisionRevisionLabelKey)
				_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "wrong-uid":
				pod.OwnerReferences[0].UID = "other"
				preserve = false
			case "wrong-kind":
				pod.OwnerReferences[0].Kind = "Other"
				preserve = false
			case "wrong-api":
				pod.OwnerReferences[0].APIVersion = "other/v1"
				preserve = false
			case "wrong-name":
				pod.OwnerReferences[0].Name = "other"
				preserve = false
			case "not-controller":
				pod.OwnerReferences[0].Controller = nil
				preserve = false
			}
			_, err = client.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
			_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
			if preserve {
				require.NoError(t, err, "an actual owned Pod still needs this history")
				require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
				require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
				_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
			}
			require.True(t, apierrors.IsNotFound(err), "unused history must remain collectible")
		})
	}
}

func TestRevisionGCRetainsUnfinishedTargetAfterRetarget(t *testing.T) {
	for _, raw := range []string{
		`{"revision":"b","patched":{"main":{"image":"next"}}}`,
		`{"version":1,"phase":"Updating","targetRevision":"b"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			ctx := context.Background()
			ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-uid"}, Status: workload.ModelServingStatus{CurrentRevision: "a", UpdateRevision: "c"}}
			client := kubefake.NewSimpleClientset()
			target, err := CreateControllerRevision(ctx, client, ms, "b", []workload.Role{{Name: "infer"}})
			require.NoError(t, err)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "member", Namespace: ms.Namespace, Labels: map[string]string{workload.RevisionLabelKey: "a"}, Annotations: map[string]string{workload.InPlaceUpdateStateAnnotationKey: raw}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}}
			_, err = client.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
			_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
			require.NoError(t, err, "unfinished B must survive even when status targets C and the Pod label is A")
			pod.Labels[workload.RevisionLabelKey] = "c"
			pod.Annotations[workload.InPlaceUpdateStateAnnotationKey] = `{"revision":"b","done":true}`
			_, err = client.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
			_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err), "a completed annotation must not retain an otherwise unused target")
		})
	}
}

func TestRevisionGCObservationFailureAndForeignRevisionSafety(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-uid"}}
	for _, mode := range []string{"pod-list-failure", "wrong-uid", "wrong-name", "wrong-kind", "wrong-api", "not-controller", "replacement-uid"} {
		t.Run(mode, func(t *testing.T) {
			client := kubefake.NewSimpleClientset()
			cr, err := CreateControllerRevision(ctx, client, ms, "unused", []workload.Role{{Name: "decode"}})
			require.NoError(t, err)
			cr.UID = "original"
			switch mode {
			case "wrong-uid":
				cr.OwnerReferences[0].UID = "foreign"
			case "wrong-name":
				cr.OwnerReferences[0].Name = "foreign"
			case "wrong-kind":
				cr.OwnerReferences[0].Kind = "Other"
			case "wrong-api":
				cr.OwnerReferences[0].APIVersion = "other/v1"
			case "not-controller":
				cr.OwnerReferences[0].Controller = nil
			}
			_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
			require.NoError(t, err)
			if mode == "pod-list-failure" {
				client.PrependReactor("list", "pods", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("injected Pod observation failure")
				})
			}
			deleteCalls := 0
			client.PrependReactor("delete", "controllerrevisions", func(a kt.Action) (bool, runtime.Object, error) {
				deleteCalls++
				require.Equal(t, types.UID("original"), *a.(kt.DeleteAction).GetDeleteOptions().Preconditions.UID)
				if mode == "replacement-uid" {
					replacement := cr.DeepCopy()
					replacement.UID = "replacement"
					require.NoError(t, client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("controllerrevisions"), replacement, ms.Namespace))
					return true, nil, apierrors.NewConflict(appsv1.Resource("controllerrevisions"), cr.Name, fmt.Errorf("UID changed"))
				}
				return false, nil, nil
			})
			err = CleanupOldControllerRevisions(ctx, client, ms)
			if mode == "pod-list-failure" {
				require.ErrorContains(t, err, "injected Pod observation failure")
			} else {
				require.NoError(t, err)
			}
			actual, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, cr.Name, metav1.GetOptions{})
			require.NoError(t, err)
			if mode == "replacement-uid" {
				require.Equal(t, 1, deleteCalls, "a conflicted deletion must not retarget the replacement")
				require.Equal(t, types.UID("replacement"), actual.UID)
			} else {
				require.Zero(t, deleteCalls)
				require.Equal(t, cr, actual)
			}
		})
	}
}
