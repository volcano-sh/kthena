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
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// ModelServingValidator handles validation of ModelServing resources.
type ModelServingValidator struct {
}

func NewModelServingValidator() *ModelServingValidator {
	return &ModelServingValidator{}
}

// Handle handles admission requests for ModelServing resources
func (v *ModelServingValidator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, modelServing, err := utils.ParseModelServingFromRequest(r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// UPDATE must compare against the existing object. Do not turn a missing or
	// malformed oldObject into CREATE validation and bypass plugin immutability.
	oldModelServing, err := parseOldModelServingForUpdate(admissionReview)
	var allowed bool
	var reason string
	if err != nil {
		reason = err.Error()
	} else if oldModelServing != nil {
		allowed, reason = v.validateModelServingUpdate(oldModelServing, modelServing)
	} else {
		allowed, reason = v.validateModelServing(modelServing)
	}

	// Create the admission response
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}

	if !allowed {
		admissionResponse.Result = &metav1.Status{
			Message: reason,
		}
	}

	// Create the admission review response
	admissionReview.Response = &admissionResponse

	// Send the response
	if err := utils.SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
		return
	}
}

func parseOldModelServingForUpdate(admissionReview *admissionv1.AdmissionReview) (*workloadv1alpha1.ModelServing, error) {
	if admissionReview.Request.Operation != admissionv1.Update {
		return nil, nil
	}
	if len(admissionReview.Request.OldObject.Raw) == 0 {
		return nil, fmt.Errorf("old ModelServing is required for update validation")
	}
	var oldModelServing *workloadv1alpha1.ModelServing
	if err := json.Unmarshal(admissionReview.Request.OldObject.Raw, &oldModelServing); err != nil {
		return nil, fmt.Errorf("failed to decode old ModelServing: %w", err)
	}
	if oldModelServing == nil || oldModelServing.Name == "" {
		return nil, fmt.Errorf("old ModelServing with metadata.name is required for update validation")
	}
	return oldModelServing, nil
}

// validateModelServing validates the ModelServing resource
func (v *ModelServingValidator) validateModelServing(modelServing *workloadv1alpha1.ModelServing) (bool, string) {
	allErrs := modelServingValidationErrors(modelServing)
	allErrs = append(allErrs, validateCoordinatedPartitions(nil, modelServing)...)
	return validationResult(allErrs)
}

func (v *ModelServingValidator) validateModelServingUpdate(
	oldModelServing, modelServing *workloadv1alpha1.ModelServing,
) (bool, string) {
	allErrs := modelServingValidationErrors(modelServing)
	allErrs = append(allErrs, validatePluginsImmutable(oldModelServing, modelServing)...)
	allErrs = append(allErrs, validateSchedulerNameUpdate(oldModelServing, modelServing)...)
	allErrs = append(allErrs, apivalidation.ValidateImmutableField(roleNameSet(modelServing), roleNameSet(oldModelServing), field.NewPath("spec", "template", "roles"))...)
	allErrs = append(allErrs, validateRoleCoordinationUpdate(oldModelServing, modelServing)...)
	allErrs = append(allErrs, validateRoleCoordinationImmutable(oldModelServing, modelServing)...)
	allErrs = append(allErrs, validateCoordinatedPartitions(oldModelServing, modelServing)...)
	return validationResult(allErrs)
}

func modelServingValidationErrors(modelServing *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validGeneratedNameLength(modelServing)...)
	allErrs = append(allErrs, validateRoleNames(modelServing)...)
	allErrs = append(allErrs, validateWorkerImages(modelServing)...)
	allErrs = append(allErrs, validatePodTemplates(modelServing)...)
	allErrs = append(allErrs, validatorReplicas(modelServing)...)
	allErrs = append(allErrs, validateRollingUpdateConfiguration(modelServing)...)
	allErrs = append(allErrs, validateMaxUnavailableForRoles(modelServing)...)
	allErrs = append(allErrs, validateRoleCoordination(modelServing)...)
	allErrs = append(allErrs, validateGangPolicy(modelServing)...)
	allErrs = append(allErrs, validateWorkerReplicas(modelServing)...)
	allErrs = append(allErrs, validateRecoveryPolicyAndRolloutStrategy(modelServing)...)
	allErrs = append(allErrs, validateEvictionStrategy(modelServing)...)

	return allErrs
}

func validationResult(allErrs field.ErrorList) (bool, string) {
	if len(allErrs) > 0 {
		var messages []string
		for _, err := range allErrs {
			messages = append(messages, fmt.Sprintf("  - %s", err.Error()))
		}
		return false, fmt.Sprintf("validation failed:\n%s", strings.Join(messages, "\n"))
	}
	return true, ""
}

