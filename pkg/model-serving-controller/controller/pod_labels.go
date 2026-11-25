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
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const podLabelReconcileTimeout = 5 * time.Second

type podIdentity struct {
	groupName string
	roleName  string
	roleID    string
	podIndex  int
	hasIndex  bool
}

// reconcilePodLabels restores drifted controller/template labels on an owned Pod
// without disturbing unrelated user or sidecar labels. When an API patch is
// issued, it returns the patched Pod so updatePod can continue with clean labels.
func (c *ModelServingController) reconcilePodLabels(oldPod, newPod *corev1.Pod) (patchedPod *corev1.Pod, ms *workloadv1alpha1.ModelServing, err error) {
	if newPod == nil || newPod.DeletionTimestamp != nil || c.kubeClientSet == nil || c.modelServingLister == nil {
		return nil, nil, nil
	}
	ms = c.modelServingFromOwnerRef(newPod)
	if ms == nil {
		return nil, nil, nil
	}

	id, ok := resolvePodIdentity(ms, oldPod, newPod)
	if !ok {
		return nil, ms, nil
	}

	// Fast path for frequent kubelet status/condition updates: when labels did
	// not change from oldPod and structural identity labels are already intact,
	// avoid ControllerRevision API lookups.
	if oldPod != nil && maps.Equal(oldPod.Labels, newPod.Labels) && hasValidStructuralLabels(ms, id, newPod.Labels) {
		currentRev := utils.ModelServingRevision(ms)
		podRev := utils.ObjectRevision(newPod)
		if podRev != "" && podRev != currentRev {
			return nil, ms, nil
		}
		if expected, ok := expectedLabelsFromModelServing(ms, id, newPod, podRev); ok {
			if len(diffPodLabels(newPod.Labels, expected, id, false, false)) == 0 {
				return nil, ms, nil
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), podLabelReconcileTimeout)
	defer cancel()

	// Clean up stale store entries if a previous mutation briefly moved this Pod
	// into another group's runningPods map.
	if oldPod != nil && c.store != nil {
		if oldGroup := oldPod.Labels[workloadv1alpha1.GroupNameLabelKey]; oldGroup != "" && oldGroup != id.groupName {
			c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), oldGroup, newPod.Name)
		}
	}

	patchedPod, err = c.patchDriftedPodLabels(ctx, ms, id, oldPod, newPod, false)
	return patchedPod, ms, err
}

// handleSelectorExitOrDeletedPod handles deletePod when GroupNameLabelKey or
// ModelServingNameLabelKey is missing on the Pod delivered to DeleteFunc.
// If the Pod is still live (selector-exit from removing GroupNameLabelKey), it
// repairs labels with Conflict retry so the Pod re-enters the informer.
// Otherwise it recovers the ModelServing + group/role identity so store cleanup
// and recovery can still proceed.
func (c *ModelServingController) handleSelectorExitOrDeletedPod(pod *corev1.Pod) (ms *workloadv1alpha1.ModelServing, servingGroupName, roleName, roleID string, handled bool) {
	ms = c.modelServingFromOwnerRef(pod)
	if ms == nil {
		return nil, "", "", "", true
	}

	id, ok := resolvePodIdentity(ms, nil, pod)
	if !ok {
		return nil, "", "", "", true
	}

	if c.kubeClientSet != nil {
		ctx, cancel := context.WithTimeout(context.Background(), podLabelReconcileTimeout)
		defer cancel()

		var liveExists bool
		var replaced bool
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			livePod, getErr := c.kubeClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				liveExists = false
				return nil
			}
			if getErr != nil {
				return getErr
			}
			if livePod.UID != pod.UID {
				liveExists = false
				replaced = true
				return nil
			}
			if livePod.DeletionTimestamp != nil {
				liveExists = false
				return nil
			}
			liveExists = true
			_, patchErr := c.patchDriftedPodLabels(ctx, ms, id, pod, livePod, true)
			return patchErr
		})
		if err != nil {
			klog.Errorf("failed to reconcile selector-exit pod %s/%s labels: %v", pod.Namespace, pod.Name, err)
			c.enqueueModelServing(ms)
			return nil, "", "", "", true
		}
		if liveExists {
			return nil, "", "", "", true
		}
		if replaced {
			// A newer Pod with the same name already exists; do not run delete
			// recovery against the replacement, only clean up the stale running entry.
			if c.store != nil {
				c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), id.groupName, pod.Name)
			}
			c.enqueueModelServing(ms)
			return nil, "", "", "", true
		}
	}

	return ms, id.groupName, id.roleName, id.roleID, false
}

