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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// A round belongs to one observation and one owner, never to the controller.
// Read errors are retained only until that observation ends.
type coordinatedRound struct {
	history    *revisionHistory
	owner      types.UID
	podsLoaded bool
	pods       []*corev1.Pod
	podErr     error
	revisions  map[string]coordinatedRevision
	snapshots  map[string]controllerRevisionObservation
}

type controllerRevisionObservation struct {
	revision *appsv1.ControllerRevision
	err      error
}

type coordinatedRevision struct {
	roles map[string]api.Role
	err   error
}

type coordinatedRoundKey struct{}

func withCoordinatedRound(ctx context.Context, ms *api.ModelServing) context.Context {
	if roleCoordination(ms) == nil || ms.ResourceVersion == "" {
		return ctx
	}
	return withObservationRound(ctx, ms)
}

// An observation is local to one readiness check or planning/action round.
// Callers start a fresh round before using later live facts.
func withObservationRound(ctx context.Context, ms *api.ModelServing) context.Context {
	if ms.ResourceVersion == "" {
		return ctx
	}
	return context.WithValue(ctx, coordinatedRoundKey{}, &coordinatedRound{owner: ms.UID, revisions: make(map[string]coordinatedRevision), snapshots: make(map[string]controllerRevisionObservation)})
}

// Full rendering and coordination must share the same immutable snapshot within
// a planning round. Action rounds still refresh observations before a mutation.
func (c *ModelServingController) controllerRevisionInRound(ctx context.Context, ms *api.ModelServing, revision string) (*appsv1.ControllerRevision, error) {
	round := coordinatedRoundOf(ctx, ms)
	if round != nil {
		if observed, ok := round.snapshots[revision]; ok {
			return observed.revision, observed.err
		}
	}
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if round != nil {
		round.snapshots[revision] = controllerRevisionObservation{revision: cr, err: err}
	}
	return cr, err
}

// ensureObservationRound reuses the caller's round or starts one, so that the
// reads of a single check share one Pod list.
func ensureObservationRound(ctx context.Context, ms *api.ModelServing) context.Context {
	if coordinatedRoundOf(ctx, ms) != nil {
		return ctx
	}
	return withObservationRound(ctx, ms)
}

func coordinatedRoundOf(ctx context.Context, ms *api.ModelServing) *coordinatedRound {
	round, _ := ctx.Value(coordinatedRoundKey{}).(*coordinatedRound)
	if round != nil && round.owner == ms.UID {
		return round
	}
	return nil
}

func (c *ModelServingController) coordinatedPods(ctx context.Context, ms *api.ModelServing, selector labels.Selector) ([]*corev1.Pod, error) {
	round := coordinatedRoundOf(ctx, ms)
	if round == nil || !round.podsLoaded {
		live, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{api.ModelServingNameLabelKey: ms.Name}.AsSelector().String()})
		var pods []*corev1.Pod
		if err == nil {
			for i := range live.Items {
				if utils.IsOwnedByModelServingWithUID(&live.Items[i], ms.UID) {
					pods = append(pods, &live.Items[i])
				}
			}
		}
		if round == nil {
			return selectCoordinatedPods(pods, selector), err
		}
		round.pods, round.podErr, round.podsLoaded = pods, err, true
	}
	return selectCoordinatedPods(round.pods, selector), round.podErr
}

func selectCoordinatedPods(pods []*corev1.Pod, selector labels.Selector) []*corev1.Pod {
	var result []*corev1.Pod
	for _, pod := range pods {
		if selector.Matches(labels.Set(pod.Labels)) {
			result = append(result, pod)
		}
	}
	return result
}

// Even a partially patched Role has spent a target start. InPlace deliberately
// retains the source labels until all members finish, so labels alone cannot
// represent this reservation.
func (c *ModelServingController) coordinatedInPlaceStarted(ctx context.Context, ms *api.ModelServing, group, role, id string) bool {
	if roleCoordination(ms) == nil || ms.Spec.RolloutStrategy.UpdateMethod != api.InPlaceUpdateMethod || ms.ResourceVersion == "" {
		return false
	}
	pods, err := c.coordinatedPods(ctx, ms, labels.Set{api.GroupNameLabelKey: group, api.RoleLabelKey: role, api.RoleIDKey: id}.AsSelector())
	if err != nil {
		return false
	}
	for _, pod := range pods {
		state := inplace.StateOf(pod)
		if pod.DeletionTimestamp == nil && state.InProgress() && state.Revision == c.revisionTarget(ctx, ms) {
			return true
		}
	}
	return false
}