// validNameLength validates the resource name generated by modelServing.
func validGeneratedNameLength(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	replicas := replicasOrDefault(ms.Spec.Replicas)
	for _, role := range ms.Spec.Template.Roles {
		roleReplicas := replicasOrDefault(role.Replicas)
		name := ms.GetName() + "-" + strconv.Itoa(int(replicas)) + "-" + role.Name + "-" + strconv.Itoa(int(roleReplicas)) + "-" + strconv.Itoa(int(role.WorkerReplicas))
		errors := apivalidation.NameIsDNS1035Label(name, false)
		if len(errors) > 0 {
			klog.Errorf("name generated by modelServing is illegal, please change ms.Name or role.Name")
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("metadata").Child("name"),
				ms.GetName(),
				fmt.Sprintf("invalid name: %s", strings.Join(errors, "; ")),
			))
		}
	}

	return allErrs
}

func replicasOrDefault(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// validateRoleNames validates that role names conform to DNS-1035 label format.
func validateRoleNames(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	for i, role := range ms.Spec.Template.Roles {
		errors := apivalidation.NameIsDNS1035Label(role.Name, false)
		if len(errors) > 0 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("name"),
				role.Name,
				fmt.Sprintf("role name must be a valid DNS-1035 label (lowercase alphanumeric characters or '-', must start with a letter): %s", strings.Join(errors, "; ")),
			))
		}
	}
	return allErrs
}

// validateRollingUpdateConfiguration validates the ServingGroup-level rolling
// update configuration. It is allowed but ignored for RoleRollingUpdate.
func validateRollingUpdateConfiguration(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		return nil
	}
	return validateRolloutBudget(*ms.Spec.RolloutStrategy.RollingUpdateConfiguration, replicasOrDefault(ms.Spec.Replicas),
		ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate, field.NewPath("spec", "rolloutStrategy", "rollingUpdateConfiguration"))
}

func validateMaxUnavailableForRoles(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	roleRollingUpdate := ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate
	for i, role := range ms.Spec.Template.Roles {
		allErrs = append(allErrs, validateRolloutBudget(role.RollingUpdateConfiguration, replicasOrDefault(role.Replicas),
			roleRollingUpdate, field.NewPath("spec", "template", "roles").Index(i))...)
	}
	return allErrs
}

// validateRolloutBudget applies the same rules to both rollout granularities.
// Settings of the inactive granularity, and of zero replicas, only need valid
// syntax because they do not bound any rollout.
func validateRolloutBudget(config workloadv1alpha1.RollingUpdateConfiguration, replicas int32, active bool, path *field.Path) field.ErrorList {
	maxUnavailablePath, maxSurgePath, partitionPath := path.Child("maxUnavailable"), path.Child("maxSurge"), path.Child("partition")
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateIntOrPercent(config.MaxUnavailable, maxUnavailablePath)...)
	allErrs = append(allErrs, validateIntOrPercentRange(config.MaxSurge, maxSurgePath, false)...)
	allErrs = append(allErrs, validateIntOrPercent(config.Partition, partitionPath)...)
	if len(allErrs) > 0 || !active || replicas <= 0 {
		return allErrs
	}

	// Percentages are bounded by 100%, so only integers can exceed replicas.
	allErrs = append(allErrs, validateIntegerReplicaLimit(config.MaxUnavailable, replicas, path, "maxUnavailable")...)
	allErrs = append(allErrs, validateIntegerReplicaLimit(config.Partition, replicas, path, "partition")...)
	maxSurge := ptr.Deref(config.MaxSurge, intstr.FromInt32(0))
	if !surgeFitsInt32(maxSurge, replicas) {
		return append(allErrs, field.Invalid(maxSurgePath, config.MaxSurge, "replicas + maxSurge cannot exceed 2147483647"))
	}
	maxUnavailable := ptr.Deref(config.MaxUnavailable, intstr.FromInt32(1))
	unavailable, _ := intstr.GetScaledValueFromIntOrPercent(&maxUnavailable, int(replicas), false)
	surge, _ := intstr.GetScaledValueFromIntOrPercent(&maxSurge, int(replicas), true)
	if unavailable == 0 && surge == 0 {
		allErrs = append(allErrs, field.Invalid(path, "", "maxUnavailable and maxSurge cannot both resolve to 0"))
	}
	return allErrs
}

