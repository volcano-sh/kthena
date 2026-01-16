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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestConstructModelServing(t *testing.T) {
	tests := []struct {
		name     string
		lws      *lwsv1.LeaderWorkerSet
		expected *workloadv1alpha1.ModelServing
	}{
		{
			name: "basic translation with defaults",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			expected: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "leaderworkerset.x-k8s.io/v1",
							Kind:               "LeaderWorkerSet",
							Name:               "test-lws",
							UID:                "test-uid",
							Controller:         ptr.To(true),
							BlockOwnerDeletion: ptr.To(true),
						},
					},
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "default",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 0, // Default Size is nil -> 1, 1-1 = 0
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "custom replicas and size",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-custom",
					Namespace: "default",
				},
				Spec: lwsv1.LeaderWorkerSetSpec{
					Replicas: ptr.To[int32](3),
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: ptr.To[int32](4),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			expected: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-custom",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](3),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "default",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 3, // 4-1 = 3
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "separate leader template",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-leader",
					Namespace: "default",
				},
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: ptr.To[int32](2),
						LeaderTemplate: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "leader", Image: "leader-image"},
								},
							},
						},
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker", Image: "worker-image"},
								},
							},
						},
					},
				},
			},
			expected: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-leader",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "default",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 1, // 2-1 = 1
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "leader", Image: "leader-image"},
										},
									},
								},
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "worker-image"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "labels and annotations propagation",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-meta",
					Namespace: "default",
				},
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						WorkerTemplate: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels:      map[string]string{"app": "test"},
								Annotations: map[string]string{"note": "test"},
							},
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			expected: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-meta",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "default",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 0,
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Metadata: &workloadv1alpha1.Metadata{
										Labels:      map[string]string{"app": "test"},
										Annotations: map[string]string{"note": "test"},
									},
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Metadata: &workloadv1alpha1.Metadata{
										Labels:      map[string]string{"app": "test"},
										Annotations: map[string]string{"note": "test"},
									},
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "corner case: size 1 means 0 worker replicas",
			lws: &lwsv1.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-size-1",
					Namespace: "default",
				},
				Spec: lwsv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
						Size: ptr.To[int32](1),
						WorkerTemplate: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "worker", Image: "nginx"},
								},
							},
						},
					},
				},
			},
			expected: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lws-size-1",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "default",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 0,
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "worker", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	c := &LWSController{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.constructModelServing(tt.lws)

			// Verify ObjectMeta
			assert.Equal(t, tt.expected.Name, got.Name)
			assert.Equal(t, tt.expected.Namespace, got.Namespace)
			if len(tt.expected.OwnerReferences) > 0 {
				assert.Equal(t, tt.expected.OwnerReferences[0].Name, got.OwnerReferences[0].Name)
				assert.Equal(t, tt.expected.OwnerReferences[0].Kind, got.OwnerReferences[0].Kind)
			}

			// Verify Spec
			assert.Equal(t, *tt.expected.Spec.Replicas, *got.Spec.Replicas)
			assert.Equal(t, len(tt.expected.Spec.Template.Roles), len(got.Spec.Template.Roles))

			role := got.Spec.Template.Roles[0]
			expectedRole := tt.expected.Spec.Template.Roles[0]

			assert.Equal(t, expectedRole.Name, role.Name)
			assert.Equal(t, *expectedRole.Replicas, *role.Replicas)
			assert.Equal(t, expectedRole.WorkerReplicas, role.WorkerReplicas)

			// Verify Templates
			assert.Equal(t, expectedRole.EntryTemplate.Spec.Containers[0].Name, role.EntryTemplate.Spec.Containers[0].Name)
			assert.Equal(t, expectedRole.EntryTemplate.Spec.Containers[0].Image, role.EntryTemplate.Spec.Containers[0].Image)

			if expectedRole.WorkerTemplate != nil {
				assert.NotNil(t, role.WorkerTemplate)
				assert.Equal(t, expectedRole.WorkerTemplate.Spec.Containers[0].Name, role.WorkerTemplate.Spec.Containers[0].Name)
			}

			// Verify Metadata if present
			if expectedRole.EntryTemplate.Metadata != nil {
				assert.Equal(t, expectedRole.EntryTemplate.Metadata.Labels, role.EntryTemplate.Metadata.Labels)
				assert.Equal(t, expectedRole.EntryTemplate.Metadata.Annotations, role.EntryTemplate.Metadata.Annotations)
			}
		})
	}
}
