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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	podutil "k8s.io/kubernetes/pkg/api/pod"
	core "k8s.io/kubernetes/pkg/apis/core"
	coreconversion "k8s.io/kubernetes/pkg/apis/core/v1"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func validatePodTemplates(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	rolesPath := field.NewPath("spec", "template", "roles")
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		allErrs = append(allErrs, validatePodTemplate(&role.EntryTemplate, rolesPath.Index(i).Child("entryTemplate"))...)
		if role.WorkerTemplate != nil {
			allErrs = append(allErrs, validatePodTemplate(role.WorkerTemplate, rolesPath.Index(i).Child("workerTemplate"))...)
		}
	}
	return allErrs
}

func validatePodTemplate(template *workloadv1alpha1.PodTemplateSpec, fldPath *field.Path) field.ErrorList {
	// Embedded PodSpecs in CRDs do not receive native Pod defaulting. Default a
	// private copy recursively, including containers, probes and volumes, so
	// omitted defaults are accepted without mutating the submitted ModelServing.
	copy := template.DeepCopy()
	podTemplate := &corev1.PodTemplate{
		Template: corev1.PodTemplateSpec{Spec: copy.Spec},
	}
	if copy.Metadata != nil {
		podTemplate.Template.Labels = copy.Metadata.Labels
		podTemplate.Template.Annotations = copy.Metadata.Annotations
	}
	coreconversion.SetObjectDefaults_PodTemplate(podTemplate)

	internalTemplate := &core.PodTemplateSpec{}
	if err := coreconversion.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(&podTemplate.Template, internalTemplate, nil); err != nil {
		return field.ErrorList{field.InternalError(fldPath, err)}
	}
	// Use this Kubernetes version's default feature gates, not zero-valued
	// options, which would reject some features enabled by default upstream.
	// The target API server remains authoritative for cluster-specific admission.
	opts := podutil.GetValidationOptionsFromPodTemplate(internalTemplate, nil)
	allErrs := corevalidation.ValidatePodTemplateSpec(internalTemplate, fldPath, opts)
	// Native template validation reports labels/annotations directly below the
	// template; our API exposes these fields under metadata.
	for _, err := range allErrs {
		for _, name := range []string{"labels", "annotations"} {
			prefix := fldPath.Child(name).String()
			if err.Field == prefix || strings.HasPrefix(err.Field, prefix+"[") || strings.HasPrefix(err.Field, prefix+".") {
				err.Field = fldPath.Child("metadata", name).String() + err.Field[len(prefix):]
			}
		}
	}
	return allErrs
}
