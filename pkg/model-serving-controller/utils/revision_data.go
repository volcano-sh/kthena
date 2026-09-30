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

package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"slices"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const defaultSchedulerName = "volcano"

type modelServingRevisionPatch struct {
	Spec modelServingRevisionSpec `json:"spec"`
}

type modelServingRevisionSpec struct {
	SchedulerName string                        `json:"schedulerName"`
	Plugins       []workloadv1alpha1.PluginSpec `json:"plugins"`
	Template      modelServingRevisionTemplate  `json:"template"`
}

type modelServingRevisionTemplate struct {
	Roles []modelServingRevisionRole `json:"roles"`
}

type modelServingRevisionRole struct {
	Name           string                            `json:"name"`
	EntryTemplate  workloadv1alpha1.PodTemplateSpec  `json:"entryTemplate"`
	WorkerReplicas int32                             `json:"workerReplicas"`
	WorkerTemplate *workloadv1alpha1.PodTemplateSpec `json:"workerTemplate,omitempty"`
}

type modelServingRoleRevisionProjection struct {
	SchedulerName string                        `json:"schedulerName"`
	Plugins       []workloadv1alpha1.PluginSpec `json:"plugins"`
	Role          modelServingRevisionRole      `json:"role"`
}

// BuildRevisionData returns the canonical spec patch used as ControllerRevision
// data and the primary revision hash input.
func BuildRevisionData(ms *workloadv1alpha1.ModelServing) ([]byte, error) {
	if ms == nil {
		return nil, fmt.Errorf("model serving is nil")
	}

	patch, err := buildRevisionPatch(ms)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshal model serving revision data: %w", err)
	}
	return data, nil
}

// BuildRoleRevisionData returns the canonical revision projection applicable to
// one Role. It is derived from the same normalized data as BuildRevisionData,
// preserving plugin order and the complete PluginSpec for each applicable hook.
func BuildRoleRevisionData(ms *workloadv1alpha1.ModelServing, roleName string) ([]byte, error) {
	patch, err := buildRevisionPatch(ms)
	if err != nil {
		return nil, err
	}
	return buildRoleRevisionDataFromPatch(patch, roleName)
}

