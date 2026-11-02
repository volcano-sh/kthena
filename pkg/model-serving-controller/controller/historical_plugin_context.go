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

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// modelServingForRevision restores rendering inputs without replacing current
// replica counts or rollout controls. Legacy snapshots contain only Roles: they
// cannot establish a different historical plugin list or scheduler.
func (c *ModelServingController) modelServingForRevision(ctx context.Context, ms *workload.ModelServing, revision string) (*workload.ModelServing, error) {
	return c.resolveModelServingRevision(ctx, ms, revision, false)
}

// preserveLegacyCounts is used only where the existing partition logic already
// selected the entire legacy Role snapshot, including its operational counts.
func (c *ModelServingController) resolveModelServingRevision(ctx context.Context, ms *workload.ModelServing, revision string, preserveLegacyCounts bool) (*workload.ModelServing, error) {
	if revision == "" {
		return ms, nil
	}
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil {
		return nil, fmt.Errorf("get historical ControllerRevision %s: %w", revision, err)
	}
	if cr == nil {
		if revision == utils.ModelServingRevision(ms) {
			return ms, nil
		}
		return nil, fmt.Errorf("historical ControllerRevision %s was not found", revision)
	}
	owner := metav1.GetControllerOf(cr)
	if cr.Namespace != ms.Namespace || owner == nil || owner.APIVersion != workload.SchemeGroupVersion.String() || owner.Kind != workload.ModelServingKind.Kind || owner.Name != ms.Name || owner.UID != ms.UID {
		return nil, fmt.Errorf("historical ControllerRevision %s is not controlled by ModelServing %s/%s UID %s", cr.Name, ms.Namespace, ms.Name, ms.UID)
	}
	switch version := cr.Annotations[utils.ControllerRevisionDataVersionAnnotation]; version {
	case utils.ControllerRevisionDataVersionV1:
		return utils.ApplyRevision(ms, cr)
	case "":
		roles, err := utils.GetRolesFromControllerRevision(cr)
		if err != nil {
			return nil, fmt.Errorf("read historical ControllerRevision %s: %w", revision, err)
		}
		if len(roles) == 0 {
			return nil, fmt.Errorf("historical ControllerRevision %s has no Roles", revision)
		}
		result := ms.DeepCopy()
		result.Spec.Template.Roles = nil
		seen := make(map[string]bool, len(roles))
		for _, role := range roles {
			if role.Name == "" || seen[role.Name] {
				return nil, fmt.Errorf("historical ControllerRevision %s has an empty or duplicate Role name %q", revision, role.Name)
			}
			seen[role.Name] = true
			for _, current := range ms.Spec.Template.Roles {
				if !preserveLegacyCounts && current.Name == role.Name {
					// Only Pod rendering fields are historical. Preserve current
					// scale and rollout decisions just as ApplyRevision does.
					current.EntryTemplate = role.EntryTemplate
					current.WorkerTemplate = role.WorkerTemplate
					current.WorkerReplicas = role.WorkerReplicas
					role = current
					break
				}
			}
			result.Spec.Template.Roles = append(result.Spec.Template.Roles, *role.DeepCopy())
		}
		return result, nil
	default:
		return nil, fmt.Errorf("historical ControllerRevision %s has unsupported data version %q", revision, version)
	}
}

func historicalRole(ms *workload.ModelServing, name, revision string) (workload.Role, error) {
	for _, role := range ms.Spec.Template.Roles {
		if role.Name == name {
			return *role.DeepCopy(), nil
		}
	}
	return workload.Role{}, fmt.Errorf("historical ControllerRevision %s has no Role %s", revision, name)
}

func (c *ModelServingController) existingRoleRevision(ms *workload.ModelServing, groupName string, role datastore.Role, current string) string {
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workload.RoleRollingUpdate {
		if role.Revision != "" {
			return role.Revision
		}
	} else if revision, ok := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), groupName); ok && revision != "" {
		return revision
	}
	return current
}
