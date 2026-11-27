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

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/bootstrap"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// newBootstrapAdmitter snapshots every live ServingGroup of ms into the
// whole-ServingGroup admission state for this reconcile. Ready sources and
// in-flight seed ServingGroups are read from the datastore; pools are keyed by
// (role name, role template hash), the same input the managed plugin turns into
// MX_MODEL_REVISION.
func (c *ModelServingController) newBootstrapAdmitter(ctx context.Context, ms *workloadv1alpha1.ModelServing) (*bootstrap.Admitter, error) {
	if !bootstrap.Enabled(ms) {
		return nil, nil
	}
	ctx = c.withRevisionHistory(ctx, ms)
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil, fmt.Errorf("list ServingGroups for bootstrap admission: %w", err)
	}
	observations := make([]bootstrap.GroupObservation, 0, len(groups))
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return nil, fmt.Errorf("list Roles of ServingGroup %s for bootstrap admission: %w", group.Name, err)
		}
		var obs bootstrap.GroupObservation
		observedRoles := make(map[string]struct{}, len(roles))
		for roleName, instances := range roles {
			if !bootstrap.InScope(ms, roleName) {
				continue
			}
			for _, role := range instances {
				if role == nil || role.Status == datastore.RoleDeleting {
					continue
				}
				observedRoles[roleName] = struct{}{}
				// Legacy records without a hash resolve it from their revision;
				// fall back to the stored RoleTemplateHash when history is absent.
				templateHash, ok := c.roleTemplateHashForComparison(ctx, ms, group, roleName, *role)
				if !ok {
					templateHash = role.RoleTemplateHash
				}
				if templateHash == "" {
					continue
				}
				key := bootstrap.PoolKey{Role: roleName, TemplateHash: templateHash}
				obs.Pools = append(obs.Pools, key)
				if role.Status == datastore.RoleRunning {
					obs.ReadyReplicas = append(obs.ReadyReplicas, key)
				}
			}
		}
		// A partially created ServingGroup whose in-scope roles are not all in
		// the datastore yet still counts once toward its group revision's pools.
		if group.Revision != "" {
			if groupWorkload, histErr := c.revisionHistory(ctx, ms).workload(ctx, group.Revision); histErr == nil {
				for _, role := range groupWorkload.Spec.Template.Roles {
					if !bootstrap.InScope(ms, role.Name) || roleReplicas(role) <= 0 {
						continue
					}
					if _, seen := observedRoles[role.Name]; seen {
						continue
					}
					obs.Pools = append(obs.Pools, bootstrap.PoolFor(groupWorkload, role.Name))
				}
			}
		}
		observations = append(observations, obs)
	}
	return bootstrap.NewAdmitter(ms, observations), nil
}

func (c *ModelServingController) emitBootstrapEvents(ms *workloadv1alpha1.ModelServing, admitter *bootstrap.Admitter) {
	for _, pool := range admitter.Summary() {
		c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "BootstrapBatchCreated",
			fmt.Sprintf("Created %d ServingGroup(s) for Role %s (template %s) with %d ready peer-to-peer source(s)",
				pool.AdmittedGroups, pool.Key.Role, pool.Key.TemplateHash, pool.Ready))
	}
}
