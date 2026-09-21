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
	"errors"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type templateComparison int

const (
	templateUnknown templateComparison = iota
	templateEquivalent
	templateDifferent
)

type revisionSnapshot struct {
	roles []workloadv1alpha1.Role
	err   error
}

type roleRevision struct {
	revision string
	roleName string
}

// revisionHistory is a read-only view of ControllerRevisions for one reconcile.
// Missing or invalid history is cached only for this reconcile so the workqueue
// retry can observe repaired history without restarting the controller.
type revisionHistory struct {
	controller      *ModelServingController
	ms              *workloadv1alpha1.ModelServing
	snapshots       map[string]revisionSnapshot
	roleHashes      map[string]string
	roleComparisons map[roleRevision]templateComparison
	comparisonErrs  map[string]error
}

type revisionHistoryKey struct{}

func (c *ModelServingController) withRevisionHistory(ctx context.Context, ms *workloadv1alpha1.ModelServing) context.Context {
	return context.WithValue(ctx, revisionHistoryKey{}, c.revisionHistory(ctx, ms))
}

func (c *ModelServingController) revisionHistory(ctx context.Context, ms *workloadv1alpha1.ModelServing) *revisionHistory {
	if history, ok := ctx.Value(revisionHistoryKey{}).(*revisionHistory); ok &&
		history.ms.UID == ms.UID && history.ms.Namespace == ms.Namespace && history.ms.Name == ms.Name &&
		history.ms.Generation == ms.Generation {
		return history
	}
	return &revisionHistory{
		controller:      c,
		ms:              ms,
		snapshots:       make(map[string]revisionSnapshot),
		roleHashes:      make(map[string]string),
		roleComparisons: make(map[roleRevision]templateComparison),
		comparisonErrs:  make(map[string]error),
	}
}

func (h *revisionHistory) decode(cr *appsv1.ControllerRevision) revisionSnapshot {
	if cr == nil {
		return revisionSnapshot{err: fmt.Errorf("ControllerRevision is missing")}
	}
	if !metav1.IsControlledBy(cr, h.ms) {
		return revisionSnapshot{err: fmt.Errorf("ControllerRevision %s is not controlled by the current ModelServing UID", cr.Name)}
	}
	roles, err := utils.GetRolesFromControllerRevision(cr)
	if err == nil && len(roles) == 0 {
		err = fmt.Errorf("ControllerRevision %s has no Role templates", cr.Name)
	}
	return revisionSnapshot{roles: roles, err: err}
}

func (h *revisionHistory) roles(ctx context.Context, revision string) ([]workloadv1alpha1.Role, error) {
	if snapshot, ok := h.snapshots[revision]; ok {
		return snapshot.roles, snapshot.err
	}

	snapshot := revisionSnapshot{err: fmt.Errorf("revision or Kubernetes client is missing")}
	if revision != "" && h.controller.kubeClientSet != nil {
		cr, err := utils.GetControllerRevision(ctx, h.controller.kubeClientSet, h.ms, revision)
		if err != nil {
			snapshot.err = err
		} else {
			snapshot = h.decode(cr)
		}
	}
	h.snapshots[revision] = snapshot
	if snapshot.err != nil {
		klog.Warningf("Cannot resolve revision %q for ModelServing %s/%s; template-driven creation and deletion are paused for affected replicas: %v",
			revision, h.ms.Namespace, h.ms.Name, snapshot.err)
		if h.controller.recorder != nil {
			h.controller.recorder.Eventf(h.ms, corev1.EventTypeWarning, "RevisionUnresolved",
				"Cannot resolve revision %q; template-driven creation and deletion are paused for affected replicas: %v", revision, snapshot.err)
		}
	}
	return snapshot.roles, snapshot.err
}

// desiredRevision uses the existing hash as a fast identity, then reuses an
// owned historical identity when its decoded Role templates are semantically
// equal. A genuinely different template is persisted before workload mutation.
func (h *revisionHistory) desiredRevision(ctx context.Context) (string, error) {
	computed := utils.ModelServingRevision(h.ms)
	// Follow the Kubernetes controller-history pattern: use the hash identity as
	// the fast path, but validate the immutable history before trusting it. A
	// collision or foreign owner must never be overwritten.
	exact, err := utils.GetControllerRevision(ctx, h.controller.kubeClientSet, h.ms, computed)
	if err != nil {
		return "", fmt.Errorf("get computed ControllerRevision: %w", err)
	}
	if exact != nil {
		snapshot := h.decode(exact)
		if snapshot.err != nil {
			return "", snapshot.err
		}
		if !utils.EqualRoleTemplatesForRevision(snapshot.roles, h.ms.Spec.Template.Roles) {
			return "", fmt.Errorf("ControllerRevision %s/%s has the computed hash but different template data", h.ms.Namespace, exact.Name)
		}
		h.snapshots[computed] = snapshot
		return computed, nil
	}

	selector := labels.SelectorFromSet(map[string]string{
		utils.ControllerRevisionLabelKey: h.ms.Name,
	})
	list, err := h.controller.kubeClientSet.AppsV1().ControllerRevisions(h.ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return "", fmt.Errorf("list ControllerRevisions: %w", err)
	}

	priority := func(cr appsv1.ControllerRevision) int {
		revision := cr.Labels[utils.ControllerRevisionRevisionLabelKey]
		if revision == h.ms.Status.UpdateRevision {
			return 2
		}
		if revision == h.ms.Status.CurrentRevision {
			return 1
		}
		return 0
	}
	sort.Slice(list.Items, func(i, j int) bool {
		left, right := list.Items[i], list.Items[j]
		if priority(left) != priority(right) {
			return priority(left) > priority(right)
		}
		if left.Revision != right.Revision {
			return left.Revision > right.Revision
		}
		if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
			return right.CreationTimestamp.Before(&left.CreationTimestamp)
		}
		return left.Name < right.Name
	})

	for i := range list.Items {
		cr := &list.Items[i]
		revision := cr.Labels[utils.ControllerRevisionRevisionLabelKey]
		if revision == "" || cr.Name != utils.GenerateControllerRevisionName(h.ms.Name, revision) {
			continue
		}
		snapshot := h.decode(cr)
		if snapshot.err != nil {
			continue
		}
		h.snapshots[revision] = snapshot
		if utils.EqualRoleTemplatesForRevision(snapshot.roles, h.ms.Spec.Template.Roles) {
			return revision, nil
		}
	}

	cr, err := utils.CreateControllerRevision(ctx, h.controller.kubeClientSet, h.ms, computed, h.ms.Spec.Template.Roles)
	if err != nil {
		return "", fmt.Errorf("persist desired ControllerRevision: %w", err)
	}
	h.snapshots[computed] = h.decode(cr)
	return computed, nil
}