// Recheck physical occupancy and first-start dependencies immediately before
// creating a Role. A partially accepted creation may finish its missing workers
// without obtaining another start; a terminating or different incarnation may
// not be adopted. Observed formal expansion counts against later replacements.
func (c *ModelServingController) coordinatedCreationContext(ctx context.Context, ms *api.ModelServing, group string, desired, applied api.Role, ordinal int, revision string) (context.Context, error) {
	if ms.ResourceVersion == "" || !isRoleRollingUpdate(ms) {
		return ctx, nil
	}
	ctx = withCoordinatedRound(ctx, ms)
	pods, err := c.coordinatedPods(ctx, ms, labels.Set{api.GroupNameLabelKey: group, api.RoleLabelKey: desired.Name}.AsSelector())
	if err != nil {
		return ctx, err
	}
	workload, err := c.revisionHistory(ctx, ms).workload(ctx, revision)
	if err != nil {
		return ctx, err
	}
	expectedHash, err := utils.RoleRevisionHash(workload, desired.Name)
	if err != nil {
		return ctx, err
	}
	id := utils.GenerateRoleID(desired.Name, ordinal)
	occupied, replay := sets.New[string](), false
	for _, pod := range pods {
		occupied.Insert(utils.GetRoleID(pod))
		if utils.GetRoleID(pod) != id {
			continue
		}
		sameTemplate := utils.ObjectRoleTemplateHash(pod) == expectedHash
		if !sameTemplate && utils.ObjectRevision(pod) == revision {
			// Upgrade replay may retain a pre-canonical hash, but arbitrary hashes
			// must not authorize adoption of a different physical incarnation.
			cr, err := c.controllerRevisionInRound(ctx, ms, revision)
			if err != nil {
				return ctx, err
			}
			if cr == nil {
				return ctx, fmt.Errorf("ControllerRevision %s is missing", revision)
			}
			if err := validateControllerRevisionOwner(ms, cr); err != nil {
				return ctx, err
			}
			roles, err := utils.GetRolesFromControllerRevision(cr)
			if err != nil {
				return ctx, err
			}
			for _, historical := range roles {
				if historical.Name != desired.Name {
					continue
				}
				observedHash := utils.ObjectRoleTemplateHash(pod)
				sameTemplate = observedHash == utils.CalRoleTemplateHash(historical)
				// Before canonical JSON hashing, Role identity used the typed
				// template's spew hash. Derive that value from trusted raw history,
				// before normalization removes overwritten inputs or adds defaults.
				historical.Replicas = nil
				historical.RollingUpdateConfiguration = api.RollingUpdateConfiguration{}
				sameTemplate = sameTemplate || observedHash == utils.Revision(historical)
				break
			}
		}

		if pod.DeletionTimestamp != nil || utils.ObjectRevision(pod) != revision || !sameTemplate {
			return ctx, fmt.Errorf("Role %s is occupied by a different or terminating incarnation", id)
		}
		replay = true
	}
	surge, err := utils.GetMaxSurgeForRole(desired)
	if err != nil {
		return ctx, err
	}
	if !replay && occupied.Len() >= roleReplicas(desired)+surge {
		return ctx, fmt.Errorf("Role %s has no physical creation capacity", desired.Name)
	}
	// First creation must establish the complete initial Gang. There is no
	// previous rollout baseline whose first target start needs gating yet.
	isTarget, err := utils.EqualRoleRevisions("", "", workload, ms, desired.Name)
	if err != nil {
		return ctx, err
	}
	if !replay && ms.Status.CurrentRevision != "" && isTarget {
		if err := c.refreshCoordinatedAvailabilityInRound(ctx, ms); err != nil {
			return ctx, err
		}
		policy, err := c.resolveRoleRolloutPolicy(ctx, ms, c.revisionTarget(ctx, ms))
		if err != nil {
			return ctx, err
		}
		if !policy.allowTargetStart(group, desired.Name) {
			return ctx, fmt.Errorf("Role %s is waiting for its target dependencies", desired.Name)
		}
	}
	return ctx, nil
}
