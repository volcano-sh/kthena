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

package inplace

import (
	"bytes"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// UpdateMethodOf returns Recreate when no update method is set.
func UpdateMethodOf(ms *workloadv1alpha1.ModelServing) workloadv1alpha1.UpdateMethod {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.UpdateMethod == "" {
		return workloadv1alpha1.RecreateUpdateMethod
	}
	return ms.Spec.RolloutStrategy.UpdateMethod
}

// OnlyImagesChanged reports whether newMS renders the same Pods as oldMS except for regular-container images.
// It copies the new images into oldMS and compares the revision data, so every other field that defines a revision is covered.
func OnlyImagesChanged(oldMS, newMS *workloadv1alpha1.ModelServing) (bool, error) {
	patched := oldMS.DeepCopy()
	for i := range patched.Spec.Template.Roles {
		role := &patched.Spec.Template.Roles[i]
		newRole := findRole(newMS, role.Name)
		if newRole == nil {
			return false, nil
		}
		copyImages(role, newRole)
	}
	oldData, err := utils.BuildRevisionData(patched)
	if err != nil {
		return false, err
	}
	newData, err := utils.BuildRevisionData(newMS)
	if err != nil {
		return false, err
	}
	return bytes.Equal(oldData, newData), nil
}

// OnlyRoleImagesChanged is OnlyImagesChanged for one Role, compared by its template hash.
func OnlyRoleImagesChanged(oldMS, newMS *workloadv1alpha1.ModelServing, roleName string) bool {
	oldRole, newRole := findRole(oldMS, roleName), findRole(newMS, roleName)
	if oldRole == nil || newRole == nil {
		return false
	}
	patched := oldRole.DeepCopy()
	copyImages(patched, newRole)
	return utils.CalRoleTemplateHash(*patched) == utils.CalRoleTemplateHash(*newRole)
}

func findRole(ms *workloadv1alpha1.ModelServing, roleName string) *workloadv1alpha1.Role {
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			return &ms.Spec.Template.Roles[i]
		}
	}
	return nil
}

func copyImages(role, from *workloadv1alpha1.Role) {
	copyTemplateImages(&role.EntryTemplate, &from.EntryTemplate)
	if role.WorkerTemplate != nil && from.WorkerTemplate != nil {
		copyTemplateImages(role.WorkerTemplate, from.WorkerTemplate)
	}
}

// copyTemplateImages copies images by container name, so any other container change still shows.
func copyTemplateImages(template, from *workloadv1alpha1.PodTemplateSpec) {
	images := make(map[string]string, len(from.Spec.Containers))
	for _, container := range from.Spec.Containers {
		images[container.Name] = container.Image
	}
	for i := range template.Spec.Containers {
		if image, ok := images[template.Spec.Containers[i].Name]; ok {
			template.Spec.Containers[i].Image = image
		}
	}
}
