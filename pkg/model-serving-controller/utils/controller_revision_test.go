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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubefake "k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestCreateControllerRevision(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "workload.kthena.io/v1alpha1",
			Kind:       "ModelServing",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill",
					},
				},
			},
		},
	}

	templateData := ms.Spec.Template.Roles

	// Test creating a ControllerRevision
	cr, err := CreateControllerRevision(ctx, client, ms, "revision-v1", templateData)
	assert.NoError(t, err)
	assert.NotNil(t, cr)
	assert.Equal(t, "test-ms-revision-v1", cr.Name)
	assert.Equal(t, "default", cr.Namespace)
	assert.Equal(t, "test-ms", cr.Labels[ControllerRevisionLabelKey])
	assert.Equal(t, "revision-v1", cr.Labels[ControllerRevisionRevisionLabelKey])
}

func TestGetControllerRevision(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "workload.kthena.io/v1alpha1",
			Kind:       "ModelServing",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill",
					},
				},
			},
		},
	}

	templateData := ms.Spec.Template.Roles

	// Create multiple ControllerRevisions
	revisions := []string{"revision-v1", "revision-v2", "revision-v3"}
	for _, rev := range revisions {
		_, err := CreateControllerRevision(ctx, client, ms, rev, templateData)
		assert.NoError(t, err)
	}

	// GetControllerRevision should return the ControllerRevision
	cr, err := GetControllerRevision(ctx, client, ms, "revision-v2")
	assert.NoError(t, err)
	assert.NotNil(t, cr)
	assert.Equal(t, "test-ms-revision-v2", cr.Name)
	assert.Equal(t, "revision-v2", cr.Labels[ControllerRevisionRevisionLabelKey])
}

// TestCreateControllerRevision_ExistingRevisionShouldRemainImmutable verifies that
// creating a ControllerRevision with an existing revision key does not mutate
// previously stored template data.
func TestCreateControllerRevision_ExistingRevisionShouldRemainImmutable(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()

	roleReplicas := int32(1)
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "workload.kthena.io/v1alpha1",
			Kind:       "ModelServing",
		},
	}

	templateV1 := []workloadv1alpha1.Role{
		{
			Name:     "prefill",
			Replicas: &roleReplicas,
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{
				Spec: corev1.PodSpec{},
			},
		},
	}
	templateV1[0].EntryTemplate.Spec.Containers = []corev1.Container{{
		Name:  "test-container",
		Image: "nginx:1.28.2",
	}}

	templateV2 := []workloadv1alpha1.Role{
		{
			Name:     "prefill",
			Replicas: &roleReplicas,
			EntryTemplate: workloadv1alpha1.PodTemplateSpec{
				Spec: corev1.PodSpec{},
			},
		},
	}
	templateV2[0].EntryTemplate.Spec.Containers = []corev1.Container{{
		Name:  "test-container",
		Image: "nginx:alpine",
	}}

	created, err := CreateControllerRevision(ctx, client, ms, "revision-v1", templateV1)
	assert.NoError(t, err)
	assert.NotNil(t, created)
	assert.Equal(t, int64(1), created.Revision)

	_, err = CreateControllerRevision(ctx, client, ms, "revision-v1", templateV2)
	assert.NoError(t, err)

	stored, err := GetControllerRevision(ctx, client, ms, "revision-v1")
	assert.NoError(t, err)
	assert.NotNil(t, stored)

	recovered, err := GetRolesFromControllerRevision(stored)
	assert.NoError(t, err)
	if assert.Len(t, recovered, 1) {
		if assert.Len(t, recovered[0].EntryTemplate.Spec.Containers, 1) {
			assert.Equal(t, "nginx:1.28.2", recovered[0].EntryTemplate.Spec.Containers[0].Image,
				"existing revision payload should not be overwritten")
		}
	}
	assert.Equal(t, int64(1), stored.Revision,
		"existing revision object should remain unchanged for same revision key")
}

// TestCleanupOldControllerRevisions_PreservesCurrentAndUpdateRevisions tests that
// CleanupOldControllerRevisions always preserves CurrentRevision and UpdateRevision
func TestCleanupOldControllerRevisions_PreservesCurrentAndUpdateRevisions(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "workload.kthena.io/v1alpha1",
			Kind:       "ModelServing",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill",
					},
				},
			},
		},
		Status: workloadv1alpha1.ModelServingStatus{
			// Set CurrentRevision and UpdateRevision to older revisions that would normally be deleted
			CurrentRevision: "revision-v1",
			UpdateRevision:  "revision-v5",
		},
	}

	templateData := ms.Spec.Template.Roles

	// Create a few revisions to test cleanup
	// The revisions that are not CurrentRevision or UpdateRevision should be deleted
	revisions := []string{"revision-v1", "revision-v2", "revision-v3", "revision-v4", "revision-v5"}
	for _, rev := range revisions {
		_, err := CreateControllerRevision(ctx, client, ms, rev, templateData)
		assert.NoError(t, err)
	}

	// Manually run cleanup
	err := CleanupOldControllerRevisions(ctx, client, ms)
	assert.NoError(t, err)

	// List all remaining ControllerRevisions
	selector := labels.SelectorFromSet(map[string]string{
		ControllerRevisionLabelKey: ms.Name,
	})
	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	assert.NoError(t, err)

	// Verify CurrentRevision and UpdateRevision are preserved
	currentRevisionName := GenerateControllerRevisionName(ms.GetName(), ms.Status.CurrentRevision)
	updateRevisionName := GenerateControllerRevisionName(ms.GetName(), ms.Status.UpdateRevision)

	remainingRevisionNames := make(map[string]bool)
	for _, cr := range list.Items {
		remainingRevisionNames[cr.Name] = true
	}

	// CurrentRevision should be preserved even though it's old
	currentCR, err := GetControllerRevision(ctx, client, ms, ms.Status.CurrentRevision)
	assert.NoError(t, err, "CurrentRevision should be preserved")
	assert.NotNil(t, currentCR, "CurrentRevision ControllerRevision should exist")
	assert.True(t, remainingRevisionNames[currentRevisionName],
		"CurrentRevision %s should be in remaining revisions", currentRevisionName)

	// UpdateRevision should be preserved even though it's old
	updateCR, err := GetControllerRevision(ctx, client, ms, ms.Status.UpdateRevision)
	assert.NoError(t, err, "UpdateRevision should be preserved")
	assert.NotNil(t, updateCR, "UpdateRevision ControllerRevision should exist")
	assert.True(t, remainingRevisionNames[updateRevisionName],
		"UpdateRevision %s should be in remaining revisions", updateRevisionName)

	// Verify other revisions (not CurrentRevision or UpdateRevision) are deleted
	assert.False(t, remainingRevisionNames["revision-v2"], "revision-v2 should be deleted")
	assert.False(t, remainingRevisionNames["revision-v3"], "revision-v3 should be deleted")
	assert.False(t, remainingRevisionNames["revision-v4"], "revision-v4 should be deleted")

	// The total number of preserved revisions should be exactly 2 (CurrentRevision and UpdateRevision)
	assert.Equal(t, 2, len(list.Items),
		"Should preserve exactly CurrentRevision and UpdateRevision")
}
