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

package webhook

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
)

func updateMethodOf(ms *workloadv1alpha1.ModelServing) workloadv1alpha1.UpdateMethod {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.UpdateMethod == "" {
		return workloadv1alpha1.RecreateUpdateMethod
	}
	return ms.Spec.RolloutStrategy.UpdateMethod
}

// validateUpdateMethod validates the rollout settings that depend on rolloutStrategy.updateMethod.
func validateUpdateMethod(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	strategy := ms.Spec.RolloutStrategy
	if strategy == nil {
		return nil
	}
	var allErrs field.ErrorList
	strategyPath := field.NewPath("spec", "rolloutStrategy")
	if updateMethodOf(ms) != workloadv1alpha1.InPlaceUpdateMethod {
		if strategy.ProgressDeadlineSeconds != nil {
			allErrs = append(allErrs, field.Forbidden(strategyPath.Child("progressDeadlineSeconds"),
				"progressDeadlineSeconds is only valid when rolloutStrategy.updateMethod is InPlace"))
		}
		return allErrs
	}

	// The in-place reconciler is not implemented yet, so the remaining checks only explain what else must change.
	allErrs = append(allErrs, field.NotSupported(strategyPath.Child("updateMethod"), strategy.UpdateMethod,
		[]string{string(workloadv1alpha1.RecreateUpdateMethod)}))
	if strategy.Type == workloadv1alpha1.RoleRollingUpdate {
		allErrs = append(allErrs, field.Forbidden(strategyPath.Child("type"),
			"RoleRollingUpdate cannot be combined with rolloutStrategy.updateMethod InPlace"))
	}
	allErrs = append(allErrs, validateInPlaceBudget(ms)...)
	if len(ms.Spec.Plugins) > 0 {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "plugins"),
			"plugins cannot be used with rolloutStrategy.updateMethod InPlace"))
	}
	rolesPath := field.NewPath("spec", "template", "roles")
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		allErrs = append(allErrs, validateInPlaceTemplate(&role.EntryTemplate, rolesPath.Index(i).Child("entryTemplate"))...)
		if role.WorkerTemplate != nil {
			allErrs = append(allErrs, validateInPlaceTemplate(role.WorkerTemplate, rolesPath.Index(i).Child("workerTemplate"))...)
		}
	}
	return allErrs
}

// validateInPlaceBudget requires one unit to be taken out of service at a time without surging.
// Unlike Recreate, maxUnavailable may not resolve to 0 even when maxSurge is positive.
func validateInPlaceBudget(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	config := ms.Spec.RolloutStrategy.RollingUpdateConfiguration
	if config == nil || ms.Spec.Replicas == nil {
		return nil
	}
	var allErrs field.ErrorList
	replicas := int(*ms.Spec.Replicas)
	configPath := field.NewPath("spec", "rolloutStrategy", "rollingUpdateConfiguration")
	if config.MaxSurge != nil {
		maxSurge, err := intstr.GetScaledValueFromIntOrPercent(config.MaxSurge, replicas, true)
		if err == nil && maxSurge != 0 {
			allErrs = append(allErrs, field.Invalid(configPath.Child("maxSurge"), config.MaxSurge,
				"maxSurge must resolve to 0 when rolloutStrategy.updateMethod is InPlace"))
		}
	}
	if config.MaxUnavailable != nil && replicas > 0 {
		maxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(config.MaxUnavailable, replicas, false)
		if err == nil && maxUnavailable < 1 {
			allErrs = append(allErrs, field.Invalid(configPath.Child("maxUnavailable"), config.MaxUnavailable,
				fmt.Sprintf("maxUnavailable must resolve to at least 1 of %d replicas when rolloutStrategy.updateMethod is InPlace", replicas)))
		}
	}
	return allErrs
}

// validateInPlaceTemplate rejects restart settings under which a container would not restart on the patched image.
func validateInPlaceTemplate(template *workloadv1alpha1.PodTemplateSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	resolved := inplace.ResolveTemplate(template)
	if resolved.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("spec", "restartPolicy"), resolved.Spec.RestartPolicy,
			[]string{string(corev1.RestartPolicyAlways)}))
	}
	containersPath := fldPath.Child("spec", "containers")
	for i, container := range resolved.Spec.Containers {
		if container.RestartPolicy != nil || len(container.RestartPolicyRules) > 0 {
			allErrs = append(allErrs, field.Forbidden(containersPath.Index(i),
				"container restartPolicy and restartPolicyRules cannot be set when rolloutStrategy.updateMethod is InPlace"))
		}
	}
	return allErrs
}

// validateInPlaceUpdate checks an update that leaves rolloutStrategy.updateMethod as InPlace.
func validateInPlaceUpdate(oldMS, ms *workloadv1alpha1.ModelServing) field.ErrorList {
	if updateMethodOf(ms) != workloadv1alpha1.InPlaceUpdateMethod {
		return nil
	}
	var allErrs field.ErrorList
	if updateMethodOf(oldMS) != workloadv1alpha1.InPlaceUpdateMethod {
		allErrs = append(allErrs, validateRolloutComplete(oldMS)...)
	}
	return append(allErrs, inplace.CheckEligibility(oldMS, ms)...)
}

// validateRolloutComplete requires every ServingGroup to be on one revision before InPlace takes effect.
// Otherwise some units would start from an older revision whose differences may not be image-only.
func validateRolloutComplete(oldMS *workloadv1alpha1.ModelServing) field.ErrorList {
	status := &oldMS.Status
	replicas := replicasOrDefault(oldMS.Spec.Replicas)
	if status.ObservedGeneration == oldMS.Generation &&
		status.CurrentRevision == status.UpdateRevision &&
		status.UpdatedReplicas == status.Replicas &&
		status.Replicas == replicas {
		return nil
	}
	return field.ErrorList{field.Forbidden(field.NewPath("spec", "rolloutStrategy", "updateMethod"),
		fmt.Sprintf("switching to InPlace requires the previous rollout to be complete: observedGeneration=%d generation=%d currentRevision=%q updateRevision=%q updatedReplicas=%d replicas=%d spec.replicas=%d",
			status.ObservedGeneration, oldMS.Generation, status.CurrentRevision, status.UpdateRevision, status.UpdatedReplicas, status.Replicas, replicas))}
}
