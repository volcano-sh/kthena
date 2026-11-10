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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func deletionTestPod(ms *workload.ModelServing, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ms.Namespace, UID: types.UID(name + "-old"), Labels: map[string]string{"role": "same"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workload.ModelServingKind)}}}
}

func assertDeletionScopePatch(t *testing.T, action k8stesting.Action, scope string, uid types.UID) {
	t.Helper()
	patch, ok := action.(k8stesting.PatchAction)
	require.True(t, ok)
	var payload struct {
		Metadata struct {
			UID         types.UID         `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(patch.GetPatch(), &payload))
	require.Equal(t, map[string]string{utils.DeletionScopeAnnotation: scope}, payload.Metadata.Annotations)
	if uid != "" {
		require.Equal(t, uid, payload.Metadata.UID)
	}
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(patch.GetPatch(), &fields))
	require.Len(t, fields, 1, "deletion preparation may only patch metadata")
}

func TestPodDeletionTargetsRetryNeverAbsorbsReplacements(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-old"}}
	first, second := deletionTestPod(ms, "a"), deletionTestPod(ms, "b")
	client := kubefake.NewSimpleClientset(first, second)
	deletes := map[string]int{}
	client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		for _, name := range []string{"a", "b"} {
			obj, err := client.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ms.Namespace, name)
			if !apierrors.IsNotFound(err) {
				require.NoError(t, err)
				pod := obj.(*corev1.Pod)
				require.Equal(t, utils.DeletionScopeRole, pod.Annotations[utils.DeletionScopeAnnotation], "all original UIDs must be marked before the first DELETE")
			}
		}
		deletion := action.(k8stesting.DeleteAction)
		deletes[deletion.GetName()]++
		options := deletion.GetDeleteOptions()
		require.NotNil(t, options.Preconditions)
		require.NotNil(t, options.Preconditions.UID)
		assert.Equal(t, types.UID(deletion.GetName()+"-old"), *options.Preconditions.UID)
		if deletion.GetName() == "b" && deletes["b"] == 1 {
			return true, nil, apierrors.NewServiceUnavailable("retry")
		}
		return false, nil, nil
	})
	targets := &podDeletionTargets{}
	selector := labels.SelectorFromSet(map[string]string{"role": "same"})
	require.ErrorContains(t, targets.Delete(ctx, client, ms, selector, utils.DeletionScopeRole), "retry")
	replacementA, replacementB, newPod := deletionTestPod(ms, "a"), deletionTestPod(ms, "b"), deletionTestPod(ms, "c")
	replacementA.UID, replacementB.UID = "a-new", "b-new"
	_, err := client.CoreV1().Pods(ms.Namespace).Create(ctx, replacementA, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().Pods(ms.Namespace).Update(ctx, replacementB, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().Pods(ms.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, targets.Delete(ctx, client, ms, selector, utils.DeletionScopeRole))
	require.NoError(t, targets.Delete(ctx, client, ms, selector, utils.DeletionScopeRole))
	assert.Equal(t, map[string]int{"a": 1, "b": 1}, deletes)
	for _, pod := range []*corev1.Pod{replacementA, replacementB, newPod} {
		current, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, pod.UID, current.UID)
	}
	listCount := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" {
			listCount++
		}
		assert.NotEqual(t, "delete-collection", action.GetVerb())
	}
	assert.Equal(t, 1, listCount)
	newer := ms.DeepCopy()
	newer.UID = "ms-new"
	require.ErrorContains(t, targets.Delete(ctx, client, newer, selector, utils.DeletionScopeRole), "different ModelServing identity")
}

func TestPodDeletionMarkerFailurePreventsDeleteAndRetainsSnapshot(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "owner"}}
	first, second := deletionTestPod(ms, "a"), deletionTestPod(ms, "b")
	client := kubefake.NewSimpleClientset(first, second)
	denied := true
	client.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		assertDeletionScopePatch(t, action, utils.DeletionScopeServingGroup, types.UID(action.(k8stesting.PatchAction).GetName()+"-old"))
		if denied && action.(k8stesting.PatchAction).GetName() == "b" {
			return true, nil, fmt.Errorf("marker denied")
		}
		return false, nil, nil
	})
	targets := &podDeletionTargets{}
	require.ErrorContains(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeServingGroup), "marker denied")
	for _, action := range client.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
	newPod := deletionTestPod(ms, "later")
	_, err := client.CoreV1().Pods(ms.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
	require.NoError(t, err)
	denied = false
	require.NoError(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeServingGroup))
	got, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, newPod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, newPod.UID, got.UID)
	require.Empty(t, got.Annotations)
}

func TestPodDeletionTargetsFenceReplacementBetweenGetAndDelete(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-old"}}
	pod := deletionTestPod(ms, "a")
	client := kubefake.NewSimpleClientset(pod)
	deletes := 0
	client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deletes++
		deletion := action.(k8stesting.DeleteAction)
		assert.Equal(t, pod.UID, *deletion.GetDeleteOptions().Preconditions.UID)
		replacement := pod.DeepCopy()
		replacement.UID = "new"
		require.NoError(t, client.Tracker().Update(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, replacement, ms.Namespace))
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("UID precondition failed"))
	})
	targets := &podDeletionTargets{}
	require.ErrorContains(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole), "UID precondition")
	require.NoError(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole))
	assert.Equal(t, 1, deletes)
	current, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, types.UID("new"), current.UID)
}

func TestPodDeletionMarkerFencesReplacementAndNeverDowngradesScope(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "owner"}}
	pod := deletionTestPod(ms, "a")
	client := kubefake.NewSimpleClientset(pod)
	client.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		assertDeletionScopePatch(t, action, utils.DeletionScopeRole, pod.UID)
		replacement := pod.DeepCopy()
		replacement.UID = "replacement"
		require.NoError(t, client.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, replacement, ms.Namespace))
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("identity changed before marker"))
	})
	targets := &podDeletionTargets{}
	require.NoError(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole))
	for _, action := range client.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
	replacement, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, types.UID("replacement"), replacement.UID)
	require.Empty(t, replacement.Annotations)
	// A late Role deletion cannot downgrade a previously committed SG marker.
	pod.Annotations = map[string]string{utils.DeletionScopeAnnotation: utils.DeletionScopeServingGroup}
	client = kubefake.NewSimpleClientset(pod)
	client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	require.NoError(t, (&podDeletionTargets{}).Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole))
	kept, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, utils.DeletionScopeServingGroup, kept.Annotations[utils.DeletionScopeAnnotation])
	for _, action := range client.Actions() {
		require.False(t, action.Matches("patch", "pods"))
	}
}

func TestPodDeletionTargetsOnlyCurrentControllerOwnedPods(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-old"}}
	owned, foreignUID, foreignName, nonController, foreignKind := deletionTestPod(ms, "owned"), deletionTestPod(ms, "foreignUID"), deletionTestPod(ms, "foreignName"), deletionTestPod(ms, "nonController"), deletionTestPod(ms, "foreignKind")
	foreignUID.OwnerReferences[0].UID = "other"
	foreignName.OwnerReferences[0].Name = "other"
	nonController.OwnerReferences[0].Controller = ptr.To(false)
	foreignKind.OwnerReferences[0].Kind = "StatefulSet"
	client := kubefake.NewSimpleClientset(owned, foreignUID, foreignName, nonController, foreignKind)
	targets := &podDeletionTargets{}
	require.NoError(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole))
	pods, err := client.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, pods.Items, 4)
	for _, pod := range pods.Items {
		assert.NotEqual(t, "owned", pod.Name)
	}
}

func TestPodDeletionTargetsAcceptedDeleteDoesNotWaitForFinalizer(t *testing.T) {
	ctx := context.Background()
	ms := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-old"}}
	pod := deletionTestPod(ms, "a")
	pod.Finalizers = []string{"example.com/hold"}
	client := kubefake.NewSimpleClientset(pod)
	deletes := 0
	client.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) { deletes++; return true, nil, nil })
	targets := &podDeletionTargets{}
	require.NoError(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole))
	require.NoError(t, targets.Delete(ctx, client, ms, labels.Everything(), utils.DeletionScopeRole))
	assert.Equal(t, 1, deletes)
	_, err := client.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err, "accepted deletion is separate from observed capacity disappearance")
}