func (c *ModelServingController) modelServingFromOwnerRef(obj metav1.Object) *workloadv1alpha1.ModelServing {
	if obj == nil || c.modelServingLister == nil {
		return nil
	}
	var owner *metav1.OwnerReference
	if ctrl := metav1.GetControllerOf(obj); ctrl != nil &&
		ctrl.APIVersion == workloadv1alpha1.SchemeGroupVersion.String() &&
		ctrl.Kind == workloadv1alpha1.ModelServingKind.Kind {
		owner = ctrl
	} else {
		refs := obj.GetOwnerReferences()
		for i := range refs {
			if refs[i].APIVersion == workloadv1alpha1.SchemeGroupVersion.String() &&
				refs[i].Kind == workloadv1alpha1.ModelServingKind.Kind {
				owner = &refs[i]
				break
			}
		}
	}
	if owner == nil || owner.Name == "" {
		return nil
	}
	ms, err := c.modelServingLister.ModelServings(obj.GetNamespace()).Get(owner.Name)
	if err != nil || ms.UID != owner.UID {
		return nil
	}
	return ms
}

func resolvePodIdentity(ms *workloadv1alpha1.ModelServing, oldPod, newPod *corev1.Pod) (podIdentity, bool) {
	if groupName, roleName, roleID, podIndex, ok := utils.ParseModelServingPodName(ms.Name, newPod.Name); ok {
		return podIdentity{
			groupName: groupName,
			roleName:  roleName,
			roleID:    roleID,
			podIndex:  podIndex,
			hasIndex:  true,
		}, true
	}
	for _, candidate := range []*corev1.Pod{oldPod, newPod} {
		if candidate == nil {
			continue
		}
		if id, ok := identityFromLabels(ms, candidate.Labels); ok {
			return id, true
		}
	}
	return podIdentity{}, false
}

func identityFromLabels(ms *workloadv1alpha1.ModelServing, podLabels map[string]string) (podIdentity, bool) {
	if len(podLabels) == 0 {
		return podIdentity{}, false
	}
	msName, groupName, ok := utils.GetModelServingAndGroupByLabel(podLabels)
	if !ok || msName != ms.Name {
		return podIdentity{}, false
	}
	if !hasGeneratedIdentity(ms, groupName, podLabels) {
		return podIdentity{}, false
	}
	roleName := podLabels[workloadv1alpha1.RoleLabelKey]
	roleID := podLabels[workloadv1alpha1.RoleIDKey]
	if roleName == "" || roleID == "" {
		return podIdentity{}, false
	}
	podIndex := 1
	hasIndex := false
	if podLabels[workloadv1alpha1.EntryLabelKey] == utils.Entry {
		podIndex = 0
		hasIndex = true
	}
	return podIdentity{
		groupName: groupName,
		roleName:  roleName,
		roleID:    roleID,
		podIndex:  podIndex,
		hasIndex:  hasIndex,
	}, true
}

func hasValidStructuralLabels(ms *workloadv1alpha1.ModelServing, id podIdentity, podLabels map[string]string) bool {
	if len(podLabels) == 0 {
		return false
	}
	if podLabels[workloadv1alpha1.ModelServingNameLabelKey] != ms.Name ||
		podLabels[workloadv1alpha1.GroupNameLabelKey] != id.groupName ||
		podLabels[workloadv1alpha1.RoleLabelKey] != id.roleName ||
		podLabels[workloadv1alpha1.RoleIDKey] != id.roleID {
		return false
	}
	if id.hasIndex {
		if id.podIndex == 0 && podLabels[workloadv1alpha1.EntryLabelKey] != utils.Entry {
			return false
		}
		if id.podIndex > 0 && podLabels[workloadv1alpha1.EntryLabelKey] == utils.Entry {
			return false
		}
	}
	return true
}