func validateIntegerReplicaLimit(value *intstr.IntOrString, replicas int32, path *field.Path, name string) field.ErrorList {
	if value != nil && value.Type == intstr.Int && value.IntVal > replicas {
		return field.ErrorList{field.Invalid(path.Child(name), value, fmt.Sprintf("%s cannot be greater than replicas (%d)", name, replicas))}
	}
	return nil
}

// surgeFitsInt32 reports whether replicas plus the resolved maxSurge fits in
// int32, the type of replica counts. It requires positive replicas.
func surgeFitsInt32(maxSurge intstr.IntOrString, replicas int32) bool {
	limit := int64(math.MaxInt32 - replicas)
	if maxSurge.Type == intstr.Int {
		return int64(maxSurge.IntVal) <= limit
	}
	// Compare before scaling: percent * replicas can overflow.
	percent, _ := strconv.ParseInt(strings.TrimSuffix(maxSurge.StrVal, "%"), 10, 64)
	return percent <= limit*100/int64(replicas)
}

func validatorReplicas(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas < 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("replicas"),
			ms.Spec.Replicas,
			"replicas must be a non-negative integer",
		))
	}

	if len(ms.Spec.Template.Roles) == 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("template").Child("roles"),
			ms.Spec.Template.Roles,
			"roles must be specified",
		))
		return allErrs
	}

	for i, role := range ms.Spec.Template.Roles {
		if role.Replicas == nil || *role.Replicas < 0 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("replicas"),
				role.Replicas,
				"role replicas must be a non-negative integer",
			))
		}
	}
	return allErrs
}

// validateGangPolicy validates the gang scheduling configuration
func validateGangPolicy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList

	if ms.Spec.Template.GangPolicy == nil || ms.Spec.Template.GangPolicy.MinRoleReplicas == nil {
		return allErrs
	}

	minRoleReplicas := ms.Spec.Template.GangPolicy.MinRoleReplicas
	minRoleReplicasPath := field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas")

	// Create a map of role names for quick lookup
	roleMap := make(map[string]workloadv1alpha1.Role)
	for _, role := range ms.Spec.Template.Roles {
		roleMap[role.Name] = role
	}

	// Validate each minRoleReplicas entry
	for roleName, minReplicas := range minRoleReplicas {
		// Check if the role exists
		roleElement, ok := roleMap[roleName]
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				minRoleReplicasPath.Key(roleName),
				roleName,
				fmt.Sprintf("role %s does not exist in template.roles", roleName),
			))
			continue
		}

		// Find the role in the roleMap then check its actual replicas
		// Calculate total replicas for this role
		// minRoleReplicas is compared against the number of Role replicas
		replicas := int32(1)
		if roleElement.Replicas != nil {
			replicas = *roleElement.Replicas
		}

		// Validate minReplicas doesn't exceed total replicas
		if minReplicas > replicas {
			allErrs = append(allErrs, field.Invalid(
				minRoleReplicasPath.Key(roleName),
				minReplicas,
				fmt.Sprintf("minRoleReplicas (%d) for role %s cannot exceed replicas (%d)", minReplicas, roleName, replicas),
			))
		}

		// Validate minReplicas is non-negative
		if minReplicas < 0 {
			allErrs = append(allErrs, field.Invalid(
				minRoleReplicasPath.Key(roleName),
				minReplicas,
				fmt.Sprintf("minRoleReplicas for role %s must be non-negative", roleName),
			))
		}
	}

	return allErrs
}

// validateWorkerReplicas validates worker replicas in roles
func validateWorkerReplicas(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList

	for i, role := range ms.Spec.Template.Roles {
		// WorkerReplicas must be non-negative
		if role.WorkerReplicas < 0 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("workerReplicas"),
				role.WorkerReplicas,
				"workerReplicas must be a non-negative integer",
			))
		}
		if role.WorkerReplicas > 0 && role.WorkerTemplate == nil {
			allErrs = append(allErrs, field.Required(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("workerTemplate"),
				"workerTemplate is required when workerReplicas is greater than 0",
			))
		}
	}
	return allErrs
}

func validateIntOrPercent(value *intstr.IntOrString, fieldPath *field.Path) field.ErrorList {
	return validateIntOrPercentRange(value, fieldPath, true)
}

