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
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

// validateBootstrapAccelerateStrategy validates spec.bootstrapAccelerateStrategy
// and rejects explicit registration of the managed ModelExpress plugin.
func validateBootstrapAccelerateStrategy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	for i, plugin := range ms.Spec.Plugins {
		if plugin.Name == plugins.ModelExpressPluginName {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "plugins").Index(i).Child("name"),
				fmt.Sprintf("plugin %s is managed by spec.bootstrapAccelerateStrategy and cannot be listed", plugins.ModelExpressPluginName)))
		}
	}

	strategy := ms.Spec.BootstrapAccelerateStrategy
	if strategy == nil {
		return allErrs
	}
	fldPath := field.NewPath("spec", "bootstrapAccelerateStrategy")

	if strategy.Provider != "" && strategy.Provider != workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("provider"), strategy.Provider,
			[]string{string(workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress)}))
	}

	templateRoles := make(map[string]workloadv1alpha1.Role, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		templateRoles[role.Name] = role
	}

	rolesPath := fldPath.Child("roles")
	if len(strategy.Roles) == 0 {
		allErrs = append(allErrs, field.Required(rolesPath, "at least one role must be specified"))
	}
	seen := make(map[string]struct{}, len(strategy.Roles))
	var scopedRoles []workloadv1alpha1.Role
	for i, role := range strategy.Roles {
		rolePath := rolesPath.Index(i)
		if role.Name == "" {
			allErrs = append(allErrs, field.Required(rolePath.Child("name"), "role name must not be empty"))
			continue
		}
		if _, duplicate := seen[role.Name]; duplicate {
			allErrs = append(allErrs, field.Duplicate(rolePath.Child("name"), role.Name))
			continue
		}
		seen[role.Name] = struct{}{}
		templateRole, exists := templateRoles[role.Name]
		if !exists {
			allErrs = append(allErrs, field.Invalid(rolePath.Child("name"), role.Name,
				fmt.Sprintf("role %s does not exist in spec.template.roles", role.Name)))
			continue
		}
		scopedRoles = append(scopedRoles, templateRole)
		allErrs = append(allErrs, validateInitialSeedReplicas(role.InitialSeedReplicas, rolePath.Child("initialSeedReplicas"))...)
	}

	if strategy.ModelExpress != nil {
		allErrs = append(allErrs, validateModelExpressConfig(strategy.ModelExpress, scopedRoles, fldPath.Child("modelExpress"))...)
	}
	return allErrs
}

// validateInitialSeedReplicas accepts a positive integer or a percentage from 1% to 100%.
func validateInitialSeedReplicas(value *intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if value == nil {
		return allErrs
	}
	switch value.Type {
	case intstr.Int:
		if value.IntValue() < 1 {
			allErrs = append(allErrs, field.Invalid(fldPath, value, "must be a positive integer"))
		}
	case intstr.String:
		if errs := validateIntOrPercent(value, fldPath); len(errs) > 0 {
			return append(allErrs, errs...)
		}
		percent, err := strconv.Atoi(strings.TrimSuffix(value.StrVal, "%"))
		if err != nil || percent < 1 || percent > 100 {
			allErrs = append(allErrs, field.Invalid(fldPath, value, "percentage must be between 1% and 100%"))
		}
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, value, "must be an int or percent"))
	}
	return allErrs
}

func validateModelExpressConfig(config *workloadv1alpha1.ModelExpressConfig, scopedRoles []workloadv1alpha1.Role, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	containersPath := fldPath.Child("engineContainers")
	if len(config.EngineContainers) == 0 {
		allErrs = append(allErrs, field.Required(containersPath, "at least one engine container must be specified"))
	}
	engineContainers := make(map[string]struct{}, len(config.EngineContainers))
	for i, name := range config.EngineContainers {
		if name == "" {
			allErrs = append(allErrs, field.Required(containersPath.Index(i), "container name must not be empty"))
			continue
		}
		if _, duplicate := engineContainers[name]; duplicate {
			allErrs = append(allErrs, field.Duplicate(containersPath.Index(i), name))
			continue
		}
		engineContainers[name] = struct{}{}
	}

	if len(engineContainers) > 0 {
		for _, role := range scopedRoles {
			rolePath := field.NewPath("spec", "template", "roles").Key(role.Name)
			if !hasEngineContainer(role.EntryTemplate.Spec.Containers, engineContainers) {
				allErrs = append(allErrs, field.Invalid(rolePath.Child("entryTemplate", "spec", "containers"), containerNames(role.EntryTemplate.Spec.Containers),
					fmt.Sprintf("must contain one of the ModelExpress engine containers %v", config.EngineContainers)))
			}
			if role.WorkerTemplate != nil && !hasEngineContainer(role.WorkerTemplate.Spec.Containers, engineContainers) {
				allErrs = append(allErrs, field.Invalid(rolePath.Child("workerTemplate", "spec", "containers"), containerNames(role.WorkerTemplate.Spec.Containers),
					fmt.Sprintf("must contain one of the ModelExpress engine containers %v", config.EngineContainers)))
			}
		}
	}

	if config.ReadyURL != "" {
		parsed, err := url.Parse(config.ReadyURL)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("readyURL"), config.ReadyURL, "must be an absolute http or https URL"))
		}
	}
	return allErrs
}

func hasEngineContainer(containers []corev1.Container, engineContainers map[string]struct{}) bool {
	for _, container := range containers {
		if _, ok := engineContainers[container.Name]; ok {
			return true
		}
	}
	return false
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		names = append(names, container.Name)
	}
	return names
}