func (h *revisionHistory) errors() error {
	keys := make([]string, 0, len(h.snapshots)+len(h.comparisonErrs))
	for revision, snapshot := range h.snapshots {
		if snapshot.err != nil {
			key := fmt.Sprintf("revision %q", revision)
			h.comparisonErrs[key] = snapshot.err
		}
	}
	for key := range h.comparisonErrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	joined := make([]error, 0, len(keys))
	for _, key := range keys {
		joined = append(joined, fmt.Errorf("resolve %s: %w", key, h.comparisonErrs[key]))
	}
	return errors.Join(joined...)
}

func (h *revisionHistory) recordComparisonError(key string, err error) {
	if err != nil {
		h.comparisonErrs[key] = err
	}
}

func (c *ModelServingController) compareServingGroupTemplate(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	group datastore.ServingGroup,
	targetRevision string,
) templateComparison {
	if group.Revision == targetRevision {
		// A ServingGroup revision alone cannot describe a partially updated
		// RoleRollingUpdate. Continue with the observed Role identities below.
	} else {
		// Validation requires at least one Role. Keep controller unit fixtures that
		// intentionally omit templates on the hash path; real ModelServings always
		// take the persisted semantic path below.
		if len(ms.Spec.Template.Roles) == 0 {
			return templateDifferent
		}
		roles, err := c.revisionHistory(ctx, ms).roles(ctx, group.Revision)
		if err != nil {
			return templateUnknown
		}
		if !utils.EqualRoleTemplatesForRevision(roles, ms.Spec.Template.Roles) {
			return templateDifferent
		}
	}

	if c.store == nil {
		return templateEquivalent
	}
	history := c.revisionHistory(ctx, ms)
	for _, desired := range ms.Spec.Template.Roles {
		observedRoles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, desired.Name)
		if err != nil {
			history.recordComparisonError(
				fmt.Sprintf("ServingGroup %s Role %s", group.Name, desired.Name), err)
			return templateUnknown
		}
		for _, observed := range observedRoles {
			comparison := c.compareRoleTemplate(ctx, ms, group, desired, observed)
			if comparison != templateEquivalent {
				return comparison
			}
		}
	}
	return templateEquivalent
}

func (c *ModelServingController) compareRoleTemplate(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	desired workloadv1alpha1.Role,
	observed datastore.Role,
) templateComparison {
	history := c.revisionHistory(ctx, ms)
	expectedHash, ok := history.roleHashes[desired.Name]
	if !ok {
		expectedHash = utils.CalRoleTemplateHash(desired)
		history.roleHashes[desired.Name] = expectedHash
	}
	if observed.RoleTemplateHash == expectedHash {
		return templateEquivalent
	}

	revision := observed.Revision
	if revision == "" {
		revision = servingGroup.Revision
	}
	key := roleRevision{revision: revision, roleName: desired.Name}
	if comparison, ok := history.roleComparisons[key]; ok {
		return comparison
	}
	roles, err := history.roles(ctx, revision)
	if err != nil {
		return templateUnknown
	}
	for _, historical := range roles {
		if historical.Name != desired.Name {
			continue
		}
		comparison := templateDifferent
		if utils.EqualRoleTemplateForRevision(historical, desired) {
			comparison = templateEquivalent
		}
		history.roleComparisons[key] = comparison
		return comparison
	}

	err = fmt.Errorf("Role %s is missing from ControllerRevision %s", desired.Name, revision)
	history.recordComparisonError(fmt.Sprintf("revision %q Role %s", revision, desired.Name), err)
	return templateUnknown
}

func (c *ModelServingController) roleTemplateHashForComparison(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	servingGroup datastore.ServingGroup,
	roleName string,
	role datastore.Role,
) (string, bool) {
	var desired *workloadv1alpha1.Role
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			desired = &ms.Spec.Template.Roles[i]
			break
		}
	}
	if desired == nil {
		return "", false
	}
	expectedHash := c.revisionHistory(ctx, ms).roleHashes[desired.Name]
	if expectedHash == "" {
		expectedHash = utils.CalRoleTemplateHash(*desired)
		c.revisionHistory(ctx, ms).roleHashes[desired.Name] = expectedHash
	}
	switch c.compareRoleTemplate(ctx, ms, servingGroup, *desired, role) {
	case templateEquivalent:
		return expectedHash, true
	case templateUnknown:
		return "", false
	default:
		revision := role.Revision
		if revision == "" {
			revision = servingGroup.Revision
		}
		roles, err := c.revisionHistory(ctx, ms).roles(ctx, revision)
		if err != nil {
			return "", false
		}
		for _, historical := range roles {
			if historical.Name == roleName {
				return utils.CalRoleTemplateHash(historical), true
			}
		}
		return "", false
	}
}