func (c *ModelServingController) patchDriftedPodLabels(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	id podIdentity,
	oldPod, targetPod *corev1.Pod,
	forceIncludeGroup bool,
) (*corev1.Pod, error) {
	expected, unresolved := c.expectedLabelsForPod(ctx, ms, id, oldPod, targetPod)
	patchLabels := diffPodLabels(targetPod.Labels, expected, id, forceIncludeGroup, unresolved)
	if len(patchLabels) == 0 {
		return nil, nil
	}

	meta := map[string]any{
		"labels": patchLabels,
	}
	if targetPod.ResourceVersion != "" {
		meta["resourceVersion"] = targetPod.ResourceVersion
	}
	if targetPod.UID != "" {
		meta["uid"] = targetPod.UID
	}
	payload, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return nil, err
	}

	patchedPod, err := c.kubeClientSet.CoreV1().Pods(targetPod.Namespace).Patch(
		ctx, targetPod.Name, types.StrategicMergePatchType, payload, metav1.PatchOptions{},
	)
	if err != nil {
		return nil, err
	}
	klog.V(2).Infof("Reconciled drifted labels on Pod %s/%s for ModelServing %s/%s",
		targetPod.Namespace, targetPod.Name, ms.Namespace, ms.Name)
	return patchedPod, nil
}

func (c *ModelServingController) expectedLabelsForPod(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	id podIdentity,
	oldPod, targetPod *corev1.Pod,
) (expected map[string]string, unresolved bool) {
	revision := c.resolvePodRevision(ms, id, oldPod, targetPod)

	var applicable *workloadv1alpha1.ModelServing
	var role workloadv1alpha1.Role
	var roleTemplateHash string
	var resolved bool

	if revision != "" {
		histMS, err := c.modelServingForRevision(ctx, ms, revision)
		if err == nil {
			if histRole, roleErr := historicalRole(histMS, id.roleName, revision); roleErr == nil {
				applicable = histMS
				role = histRole
				roleTemplateHash, _ = utils.RoleRevisionHash(histMS, id.roleName)
				resolved = true
			}
		}
	}

	if !resolved && (revision == "" || revision == utils.ModelServingRevision(ms)) {
		if expectedCur, ok := expectedLabelsFromModelServing(ms, id, targetPod, revision); ok {
			return expectedCur, false
		}
	}

	if resolved && id.hasIndex {
		expected = utils.ExpectedPodLabels(role, applicable, id.groupName, id.roleID, id.podIndex, revision, roleTemplateHash)
		if revision == "" {
			delete(expected, workloadv1alpha1.RevisionLabelKey)
		}
		if roleTemplateHash == "" {
			delete(expected, workloadv1alpha1.RoleTemplateHashLabelKey)
		}
		if hasPlugin(applicable, plugins.LWSLabelsPluginName) {
			if lwsLabels, err := plugins.ExpectedLWSLabels(applicable, id.groupName, targetPod.Name, targetPod.Namespace, id.podIndex == 0); err == nil {
				maps.Copy(expected, lwsLabels)
			}
		}
		return expected, false
	}

	// Historical ControllerRevision could not be resolved; restore only structural
	// identity labels and avoid overwriting historical template labels with current spec.
	expected = map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.GroupNameLabelKey:        id.groupName,
		workloadv1alpha1.RoleLabelKey:             id.roleName,
		workloadv1alpha1.RoleIDKey:                id.roleID,
	}
	if id.hasIndex && id.podIndex == 0 {
		expected[workloadv1alpha1.EntryLabelKey] = utils.Entry
	}
	if revision != "" {
		expected[workloadv1alpha1.RevisionLabelKey] = revision
	}
	if oldPod != nil {
		if h := utils.ObjectRoleTemplateHash(oldPod); h != "" {
			expected[workloadv1alpha1.RoleTemplateHashLabelKey] = h
		}
	}
	return expected, true
}