// validateIntOrPercentRange validates a non-negative integer or percentage.
// boundedPercent limits percentages to 100%.
func validateIntOrPercentRange(value *intstr.IntOrString, fieldPath *field.Path, boundedPercent bool) field.ErrorList {
	allErrs := field.ErrorList{}
	if value == nil {
		return allErrs
	}
	switch value.Type {
	case intstr.String:
		for _, msg := range validation.IsValidPercent(value.StrVal) {
			allErrs = append(allErrs, field.Invalid(fieldPath, value, msg))
		}
		if len(allErrs) > 0 {
			break
		}
		// Strip trailing '%' and parse; IsValidPercent already ensures the format is valid.
		percentStr := strings.TrimSuffix(value.StrVal, "%")
		percent, err := strconv.Atoi(percentStr)
		if err != nil || percent < 0 || (boundedPercent && percent > 100) {
			message := "must be a valid percent value (0-100)"
			if !boundedPercent {
				message = "must be a non-negative integer percent"
			}
			allErrs = append(allErrs, field.Invalid(fieldPath, value, message))
		}
	case intstr.Int:
		allErrs = append(allErrs, validateNonnegativeField(int64(value.IntValue()), fieldPath)...)
	default:
		allErrs = append(allErrs, field.Invalid(fieldPath, value, "must be an int or percent"))
	}
	return allErrs
}

func validateNonnegativeField(value int64, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value, "must be a non-negative integer"))
	}
	return allErrs
}

// validateWorkerImages validates the image of entryPod and workerPod.
func validateWorkerImages(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	for i, role := range ms.Spec.Template.Roles {
		// validate entryPod image
		for j, container := range role.EntryTemplate.Spec.Containers {
			if container.Image != "" {
				if err := validateImageField(container.Image); err != nil {
					allErrs = append(allErrs, field.Invalid(
						field.NewPath("spec").Child("template").Child("roles").Index(i).Child("entryTemplate").Child("spec").Child("containers").Index(j).Child("image"),
						container.Image,
						fmt.Sprintf("invalid container image reference: %v", err),
					))
				}
			}
		}

		// validate workerPods image
		if role.WorkerTemplate != nil {
			for j, container := range role.WorkerTemplate.Spec.Containers {
				if container.Image != "" {
					if err := validateImageField(container.Image); err != nil {
						allErrs = append(allErrs, field.Invalid(
							field.NewPath("spec").Child("template").Child("roles").Index(i).Child("workerTemplate").Child("spec").Child("containers").Index(j).Child("image"),
							container.Image,
							fmt.Sprintf("invalid container image reference: %v", err),
						))
					}
				}
			}
		}
	}
	return allErrs
}

// validateImageField checks if a container image string is a valid Docker reference.
func validateImageField(image string) error {
	if image == "" {
		// Optional: return the error if you want to require the image field
		return nil
	}

	// Simple validation: check if image contains at least one character and no spaces
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image cannot be empty or whitespace only")
	}

	if strings.Contains(image, " ") {
		return fmt.Errorf("image cannot contain spaces")
	}

	return nil
}

func validateRecoveryPolicyAndRolloutStrategy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	// Effective defaults:
	// - recoveryPolicy: RoleRecreate
	// - rolloutStrategy.type: ServingGroupRollingUpdate
	// Required one-to-one mapping not be:
	// - ServingGroupRecreate <-> roleRollingUpdate
	effectiveRecoveryPolicy := ms.Spec.RecoveryPolicy
	if effectiveRecoveryPolicy == "" {
		effectiveRecoveryPolicy = workloadv1alpha1.RoleRecreate
	}

	effectiveRolloutType := workloadv1alpha1.ServingGroupRollingUpdate
	if ms.Spec.RolloutStrategy != nil {
		effectiveRolloutType = ms.Spec.RolloutStrategy.Type
		if effectiveRolloutType == "" {
			effectiveRolloutType = workloadv1alpha1.ServingGroupRollingUpdate
		}
	}

	unMatched := (effectiveRecoveryPolicy == workloadv1alpha1.ServingGroupRecreate && effectiveRolloutType == workloadv1alpha1.RoleRollingUpdate)

	if unMatched {
		// Point to the explicitly specified field when possible.
		errPath := field.NewPath("spec").Child("rolloutStrategy").Child("type")
		errValue := any(effectiveRolloutType)
		if ms.Spec.RolloutStrategy == nil {
			errPath = field.NewPath("spec").Child("recoveryPolicy")
			errValue = effectiveRecoveryPolicy
		}

		allErrs = append(allErrs, field.Invalid(
			errPath,
			errValue,
			fmt.Sprintf(
				"incompatible recoveryPolicy and rolloutStrategy.type after applying defaults: recoveryPolicy=%s, rolloutStrategy.type=%s; valid pairs: (%s,%s) or (%s,%s)",
				effectiveRecoveryPolicy,
				effectiveRolloutType,
				workloadv1alpha1.ServingGroupRecreate,
				workloadv1alpha1.ServingGroupRollingUpdate,
				workloadv1alpha1.RoleRecreate,
				workloadv1alpha1.RoleRollingUpdate,
			),
		))
	}

	return allErrs
}

