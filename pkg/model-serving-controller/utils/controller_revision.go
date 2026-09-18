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
	"context"
	"encoding/json"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
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
)

// CreateControllerRevision maintains the legacy wrapped Role revision format
// used by the current controller integration. New v1 revision paths must use
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

	// Check if ControllerRevision already exists
	controllerRevisionName := GenerateControllerRevisionName(ms.Name, revision)
	existing, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, controllerRevisionName, metav1.GetOptions{})
	if err == nil {
		// A revision name identifies immutable historical template data. Never
		// overwrite it: doing so would make stable ordinal recovery use a
		// template different from the one referenced by live resources.
		if string(existing.Data.Raw) != string(data) {
			return nil, fmt.Errorf("ControllerRevision %s/%s already exists with different template data", ms.Namespace, controllerRevisionName)
		}
		return existing, nil
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

const defaultRevisionHistoryLimit int32 = 10

// CleanupOldControllerRevisions removes the oldest non-live revisions until
// RevisionHistoryLimit is satisfied. CurrentRevision, UpdateRevision, and
// persisted RevisionReferences are the authoritative live revision pins.
func CleanupOldControllerRevisions(
	ctx context.Context,
	client kubernetes.Interface,
	ms *workloadv1alpha1.ModelServing,
) error {
	selector := labels.SelectorFromSet(map[string]string{
		ControllerRevisionLabelKey: ms.Name,
	})

	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Errorf("list ControllerRevisions: %w", err)
	}

	live := make(map[string]struct{})
	if ms.Status.CurrentRevision != "" {
		live[ms.Status.CurrentRevision] = struct{}{}
	}
	if ms.Status.UpdateRevision != "" {
		live[ms.Status.UpdateRevision] = struct{}{}
	}
	for _, revision := range ms.Status.RevisionReferences {
		if revision != "" {
			live[revision] = struct{}{}
		}
	}
	nonLive := make([]*appsv1.ControllerRevision, 0, len(list.Items))
	for i := range list.Items {
		revision := &list.Items[i]
		owner := metav1.GetControllerOfNoCopy(revision)
		if owner == nil || owner.UID != ms.UID {
			continue
		}
		if _, exists := live[revision.Labels[ControllerRevisionRevisionLabelKey]]; exists {
			continue
		}
		nonLive = append(nonLive, revision)
	}

	sort.Slice(nonLive, func(i, j int) bool {
		return controllerRevisionLess(nonLive[i], nonLive[j])
	})
	limit := defaultRevisionHistoryLimit
	if ms.Spec.RevisionHistoryLimit != nil {
		limit = *ms.Spec.RevisionHistoryLimit
	}
	excess := len(nonLive) - int(limit)
	for i := 0; i < excess; i++ {
		revision := nonLive[i]
		if err := client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, revision.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete old ControllerRevision %s: %w", revision.Name, err)
		}
		klog.V(4).Infof("Deleted old ControllerRevision %s/%s", ms.Namespace, revision.Name)
	}

	return nil
}