func expectedLabelsFromModelServing(ms *workloadv1alpha1.ModelServing, id podIdentity, targetPod *corev1.Pod, revision string) (map[string]string, bool) {
	if !id.hasIndex {
		return nil, false
	}
	role, err := historicalRole(ms, id.roleName, revision)
	if err != nil {
		return nil, false
	}
	var roleTemplateHash string
	if revision != "" {
		roleTemplateHash, _ = utils.RoleRevisionHash(ms, id.roleName)
	}
	expected := utils.ExpectedPodLabels(role, ms, id.groupName, id.roleID, id.podIndex, revision, roleTemplateHash)
	if revision == "" {
		delete(expected, workloadv1alpha1.RevisionLabelKey)
	}
	if roleTemplateHash == "" {
		delete(expected, workloadv1alpha1.RoleTemplateHashLabelKey)
	}
	if hasPlugin(ms, plugins.LWSLabelsPluginName) {
		if lwsLabels, err := plugins.ExpectedLWSLabels(ms, id.groupName, targetPod.Name, targetPod.Namespace, id.podIndex == 0); err == nil {
			maps.Copy(expected, lwsLabels)
		}
	}
	return expected, true
}

func (c *ModelServingController) resolvePodRevision(
	ms *workloadv1alpha1.ModelServing,
	id podIdentity,
	oldPod, targetPod *corev1.Pod,
) string {
	state := inplace.StateOf(targetPod)
	if state != nil && state.Done && state.Revision != "" {
		return state.Revision
	}

	inProgress := state.InProgress()
	if oldPod != nil {
		if rev := utils.ObjectRevision(oldPod); rev != "" && (!inProgress || rev != state.Revision) {
			return rev
		}
	}
	if rev := utils.ObjectRevision(targetPod); rev != "" && (!inProgress || rev != state.Revision) {
		return rev
	}
	if c.store != nil {
		if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
			if roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), id.groupName, id.roleName); err == nil {
				for _, r := range roles {
					if r.Name == id.roleID && r.Revision != "" {
						return r.Revision
					}
				}
			}
		}
		if rev, ok := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), id.groupName); ok && rev != "" {
			return rev
		}
	}
	if ms.Status.CurrentRevision != "" && (!inProgress || ms.Status.CurrentRevision != state.Revision) {
		return ms.Status.CurrentRevision
	}
	if !inProgress {
		return utils.ModelServingRevision(ms)
	}
	return ""
}

func diffPodLabels(current, expected map[string]string, id podIdentity, forceIncludeGroup, unresolved bool) map[string]any {
	patchLabels := map[string]any{}
	for k, want := range expected {
		if got, ok := current[k]; !ok || got != want {
			patchLabels[k] = want
		}
	}
	if forceIncludeGroup {
		if _, ok := patchLabels[workloadv1alpha1.GroupNameLabelKey]; !ok {
			if _, present := current[workloadv1alpha1.GroupNameLabelKey]; !present {
				patchLabels[workloadv1alpha1.GroupNameLabelKey] = id.groupName
			}
		}
	}
	// Remove a spurious EntryLabelKey added to a worker Pod unless the role's
	// WorkerTemplate explicitly defines it (in which case expected has it).
	if !unresolved && id.hasIndex && id.podIndex > 0 {
		if _, inExpected := expected[workloadv1alpha1.EntryLabelKey]; !inExpected {
			if _, hasEntry := current[workloadv1alpha1.EntryLabelKey]; hasEntry {
				patchLabels[workloadv1alpha1.EntryLabelKey] = nil
			}
		}
	}
	return patchLabels
}

func hasPlugin(ms *workloadv1alpha1.ModelServing, name string) bool {
	if ms == nil {
		return false
	}
	for _, p := range ms.Spec.Plugins {
		if p.Name == name {
			return true
		}
	}
	return false
}
