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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/bootstrap"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// newBootstrapAdmitter snapshots every live role replica of ms into the
// admission state for this reconcile. Sources and in-flight replicas are read
// from the datastore; pools are keyed by role template hash, the same input the
// managed plugin turns into MX_MODEL_REVISION.
func (c *ModelServingController) newBootstrapAdmitter(ms *workloadv1alpha1.ModelServing) (*bootstrap.Admitter, error) {
	if !bootstrap.Enabled(ms) {
		return nil, nil
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil, fmt.Errorf("list ServingGroups for bootstrap admission: %w", err)
	}
	var replicas []bootstrap.Replica
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return nil, fmt.Errorf("list Roles of ServingGroup %s for bootstrap admission: %w", group.Name, err)
		}
		for roleName, instances := range roles {
			if _, ok := bootstrap.RoleConfig(ms, roleName); !ok {
				continue
			}
			for _, role := range instances {
				if role == nil || role.Status == datastore.RoleDeleting {
					continue
				}
				// Legacy records without a hash resolve it from their revision.
				templateHash, _ := c.resolveRoleTemplateHashForComparison(ms, group, roleName, *role)
				replicas = append(replicas, bootstrap.Replica{
					Role:         roleName,
					TemplateHash: templateHash,
					Running:      role.Status == datastore.RoleRunning,
				})
			}
		}
	}
	gang := ms.Spec.SchedulerName == "volcano" && c.podGroupManager != nil && c.podGroupManager.HasPodGroupCRD()
	return bootstrap.NewAdmitter(ms, replicas, bootstrap.Options{GangScheduling: gang}), nil
}

// planBootstrapGroupCore admits the missing gang-minimum replicas of an
// existing ServingGroup as one unit before its roles are reconciled.
func (c *ModelServingController) planBootstrapGroupCore(admitter *bootstrap.Admitter, ms *workloadv1alpha1.ModelServing, groupName string, roles []workloadv1alpha1.Role) error {
	if admitter == nil {
		return nil
	}
	present := make(map[string]int, len(roles))
	for _, role := range roles {
		if !admitter.InScope(role.Name) {
			continue
		}
		roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, role.Name)
		if err != nil {
			return fmt.Errorf("list Role %s of ServingGroup %s for bootstrap admission: %w", role.Name, groupName, err)
		}
		present[role.Name] = len(roleList)
	}
	admitter.PlanGroupCore(groupName, roles, present)
	return nil
}

func (c *ModelServingController) emitBootstrapEvents(ms *workloadv1alpha1.ModelServing, admitter *bootstrap.Admitter) {
	for _, pool := range admitter.Summary() {
		c.emitRoleStatusEvent(ms, corev1.EventTypeNormal, "BootstrapBatchCreated",
			fmt.Sprintf("Created %d replica(s) of Role %s (template %s) with %d ready peer-to-peer source(s)",
				pool.Admitted, pool.Key.Role, pool.Key.TemplateHash, pool.Ready))
	}
}
