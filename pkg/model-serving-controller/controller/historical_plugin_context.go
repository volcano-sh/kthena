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
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// modelServingForRevision restores rendering inputs without replacing current
// replica counts or rollout controls. Legacy snapshots contain only Roles: they
// cannot establish a different historical plugin list or scheduler.
func (c *ModelServingController) modelServingForRevision(ctx context.Context, ms *workload.ModelServing, revision string) (*workload.ModelServing, error) {
	return c.resolveModelServingRevision(ctx, ms, revision, false)
}

// Rendering always preserves current counts and controls. Coordination restores
// its separate historical replica baseline in readCoordinatedRevision.
func (c *ModelServingController) resolveModelServingRevision(ctx context.Context, ms *workload.ModelServing, revision string, _ bool) (*workload.ModelServing, error) {
	return c.revisionHistory(ctx, ms).workload(ctx, revision)
}

func validateControllerRevisionOwner(ms *workload.ModelServing, cr *appsv1.ControllerRevision) error {
	owner := metav1.GetControllerOf(cr)
	if cr.Namespace != ms.Namespace || owner == nil || owner.APIVersion != workload.SchemeGroupVersion.String() || owner.Kind != workload.ModelServingKind.Kind || owner.Name != ms.Name || owner.UID != ms.UID {
		return fmt.Errorf("historical ControllerRevision %s is not controlled by ModelServing %s/%s UID %s", cr.Name, ms.Namespace, ms.Name, ms.UID)
	}
	return nil
}

func historicalRole(ms *workload.ModelServing, name, revision string) (workload.Role, error) {
	for _, role := range ms.Spec.Template.Roles {
		if role.Name == name {
			return *role.DeepCopy(), nil
		}
	}
	return workload.Role{}, fmt.Errorf("historical ControllerRevision %s has no Role %s", revision, name)
}
