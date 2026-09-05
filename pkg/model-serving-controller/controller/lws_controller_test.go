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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	lwslisters "sigs.k8s.io/lws/client-go/listers/leaderworkerset/v1"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	kthenalisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
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

func TestEnsureLWSHeadlessService(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample",
			Namespace: "default",
			UID:       "sample-uid",
		},
	}
	kubeClient := fake.NewSimpleClientset()
	controller := &LWSController{kubeClient: kubeClient}

	require.NoError(t, controller.ensureLWSHeadlessService(context.Background(), lws))
	require.NoError(t, controller.ensureLWSHeadlessService(context.Background(), lws))

	service, err := kubeClient.CoreV1().Services(lws.Namespace).Get(context.Background(), lws.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, service.Spec.ClusterIP)
	assert.True(t, service.Spec.PublishNotReadyAddresses)
	assert.Equal(t, map[string]string{lwsv1.SetNameLabelKey: lws.Name}, service.Spec.Selector)
	require.Len(t, service.OwnerReferences, 1)
	assert.Equal(t, lws.UID, service.OwnerReferences[0].UID)
}

func TestDeletedLWSHeadlessServiceIsRecreated(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample",
			Namespace: "default",
			UID:       "sample-uid",
		},
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lws.Name,
			Namespace: lws.Namespace,
			Labels:    map[string]string{lwsv1.SetNameLabelKey: lws.Name},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(lws, lwsv1.GroupVersion.WithKind("LeaderWorkerSet")),
			},
		},
	}
	kubeClient := fake.NewSimpleClientset(service)
	lwsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, lwsIndexer.Add(lws))
	modelServingIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	controller := &LWSController{
		kubeClient:         kubeClient,
		kthenaClient:       kthenafake.NewSimpleClientset(),
		lwsLister:          lwslisters.NewLeaderWorkerSetLister(lwsIndexer),
		modelServingLister: kthenalisters.NewModelServingLister(modelServingIndexer),
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "LeaderWorkerSets"},
		),
	}
	t.Cleanup(controller.workqueue.ShutDown)

	require.NoError(t, kubeClient.CoreV1().Services(lws.Namespace).Delete(context.Background(), service.Name, metav1.DeleteOptions{}))
	controller.handleObject(cache.DeletedFinalStateUnknown{Key: lws.Namespace + "/" + service.Name, Obj: service})
	require.Equal(t, 1, controller.workqueue.Len())
	key, shutdown := controller.workqueue.Get()
	require.False(t, shutdown)
	require.Equal(t, lws.Namespace+"/"+lws.Name, key)

	require.NoError(t, controller.syncHandler(context.Background(), key))
	controller.workqueue.Done(key)
	controller.workqueue.Forget(key)
	recreated, err := kubeClient.CoreV1().Services(lws.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, recreated.Spec.ClusterIP)
	assert.True(t, recreated.Spec.PublishNotReadyAddresses)
}

func TestBuildLWSStatus(t *testing.T) {
	transitionTime := metav1.NewTime(time.Unix(100, 0))
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "default", Generation: 3},
	}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status: workloadv1alpha1.ModelServingStatus{
			ObservedGeneration: 1,
			Replicas:           2,
			UpdatedReplicas:    2,
			AvailableReplicas:  2,
			Conditions: []metav1.Condition{{
				Type:               string(workloadv1alpha1.ModelServingAvailable),
				Status:             metav1.ConditionTrue,
				Reason:             "AllGroupsReady",
				Message:            "All Serving groups are ready",
				LastTransitionTime: transitionTime,
			}},
		},
	}
	status := buildLWSStatus(lws, ms, metav1.NewTime(time.Unix(200, 0)))
	assert.Equal(t, int32(2), status.Replicas)
	assert.Equal(t, int32(2), status.ReadyReplicas)
	assert.Equal(t, int32(2), status.UpdatedReplicas)
	require.Len(t, status.Conditions, 1)
	assert.Equal(t, string(lwsv1.LeaderWorkerSetAvailable), status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, status.Conditions[0].Status)
	assert.Equal(t, int64(3), status.Conditions[0].ObservedGeneration)
}

func TestProjectLWSConditionsTracksTransitions(t *testing.T) {
	oldTime := metav1.NewTime(time.Unix(100, 0))
	firstTransition := metav1.NewTime(time.Unix(200, 0))
	secondReconcile := metav1.NewTime(time.Unix(300, 0))
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 5},
		Status: lwsv1.LeaderWorkerSetStatus{Conditions: []metav1.Condition{
			{Type: string(lwsv1.LeaderWorkerSetAvailable), Status: metav1.ConditionTrue, LastTransitionTime: oldTime},
			{Type: string(lwsv1.LeaderWorkerSetProgressing), Status: metav1.ConditionFalse, LastTransitionTime: oldTime},
			{Type: "External", Status: metav1.ConditionTrue, LastTransitionTime: oldTime},
		}},
	}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status: workloadv1alpha1.ModelServingStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: string(workloadv1alpha1.ModelServingAvailable), Status: metav1.ConditionFalse, Reason: "GroupsProgressing"},
			{Type: string(workloadv1alpha1.ModelServingProgressing), Status: metav1.ConditionTrue, Reason: "GroupProgressing"},
		}},
	}

	projected := projectLWSConditions(lws, ms, firstTransition)
	require.Len(t, projected, 3)
	assertCondition(t, projected, "External", metav1.ConditionTrue, oldTime)
	assertCondition(t, projected, string(lwsv1.LeaderWorkerSetAvailable), metav1.ConditionFalse, firstTransition)
	assertCondition(t, projected, string(lwsv1.LeaderWorkerSetProgressing), metav1.ConditionTrue, firstTransition)

	lws.Status.Conditions = projected
	projected = projectLWSConditions(lws, ms, secondReconcile)
	assertCondition(t, projected, string(lwsv1.LeaderWorkerSetAvailable), metav1.ConditionFalse, firstTransition)
	assertCondition(t, projected, string(lwsv1.LeaderWorkerSetProgressing), metav1.ConditionTrue, firstTransition)
}

func TestProjectLWSConditionsRejectsStaleAvailability(t *testing.T) {
	now := metav1.NewTime(time.Unix(200, 0))
	lws := &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Status: workloadv1alpha1.ModelServingStatus{
			ObservedGeneration: 3,
			Conditions: []metav1.Condition{{
				Type:   string(workloadv1alpha1.ModelServingAvailable),
				Status: metav1.ConditionTrue,
			}},
		},
	}

	projected := projectLWSConditions(lws, ms, now)
	require.Len(t, projected, 1)
	assert.Equal(t, string(lwsv1.LeaderWorkerSetAvailable), projected[0].Type)
	assert.Equal(t, metav1.ConditionFalse, projected[0].Status)
	assert.Equal(t, "ModelServingStatusStale", projected[0].Reason)
	assert.Equal(t, int64(2), projected[0].ObservedGeneration)
	assert.Equal(t, now, projected[0].LastTransitionTime)
}

func assertCondition(t *testing.T, conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, transitionTime metav1.Time) {
	t.Helper()
	for _, condition := range conditions {
		if condition.Type == conditionType {
			assert.Equal(t, status, condition.Status)
			assert.Equal(t, transitionTime, condition.LastTransitionTime)
			return
		}
	}
	t.Fatalf("condition %q not found", conditionType)
}