func buildRoleRevisionDataFromPatch(patch *modelServingRevisionPatch, roleName string) ([]byte, error) {
	for _, role := range patch.Spec.Template.Roles {
		if role.Name != roleName {
			continue
		}
		plugins := make([]workloadv1alpha1.PluginSpec, 0, len(patch.Spec.Plugins))
		for _, plugin := range patch.Spec.Plugins {
			if pluginAppliesToRole(plugin, role) {
				plugins = append(plugins, plugin)
			}
		}
		projection := modelServingRoleRevisionProjection{
			SchedulerName: patch.Spec.SchedulerName,
			Plugins:       plugins,
			Role:          role,
		}
		data, err := json.Marshal(projection)
		if err != nil {
			return nil, fmt.Errorf("marshal Role %q revision data: %w", roleName, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("role %q not found", roleName)
}

// RoleRevisionHash returns the hash of the canonical revision inputs that
// apply to one Role.
func RoleRevisionHash(ms *workloadv1alpha1.ModelServing, roleName string) (string, error) {
	data, err := BuildRoleRevisionData(ms, roleName)
	if err != nil {
		return "", err
	}
	return RevisionDataHash(data, nil), nil
}

// RoleRevisionHashFromRevisionData derives the same Role identity directly
// from canonical ControllerRevision data without reconstructing a workload.
func RoleRevisionHashFromRevisionData(data []byte, roleName string) (string, error) {
	patch, err := decodeRevisionPatch(data)
	if err != nil {
		return "", err
	}
	roleData, err := buildRoleRevisionDataFromPatch(patch, roleName)
	if err != nil {
		return "", err
	}
	return RevisionDataHash(roleData, nil), nil
}

func pluginAppliesToRole(plugin workloadv1alpha1.PluginSpec, role modelServingRevisionRole) bool {
	if plugin.Scope == nil {
		return true
	}
	if len(plugin.Scope.Roles) > 0 && !slices.Contains(plugin.Scope.Roles, role.Name) {
		return false
	}
	return plugin.Scope.Target != workloadv1alpha1.PluginTargetWorker || role.WorkerReplicas > 0
}

func buildRevisionPatch(ms *workloadv1alpha1.ModelServing) (*modelServingRevisionPatch, error) {
	if ms == nil {
		return nil, fmt.Errorf("model serving is nil")
	}
	normalized := ms.DeepCopy()
	if normalized.Spec.SchedulerName == "" {
		normalized.Spec.SchedulerName = defaultSchedulerName
	}

	plugins := normalized.Spec.Plugins
	if plugins == nil {
		plugins = []workloadv1alpha1.PluginSpec{}
	}
	for i := range plugins {
		plugin := &plugins[i]
		if plugin.Type == "" {
			plugin.Type = workloadv1alpha1.PluginTypeBuiltIn
		}
		if plugin.Config != nil {
			trimmedConfig := bytes.TrimSpace(plugin.Config.Raw)
			if len(trimmedConfig) == 0 || bytes.Equal(trimmedConfig, []byte("null")) {
				plugin.Config = nil
			} else {
				canonicalConfig, err := canonicalJSON(plugin.Config.Raw)
				if err != nil {
					return nil, fmt.Errorf("canonicalize plugin %q config: %w", plugin.Name, err)
				}
				plugin.Config = &apiextensionsv1.JSON{Raw: canonicalConfig}
			}
		}
		if plugin.Scope == nil {
			continue
		}
		sort.Strings(plugin.Scope.Roles)
		plugin.Scope.Roles = slices.Compact(plugin.Scope.Roles)
		if plugin.Scope.Target == "" {
			plugin.Scope.Target = workloadv1alpha1.PluginTargetAll
		}
		if len(plugin.Scope.Roles) == 0 && plugin.Scope.Target == workloadv1alpha1.PluginTargetAll {
			plugin.Scope = nil
		}
	}

	roles := make([]modelServingRevisionRole, len(normalized.Spec.Template.Roles))
	for i := range normalized.Spec.Template.Roles {
		role := &normalized.Spec.Template.Roles[i]
		normalizePodTemplate(&role.EntryTemplate, normalized.Spec.SchedulerName)
		normalizePodTemplate(role.WorkerTemplate, normalized.Spec.SchedulerName)
		roles[i] = modelServingRevisionRole{
			Name:           role.Name,
			EntryTemplate:  role.EntryTemplate,
			WorkerReplicas: role.WorkerReplicas,
			WorkerTemplate: role.WorkerTemplate,
		}
	}
	sort.Slice(roles, func(i, j int) bool {
		return roles[i].Name < roles[j].Name
	})

	return &modelServingRevisionPatch{
		Spec: modelServingRevisionSpec{
			SchedulerName: normalized.Spec.SchedulerName,
			Plugins:       plugins,
			Template: modelServingRevisionTemplate{
				Roles: roles,
			},
		},
	}, nil
}

// normalizePodTemplate removes only differences that ModelServing itself
// overwrites before plugins render the workload. Kubernetes Pod API defaults
// remain part of the template because plugins can observe the pre-admission Pod.
func normalizePodTemplate(template *workloadv1alpha1.PodTemplateSpec, schedulerName string) {
	if template == nil {
		return
	}
	if template.Metadata != nil && len(template.Metadata.Labels) == 0 && len(template.Metadata.Annotations) == 0 {
		template.Metadata = nil
	}

	spec := &template.Spec
	spec.SchedulerName = schedulerName
	for i := range spec.InitContainers {
		removeControllerOwnedEnv(&spec.InitContainers[i])
	}
	for i := range spec.Containers {
		removeControllerOwnedEnv(&spec.Containers[i])
	}
}

func removeControllerOwnedEnv(container *corev1.Container) {
	container.Env = slices.DeleteFunc(container.Env, func(env corev1.EnvVar) bool {
		switch env.Name {
		case workloadv1alpha1.GroupSizeEnv, workloadv1alpha1.EntryAddressEnv, workloadv1alpha1.WorkerIndexEnv:
			return true
		default:
			return false
		}
	})
}

func canonicalJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}

// RevisionDataHash hashes canonical revision data. A non-nil collision count
// salts the hash so a ControllerRevision name collision can be retried without
// changing the immutable data.
func RevisionDataHash(data []byte, collisionCount *int32) string {
	hasher := fnv.New32()
	_, _ = hasher.Write(data)
	if collisionCount != nil {
		_, _ = hasher.Write([]byte(strconv.FormatInt(int64(*collisionCount), 10)))
	}
	return rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
}

// ApplyRevision restores revisioned fields while preserving operational fields
// from the current ModelServing. Historical-only Roles use the API default of
// one replica.
func ApplyRevision(ms *workloadv1alpha1.ModelServing, cr *appsv1.ControllerRevision) (*workloadv1alpha1.ModelServing, error) {
	if ms == nil {
		return nil, fmt.Errorf("model serving is nil")
	}
	if cr == nil || len(cr.Data.Raw) == 0 {
		return nil, fmt.Errorf("controller revision or its data is nil")
	}
	if !metav1.IsControlledBy(cr, ms) {
		return nil, fmt.Errorf("controller revision %q is not controlled by ModelServing %s/%s", cr.Name, ms.Namespace, ms.Name)
	}
	if cr.Annotations[ControllerRevisionDataVersionAnnotation] != ControllerRevisionDataVersionV1 {
		return nil, fmt.Errorf("controller revision %q does not contain v1 revision data", cr.Name)
	}

	patch, err := decodeRevisionPatch(cr.Data.Raw)
	if err != nil {
		return nil, err
	}
	targetRoles := make(map[string]modelServingRevisionRole, len(patch.Spec.Template.Roles))
	for _, role := range patch.Spec.Template.Roles {
		targetRoles[role.Name] = role
	}

	result := ms.DeepCopy()
	result.Spec.SchedulerName = patch.Spec.SchedulerName
	result.Spec.Plugins = patch.Spec.Plugins
	result.Spec.Template.Roles = make([]workloadv1alpha1.Role, 0, len(targetRoles))
	used := make(map[string]struct{}, len(targetRoles))
	for i := range ms.Spec.Template.Roles {
		current := &ms.Spec.Template.Roles[i]
		target, exists := targetRoles[current.Name]
		if !exists {
			continue
		}
		role := current.DeepCopy()
		applyRevisionRole(role, target)
		result.Spec.Template.Roles = append(result.Spec.Template.Roles, *role)
		used[current.Name] = struct{}{}
	}
	for _, target := range patch.Spec.Template.Roles {
		if _, exists := used[target.Name]; exists {
			continue
		}
		result.Spec.Template.Roles = append(result.Spec.Template.Roles, revisionRole(target))
	}
	return result, nil
}

// ModelServingForControllerRevision returns the ModelServing configuration to
// use when creating workloads for a ControllerRevision. V1 revisions restore
// every revisioned field. Legacy revisions only contain Roles, so fields that
// were never recorded by the legacy format remain sourced from the current
// ModelServing.
func ModelServingForControllerRevision(ms *workloadv1alpha1.ModelServing, cr *appsv1.ControllerRevision) (*workloadv1alpha1.ModelServing, error) {
	if ms == nil {
		return nil, fmt.Errorf("model serving is nil")
	}
	if cr == nil || len(cr.Data.Raw) == 0 {
		return nil, fmt.Errorf("controller revision or its data is nil")
	}
	if cr.Annotations[ControllerRevisionDataVersionAnnotation] == ControllerRevisionDataVersionV1 {
		return ApplyRevision(ms, cr)
	}
	if !metav1.IsControlledBy(cr, ms) {
		return nil, fmt.Errorf("controller revision %q is not controlled by ModelServing %s/%s", cr.Name, ms.Namespace, ms.Name)
	}

	roles, err := decodeLegacyRevisionRoles(cr.Data.Raw)
	if err != nil {
		return nil, err
	}
	result := ms.DeepCopy()
	result.Spec.Template.Roles = mergeRevisionRoles(ms.Spec.Template.Roles, roles)
	return result, nil
}

func mergeRevisionRoles(current, revision []workloadv1alpha1.Role) []workloadv1alpha1.Role {
	currentByName := make(map[string]workloadv1alpha1.Role, len(current))
	for i := range current {
		currentByName[current[i].Name] = current[i]
	}

	result := make([]workloadv1alpha1.Role, 0, len(revision))
	for i := range revision {
		role := *revision[i].DeepCopy()
		currentRole, exists := currentByName[role.Name]
		if !exists {
			// Replicas and rollout settings are operational fields. A legacy
			// snapshot may contain them because the old format stored the whole
			// Role, but historical-only Roles follow the v1 default semantics.
			defaultReplicas := int32(1)
			role.Replicas = &defaultReplicas
			role.RollingUpdateConfiguration = workloadv1alpha1.RollingUpdateConfiguration{}
			result = append(result, role)
			continue
		}
		role.Replicas = copyInt32(currentRole.Replicas)
		role.RollingUpdateConfiguration = *currentRole.RollingUpdateConfiguration.DeepCopy()
		result = append(result, role)
	}
	return result
}

func decodeRevisionPatch(data []byte) (*modelServingRevisionPatch, error) {
	var patch modelServingRevisionPatch
	if err := json.Unmarshal(data, &patch); err != nil {
		return nil, fmt.Errorf("unmarshal v1 controller revision data: %w", err)
	}
	if len(patch.Spec.Template.Roles) == 0 {
		return nil, fmt.Errorf("v1 controller revision data has no roles")
	}
	roleNames := make(map[string]struct{}, len(patch.Spec.Template.Roles))
	for _, role := range patch.Spec.Template.Roles {
		if _, exists := roleNames[role.Name]; exists {
			return nil, fmt.Errorf("revision data contains duplicate role %q", role.Name)
		}
		roleNames[role.Name] = struct{}{}
	}
	return &patch, nil
}

func revisionRole(source modelServingRevisionRole) workloadv1alpha1.Role {
	role := workloadv1alpha1.Role{}
	applyRevisionRole(&role, source)
	defaultReplicas := int32(1)
	role.Replicas = &defaultReplicas
	return role
}

func applyRevisionRole(target *workloadv1alpha1.Role, source modelServingRevisionRole) {
	target.Name = source.Name
	target.EntryTemplate = *source.EntryTemplate.DeepCopy()
	target.WorkerReplicas = source.WorkerReplicas
	if source.WorkerTemplate == nil {
		target.WorkerTemplate = nil
	} else {
		target.WorkerTemplate = source.WorkerTemplate.DeepCopy()
	}
}
