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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

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

	// A hash collision or corrupted historical snapshot must never be repaired
	// by overwriting data referenced by live stable or surge resources.
	_, err = CreateControllerRevision(ctx, client, ms, "revision-v1", []workloadv1alpha1.Role{{Name: "decode"}})
	assert.ErrorContains(t, err, "already exists with different template data")
	persisted, err := GetControllerRevision(ctx, client, ms, "revision-v1")
	assert.NoError(t, err)
	roles, err := GetRolesFromControllerRevision(persisted)
	assert.NoError(t, err)
	assert.Equal(t, "prefill", roles[0].Name)
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

func TestCleanupOldControllerRevisionsRetainsPinnedRevisionsAndHistoryLimit(t *testing.T) {
	tests := []struct {
		name       string
		limit      int32
		current    string
		update     string
		references []string
		revisions  []string
		want       []string
	}{
		{
			name:      "current and update with zero history",
			limit:     0,
			current:   "r1",
			update:    "r5",
			revisions: []string{"r1", "r2", "r3", "r4", "r5"},
			want:      []string{"r1", "r5"},
		},
		{
			name:       "durable reference and newest unused with limit one",
			limit:      1,
			current:    "r1",
			update:     "r5",
			references: []string{"r2"},
			revisions:  []string{"r1", "r2", "r3", "r4", "r5"},
			want:       []string{"r1", "r2", "r4", "r5"},
		},
		{
			name:       "durable reference with zero history",
			limit:      0,
			current:    "r1",
			update:     "r3",
			references: []string{"r2"},
			revisions:  []string{"r1", "r2", "r3"},
			want:       []string{"r1", "r2", "r3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "default", UID: "cleanup-uid"},
				Spec:       workloadv1alpha1.ModelServingSpec{RevisionHistoryLimit: ptr.To(tt.limit)},
				Status: workloadv1alpha1.ModelServingStatus{
					CurrentRevision: tt.current, UpdateRevision: tt.update, RevisionReferences: tt.references,
				},
			}
			client := kubefake.NewSimpleClientset()
			for _, revision := range tt.revisions {
				_, err := CreateControllerRevision(ctx, client, ms, revision, nil)
				assert.NoError(t, err)
			}
			assert.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))

			list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(map[string]string{ControllerRevisionLabelKey: ms.Name}).String(),
			})
			assert.NoError(t, err)
			remaining := make([]string, 0, len(list.Items))
			for _, cr := range list.Items {
				remaining = append(remaining, cr.Labels[ControllerRevisionRevisionLabelKey])
			}
			assert.ElementsMatch(t, tt.want, remaining)
		})
	}
}
