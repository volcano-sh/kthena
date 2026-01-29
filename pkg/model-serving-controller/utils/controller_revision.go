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
)

// CreateControllerRevision creates or retrieves a ControllerRevision for a specific revision.
// In ModelServing, we typically have at most two revisions: CurrentRevision and UpdateRevision.
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
		// If already exists, check if data has changed
		if string(existing.Data.Raw) != string(data) {
			existing.Data = runtime.RawExtension{
				Raw: data,
			}
			existing.Revision++
			updated, updateErr := client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
			if updateErr != nil {
				return nil, fmt.Errorf("failed to update ControllerRevision: %v", updateErr)
			}
			klog.V(4).Infof("Updated ControllerRevision %s/%s with revision %s", ms.Namespace, controllerRevisionName, revision)
			return updated, nil
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

	// Try to unmarshal as wrapped data first
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(cr.Data.Raw, &wrapper); err == nil {
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
	if err := json.Unmarshal(cr.Data.Raw, &roles); err != nil {
		return nil, fmt.Errorf("failed to unmarshal roles from ControllerRevision: %v", err)
	}

	return roles, nil
}

// CleanupOldControllerRevisions deletes old ControllerRevisions that are no longer in use.
// In ModelServing, we typically have at most two revisions (CurrentRevision and UpdateRevision),
// so this cleanup removes all revisions except CurrentRevision and UpdateRevision.
func CleanupOldControllerRevisions(
	ctx context.Context,
	client kubernetes.Interface,
	ms *workloadv1alpha1.ModelServing,
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

	// Delete all revisions except CurrentRevision and UpdateRevision
	deletedCount := 0
	for i := range list.Items {
		revision := &list.Items[i]
		// Skip if this revision must be preserved
		if (currentRevisionName != "" && revision.Name == currentRevisionName) ||
			(updateRevisionName != "" && revision.Name == updateRevisionName) {
			continue
		}

		err := client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, revision.Name, metav1.DeleteOptions{})
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
