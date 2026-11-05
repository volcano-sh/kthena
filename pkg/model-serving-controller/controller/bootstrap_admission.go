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
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/bootstrap"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// newBootstrapAdmitter snapshots every live role replica of ms into the
// admission state for this reconcile. Sources and in-flight replicas are read
// from the datastore and the Pods' configuration annotation, never persisted.
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
			if _, inScope := bootstrap.RoleConfig(ms, roleName); !inScope {
				continue
			}
			for roleID, role := range instances {
				if role == nil || role.Status == datastore.RoleDeleting {
					continue
				}
				hash, known := c.observedBootstrapConfigHash(ms, group.Name, roleName, roleID)
				replicas = append(replicas, bootstrap.Replica{
					Role:         roleName,
					TemplateHash: role.RoleTemplateHash,
					ConfigHash:   hash,
					HashKnown:    known,
					Running:      role.Status == datastore.RoleRunning,
				})
			}
		}
	}
	gang := ms.Spec.SchedulerName == "volcano" && c.podGroupManager != nil && c.podGroupManager.HasPodGroupCRD()
	return bootstrap.NewAdmitter(ms, replicas, bootstrap.Options{
		ServerAddress:  c.modelExpressServerAddress,
		GangScheduling: gang,
	})
}

// observedBootstrapConfigHash returns the configuration hash shared by all
// live Pods of a role replica. known is false when no Pod has been observed.
// Pods that disagree or lack the annotation yield an empty hash, so the replica
// can never be a source for the current configuration.
func (c *ModelServingController) observedBootstrapConfigHash(ms *workloadv1alpha1.ModelServing, group, roleName, roleID string) (string, bool) {
	if c.podsInformer == nil {
		return "", false
	}
	pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, group, roleName, roleID))
	if err != nil {
		return "", false
	}
	hash, observed := "", false
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		podHash := pod.Annotations[workloadv1alpha1.BootstrapConfigHashAnnotationKey]
		if !observed {
			hash, observed = podHash, true
			continue
		}
		if podHash != hash {
			return "", true
		}
	}
	return hash, observed
}

// planBootstrapGroupCore admits the missing gang-minimum replicas of an
// existing ServingGroup as one unit before its roles are reconciled.
func (c *ModelServingController) planBootstrapGroupCore(admitter *bootstrap.Admitter, ms *workloadv1alpha1.ModelServing, groupName string, roles []workloadv1alpha1.Role) error {
	if admitter == nil {
		return nil
	}
	present := make(map[string]sets.Set[int], len(roles))
	for _, role := range roles {
		if !admitter.InScope(role.Name) {
			continue
		}
		roleList, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, role.Name)
		if err != nil {
			return fmt.Errorf("list Role %s of ServingGroup %s for bootstrap admission: %w", role.Name, groupName, err)
		}
		ordinals := sets.New[int]()
		for _, instance := range roleList {
			if _, ordinal := utils.GetParentNameAndOrdinal(instance.Name); ordinal >= 0 {
				ordinals.Insert(ordinal)
			}
		}
		present[role.Name] = ordinals
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