func validateEvictionStrategy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.EvictionStrategy == nil {
		return allErrs
	}

	strategy := ms.Spec.RolloutStrategy.EvictionStrategy
	fldPath := field.NewPath("spec").Child("rolloutStrategy").Child("evictionStrategy")

	protectionLevel := strategy.ProtectionLevel
	if protectionLevel == "" {
		protectionLevel = workloadv1alpha1.ProtectionLevelServingGroup
	}

	switch protectionLevel {
	case workloadv1alpha1.ProtectionLevelServingGroup:
		allErrs = append(allErrs, validateServingGroupMinAvailable(ms, strategy.MinAvailable, fldPath.Child("minAvailable"))...)
	case workloadv1alpha1.ProtectionLevelRole:
		allErrs = append(allErrs, validateRoleMinAvailable(ms, strategy.RoleMinAvailable, fldPath.Child("roleMinAvailable"))...)
	default:
		allErrs = append(allErrs, field.NotSupported(
			fldPath.Child("protectionLevel"),
			strategy.ProtectionLevel,
			[]string{string(workloadv1alpha1.ProtectionLevelServingGroup), string(workloadv1alpha1.ProtectionLevelRole)},
		))
	}

	return allErrs
}

func validateServingGroupMinAvailable(ms *workloadv1alpha1.ModelServing, minAvailable *intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if minAvailable == nil {
		return append(allErrs, field.Required(fldPath, "minAvailable is required when evictionStrategy.protectionLevel is ServingGroup"))
	}

	allErrs = append(allErrs, validateIntOrPercent(minAvailable, fldPath)...)
	if len(allErrs) > 0 || ms.Spec.Replicas == nil || *ms.Spec.Replicas < 0 {
		return allErrs
	}

	totalReplicas := int(*ms.Spec.Replicas)
	value, err := intstr.GetScaledValueFromIntOrPercent(minAvailable, totalReplicas, true)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, minAvailable, fmt.Sprintf("invalid minAvailable: %v", err)))
	} else if value > totalReplicas {
		allErrs = append(allErrs, field.Invalid(fldPath, minAvailable, fmt.Sprintf("minAvailable (%d) cannot exceed replicas (%d)", value, totalReplicas)))
	}

	return allErrs
}

func validateRoleMinAvailable(ms *workloadv1alpha1.ModelServing, roleMinAvailable map[string]intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(roleMinAvailable) == 0 {
		return append(allErrs, field.Required(fldPath, "roleMinAvailable is required when evictionStrategy.protectionLevel is Role"))
	}

	roleReplicas := make(map[string]int, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		if role.Replicas == nil || *role.Replicas < 0 {
			continue
		}
		roleReplicas[role.Name] = int(*role.Replicas)
	}

	for roleName, minAvailable := range roleMinAvailable {
		rolePath := fldPath.Key(roleName)
		replicas, ok := roleReplicas[roleName]
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				rolePath,
				roleName,
				fmt.Sprintf("role %s does not exist in template.roles", roleName),
			))
			continue
		}

		before := len(allErrs)
		allErrs = append(allErrs, validateIntOrPercent(&minAvailable, rolePath)...)
		if len(allErrs) > before {
			continue
		}

		value, err := intstr.GetScaledValueFromIntOrPercent(&minAvailable, replicas, true)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(rolePath, minAvailable, fmt.Sprintf("invalid roleMinAvailable: %v", err)))
		} else if value > replicas {
			allErrs = append(allErrs, field.Invalid(rolePath, minAvailable, fmt.Sprintf("roleMinAvailable (%d) for role %s cannot exceed replicas (%d)", value, roleName, replicas)))
		}
	}

	return allErrs
}

func roleNameSet(ms *workloadv1alpha1.ModelServing) []string {
	names := make([]string, 0, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		names = append(names, role.Name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// Changing scheduler requires migrating the shared PodGroup as well as Pods.
// Keep the effective scheduler immutable; explicit empty selects default-scheduler.
func validateSchedulerNameUpdate(old, current *workloadv1alpha1.ModelServing) field.ErrorList {
	return apivalidation.ValidateImmutableField(utils.EffectiveSchedulerName(current.Spec.SchedulerName),
		utils.EffectiveSchedulerName(old.Spec.SchedulerName), field.NewPath("spec", "schedulerName"))
}
