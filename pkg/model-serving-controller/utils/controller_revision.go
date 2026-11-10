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
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	// ControllerRevisionLabelKey is the label key for ModelServing name
	ControllerRevisionLabelKey = workloadv1alpha1.ModelServingNameLabelKey
	// ControllerRevisionRevisionLabelKey is the label key for revision
	ControllerRevisionRevisionLabelKey = workloadv1alpha1.RevisionLabelKey
	// ControllerRevisionDataVersionAnnotation identifies the canonical revision
	// data format introduced for stable revision history and rollback.
	ControllerRevisionDataVersionAnnotation = "modelserving.volcano.sh/revision-data-version"
	// ControllerRevisionDataVersionV1 is the current revision data format.
	ControllerRevisionDataVersionV1 = "v1"
	// CoordinatedRoleReplicaBaselineAnnotation records the coordinator's old
	// replica counts separately from immutable template data.
	CoordinatedRoleReplicaBaselineAnnotation = "modelserving.volcano.sh/coordinated-role-replica-baseline"
)

// CreateControllerRevision maintains the legacy wrapped Role revision format
// used by older controllers. New v1 revision paths must use
// BuildRevisionData and RecordModelServingRevision so revision data remains
// immutable.
func CreateControllerRevision(ctx context.Context, client kubernetes.Interface, ms *workloadv1alpha1.ModelServing, revision string, templateData interface{}) (*appsv1.ControllerRevision, error) {
	// Serialize template data
	// Wrap data in a map to ensure it's a valid JSON object (Kubernetes requirement for RawExtension)
	wrappedData := map[string]interface{}{
		"data": templateData,
	}
	data, err := json.Marshal(wrappedData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal template data: %v", err)
	}
	validateExisting := func(existing *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
		if !controlledByModelServing(existing, ms) {
			return nil, fmt.Errorf("ControllerRevision %s/%s is not controlled by the current ModelServing UID", ms.Namespace, existing.Name)
		}
		if bytes.Equal(existing.Data.Raw, data) {
			return existing, nil
		}
		desiredRoles, desiredOK := templateData.([]workloadv1alpha1.Role)
		existingRoles, decodeErr := GetRolesFromControllerRevision(existing)
		if desiredOK && decodeErr == nil && EqualRoleTemplatesForRevision(existingRoles, desiredRoles) {
			return existing, nil
		}
		return nil, fmt.Errorf("ControllerRevision %s/%s already exists with different template data", ms.Namespace, existing.Name)
	}

	// Check if ControllerRevision already exists
	controllerRevisionName := GenerateControllerRevisionName(ms.Name, revision)
	existing, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, controllerRevisionName, metav1.GetOptions{})
	if err == nil {
		if !controlledByModelServing(existing, ms) {
			return nil, fmt.Errorf("ControllerRevision %s/%s is not controlled by this ModelServing", ms.Namespace, controllerRevisionName)
		}
		// A revision name identifies immutable historical template data. Never
		// overwrite it: semantic equality may reuse it, but different data is a
		// collision and must remain unchanged.
		return validateExisting(existing)
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get ControllerRevision: %v", err)
	}

	// Create ControllerRevision
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerRevisionName,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				ControllerRevisionLabelKey:         ms.Name,
				ControllerRevisionRevisionLabelKey: revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				newModelServingOwnerRef(ms),
			},
		},
		Revision: 1, // ControllerRevision revision number
		Data: runtime.RawExtension{
			Raw: data,
		},
	}

	// Create ControllerRevision
	created, err := client.AppsV1().ControllerRevisions(ms.Namespace).Create(ctx, cr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, controllerRevisionName, metav1.GetOptions{})
		if getErr != nil {
			return nil, fmt.Errorf("failed to get concurrently created ControllerRevision: %v", getErr)
		}
		return validateExisting(existing)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create ControllerRevision: %v", err)
	}

	klog.V(4).Infof("Created ControllerRevision %s/%s with revision %s", ms.Namespace, controllerRevisionName, revision)
	return created, nil
}

// GetControllerRevision retrieves a ControllerRevision by its revision string
func GetControllerRevision(
	ctx context.Context,
	client kubernetes.Interface,
	ms *workloadv1alpha1.ModelServing,
	revision string,
) (*appsv1.ControllerRevision, error) {
	//TODO: get it from a informer's store
	controllerRevisionName := GenerateControllerRevisionName(ms.Name, revision)
	cr, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, controllerRevisionName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return cr, nil
}

// GetRolesFromControllerRevision extracts roles template data from a ControllerRevision
func GetRolesFromControllerRevision(cr *appsv1.ControllerRevision) ([]workloadv1alpha1.Role, error) {
	if cr == nil || cr.Data.Raw == nil {
		return nil, fmt.Errorf("ControllerRevision or its data is nil")
	}
	if cr.Annotations[ControllerRevisionDataVersionAnnotation] == ControllerRevisionDataVersionV1 {
		patch, err := decodeRevisionPatch(cr.Data.Raw)
		if err != nil {
			return nil, err
		}
		roles := make([]workloadv1alpha1.Role, 0, len(patch.Spec.Template.Roles))
		for _, role := range patch.Spec.Template.Roles {
			roles = append(roles, revisionRole(role))
		}
		return roles, nil
	}
	return decodeLegacyRevisionRoles(cr.Data.Raw)
}

func decodeLegacyRevisionRoles(data []byte) ([]workloadv1alpha1.Role, error) {
	// Try to unmarshal as wrapped data first.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err == nil {
		if rawData, ok := wrapper["data"]; ok {
			var roles []workloadv1alpha1.Role
			if err := json.Unmarshal(rawData, &roles); err != nil {
				return nil, fmt.Errorf("failed to unmarshal roles from wrapped data: %v", err)
			}
			return roles, nil
		}
	}

	// Fallback: try to unmarshal directly (for backward compatibility or if not wrapped)
	var roles []workloadv1alpha1.Role
	if err := json.Unmarshal(data, &roles); err != nil {
		return nil, fmt.Errorf("failed to unmarshal roles from ControllerRevision: %v", err)
	}

	return roles, nil
}

