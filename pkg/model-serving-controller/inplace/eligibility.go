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
	"fmt"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const ineligibleChange = "cannot change while rolloutStrategy.updateMethod is InPlace; use Recreate"

// ResolveTemplate returns a copy of the template with restartPolicy and imagePullPolicy resolved as the API server defaults them.
// Other omitted fields are kept as they are, so setting one of them to its default value still counts as a change.
func ResolveTemplate(template *workloadv1alpha1.PodTemplateSpec) corev1.PodTemplateSpec {
	resolved := corev1.PodTemplateSpec{Spec: *template.Spec.DeepCopy()}
	if template.Metadata != nil {
		metadata := template.Metadata.DeepCopy()
		resolved.Labels = metadata.Labels
		resolved.Annotations = metadata.Annotations
	}
	if resolved.Spec.RestartPolicy == "" {
		resolved.Spec.RestartPolicy = corev1.RestartPolicyAlways
	}
	for i := range resolved.Spec.InitContainers {
		resolvePullPolicy(&resolved.Spec.InitContainers[i])
	}
	for i := range resolved.Spec.Containers {
		resolvePullPolicy(&resolved.Spec.Containers[i])
	}
	return resolved
}

// resolvePullPolicy follows the API server default: Always for the latest tag or no tag, and IfNotPresent otherwise.
func resolvePullPolicy(container *corev1.Container) {
	if container.ImagePullPolicy != "" {
		return
	}
	container.ImagePullPolicy = corev1.PullIfNotPresent
	if imageTag(container.Image) == "latest" {
		container.ImagePullPolicy = corev1.PullAlways
	}
}

func imageTag(image string) string {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}
	if tagged, ok := named.(reference.Tagged); ok {
		return tagged.Tag()
	}
	if _, ok := named.(reference.Digested); ok {
		return ""
	}
	return "latest"
}

// CheckEligibility reports every change from oldMS to newMS that an in-place update cannot apply.
// Only regular-container images and the scaling and rollout fields documented on UpdateMethod may differ.
func CheckEligibility(oldMS, newMS *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	if oldMS.Spec.SchedulerName != newMS.Spec.SchedulerName {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("schedulerName"), ineligibleChange))
	}
	if !equality.Semantic.DeepEqual(oldMS.Spec.Plugins, newMS.Spec.Plugins) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("plugins"), ineligibleChange))
	}

	oldGroup, newGroup := &oldMS.Spec.Template, &newMS.Spec.Template
	templatePath := specPath.Child("template")
	if !equality.Semantic.DeepEqual(oldGroup.GangPolicy, newGroup.GangPolicy) {
		allErrs = append(allErrs, field.Forbidden(templatePath.Child("gangPolicy"), ineligibleChange))
	}
	if !equality.Semantic.DeepEqual(oldGroup.NetworkTopology, newGroup.NetworkTopology) {
		allErrs = append(allErrs, field.Forbidden(templatePath.Child("networkTopology"), ineligibleChange))
	}

	rolesPath := templatePath.Child("roles")
	if !sameRoleNames(oldGroup.Roles, newGroup.Roles) {
		return append(allErrs, field.Forbidden(rolesPath, "Role names and order "+ineligibleChange))
	}
	for i := range newGroup.Roles {
		oldRole, newRole := &oldGroup.Roles[i], &newGroup.Roles[i]
		rolePath := rolesPath.Index(i)
		if oldRole.WorkerReplicas != newRole.WorkerReplicas {
			allErrs = append(allErrs, field.Forbidden(rolePath.Child("workerReplicas"), ineligibleChange))
		}
		allErrs = append(allErrs, checkTemplate(&oldRole.EntryTemplate, &newRole.EntryTemplate, rolePath.Child("entryTemplate"))...)
		switch {
		case oldRole.WorkerTemplate == nil && newRole.WorkerTemplate == nil:
		case oldRole.WorkerTemplate == nil || newRole.WorkerTemplate == nil:
			allErrs = append(allErrs, field.Forbidden(rolePath.Child("workerTemplate"), ineligibleChange))
		default:
			allErrs = append(allErrs, checkTemplate(oldRole.WorkerTemplate, newRole.WorkerTemplate, rolePath.Child("workerTemplate"))...)
		}
	}
	return allErrs
}

func checkTemplate(oldTemplate, newTemplate *workloadv1alpha1.PodTemplateSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	oldResolved := ResolveTemplate(oldTemplate)
	newResolved := ResolveTemplate(newTemplate)
	containersPath := fldPath.Child("spec", "containers")
	if !sameContainerNames(oldResolved.Spec.Containers, newResolved.Spec.Containers) {
		return field.ErrorList{field.Forbidden(containersPath, "container names and order "+ineligibleChange)}
	}

	for i := range newResolved.Spec.Containers {
		oldContainer, newContainer := &oldResolved.Spec.Containers[i], &newResolved.Spec.Containers[i]
		if oldContainer.ImagePullPolicy != newContainer.ImagePullPolicy {
			allErrs = append(allErrs, field.Invalid(containersPath.Index(i).Child("imagePullPolicy"), newContainer.ImagePullPolicy,
				fmt.Sprintf("effective imagePullPolicy %s; set it to %s explicitly to keep the current pull behavior", ineligibleChange, oldContainer.ImagePullPolicy)))
		}
		// Undo the image change on the copy, so any remaining difference is not an image change.
		newContainer.Image = oldContainer.Image
		newContainer.ImagePullPolicy = oldContainer.ImagePullPolicy
	}
	if !equality.Semantic.DeepEqual(oldResolved, newResolved) {
		allErrs = append(allErrs, field.Forbidden(fldPath, "fields other than regular-container images "+ineligibleChange))
	}
	return allErrs
}

func sameRoleNames(oldRoles, newRoles []workloadv1alpha1.Role) bool {
	if len(oldRoles) != len(newRoles) {
		return false
	}
	for i := range oldRoles {
		if oldRoles[i].Name != newRoles[i].Name {
			return false
		}
	}
	return true
}

func sameContainerNames(oldContainers, newContainers []corev1.Container) bool {
	if len(oldContainers) != len(newContainers) {
		return false
	}
	for i := range oldContainers {
		if oldContainers[i].Name != newContainers[i].Name {
			return false
		}
	}
	return true
}