// ModelServingAtRevision rebuilds ms with the revisioned fields recorded in cr.
// A ControllerRevision written before v1 revision data records only the Roles.
func ModelServingAtRevision(ms *workloadv1alpha1.ModelServing, cr *appsv1.ControllerRevision) (*workloadv1alpha1.ModelServing, error) {
	if cr != nil && cr.Annotations[ControllerRevisionDataVersionAnnotation] == ControllerRevisionDataVersionV1 {
		return ApplyRevision(ms, cr)
	}
	roles, err := GetRolesFromControllerRevision(cr)
	if err != nil {
		return nil, err
	}
	result := ms.DeepCopy()
	result.Spec.Template.Roles = roles
	return result, nil
}

// CleanupOldControllerRevisions deletes old ControllerRevisions that are no longer in use.
// Status names current/update revisions; active mixed units may reference additional snapshots.
// Preserve status, store and live Pod references, including mixed Role revisions.
func CleanupOldControllerRevisions(
	ctx context.Context,
	client kubernetes.Interface,
	ms *workloadv1alpha1.ModelServing,
	referencedRevisions ...string,
) error {
	// Get all ControllerRevisions for this ModelServing
	selector := labels.SelectorFromSet(map[string]string{
		ControllerRevisionLabelKey: ms.Name,
	})

	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list ControllerRevisions: %v", err)
	}

	// Get the revision names that must be preserved (CurrentRevision and UpdateRevision)
	var currentRevisionName, updateRevisionName string
	if ms.Status.CurrentRevision != "" {
		currentRevisionName = GenerateControllerRevisionName(ms.Name, ms.Status.CurrentRevision)
	}
	if ms.Status.UpdateRevision != "" {
		updateRevisionName = GenerateControllerRevisionName(ms.Name, ms.Status.UpdateRevision)
	}
	preservedRevisionNames := make(map[string]struct{}, len(referencedRevisions)+2)
	if currentRevisionName != "" {
		preservedRevisionNames[currentRevisionName] = struct{}{}
	}
	if updateRevisionName != "" {
		preservedRevisionNames[updateRevisionName] = struct{}{}
	}
	for _, referencedRevision := range referencedRevisions {
		if referencedRevision != "" {
			preservedRevisionNames[GenerateControllerRevisionName(ms.Name, referencedRevision)] = struct{}{}
		}
	}

	// API Pods fence cache gaps and unfinished/terminating units. Their historical
	// snapshot remains necessary until the last actual reference disappears.
	pods, err := client.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list Pod revision references: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if controlledByModelServing(pod, ms) {
			if revision := ObjectRevision(pod); revision != "" {
				preservedRevisionNames[GenerateControllerRevisionName(ms.Name, revision)] = struct{}{}
			}
			if revision := unfinishedInPlaceRevision(pod); revision != "" {
				preservedRevisionNames[GenerateControllerRevisionName(ms.Name, revision)] = struct{}{}
			}
		}
	}

	// Delete only unreferenced history controlled by this exact ModelServing.
	deletedCount := 0
	for i := range list.Items {
		revision := &list.Items[i]
		if !controlledByModelServing(revision, ms) {
			continue
		}
		// Skip if this revision must be preserved
		if _, preserved := preservedRevisionNames[revision.Name]; preserved {
			continue
		}
		err := client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, revision.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &revision.UID}})
		if err != nil && !apierrors.IsNotFound(err) {
			klog.Warningf("Failed to delete old ControllerRevision %s/%s: %v", ms.Namespace, revision.Name, err)
		} else {
			deletedCount++
			klog.V(4).Infof("Deleted old ControllerRevision %s/%s", ms.Namespace, revision.Name)
		}
	}

	if deletedCount > 0 {
		klog.V(4).Infof("Cleaned up %d old ControllerRevisions for ModelServing %s/%s (preserved CurrentRevision=%s, UpdateRevision=%s)",
			deletedCount, ms.Namespace, ms.Name, ms.Status.CurrentRevision, ms.Status.UpdateRevision)
	}

	return nil
}

// An unfinished patch can still target B after the desired revision advances to
// C while the Pod label remains A. Decode only its history reference here: the
// inplace package depends on utils. Keep current and legacy completion semantics
// aligned with inplace.StateOf; completed patch records do not pin old history.
func unfinishedInPlaceRevision(pod *corev1.Pod) string {
	var state struct {
		Revision       string `json:"revision"`
		TargetRevision string `json:"targetRevision"`
		Version        int    `json:"version"`
		Phase          string `json:"phase"`
		Done           bool   `json:"done"`
	}
	if err := json.Unmarshal([]byte(pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey]), &state); err != nil {
		return ""
	}
	if state.Version != 0 || state.Phase != "" {
		if state.Phase != "Done" {
			return state.TargetRevision
		}
	} else if !state.Done {
		return state.Revision
	}
	return ""
}

// controlledByModelServing requires the complete controller identity, not just labels.
func controlledByModelServing(object metav1.Object, ms *workloadv1alpha1.ModelServing) bool {
	owner := metav1.GetControllerOf(object)
	return object.GetNamespace() == ms.Namespace && owner != nil && owner.APIVersion == workloadv1alpha1.SchemeGroupVersion.String() && owner.Kind == workloadv1alpha1.ModelServingKind.Kind && owner.Name == ms.Name && owner.UID == ms.UID
}
