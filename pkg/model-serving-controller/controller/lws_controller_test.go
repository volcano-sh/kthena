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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	lwsclientset "sigs.k8s.io/lws/client-go/clientset/versioned"
	lwsinformers "sigs.k8s.io/lws/client-go/informers/externalversions"
	lwslisters "sigs.k8s.io/lws/client-go/listers/leaderworkerset/v1"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	kthenainformers "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	kthenalisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	msplugins "github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
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
			hasHeadlessServicePlugin := false
			for _, plugin := range got.Spec.Plugins {
				if plugin.Name == msplugins.HeadlessServicePluginName {
					hasHeadlessServicePlugin = true
					break
				}
			}
			assert.True(t, hasHeadlessServicePlugin)

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

func TestLWSSyncHandlerModelServingOwnership(t *testing.T) {
	lws := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "team-llm", Namespace: "default", UID: "lws-uid"},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas: ptr.To(int32(1)),
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				WorkerTemplate: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "nginx"}}},
				},
			},
		},
	}
	// A ModelServing a user created with the same name as the LWS.
	userMS := func() *workloadv1alpha1.ModelServing {
		return &workloadv1alpha1.ModelServing{
			ObjectMeta: metav1.ObjectMeta{Name: "team-llm", Namespace: "default", UID: "existing-ms-uid", Annotations: map[string]string{"example.com/preserve": "yes"}},
			Spec:       workloadv1alpha1.ModelServingSpec{Replicas: ptr.To(int32(3))},
		}
	}
	ownedBy := func(ms *workloadv1alpha1.ModelServing, uid string) *workloadv1alpha1.ModelServing {
		ms.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: lwsv1.GroupVersion.String(),
			Kind:       "LeaderWorkerSet",
			Name:       "team-llm",
			UID:        types.UID(uid),
			Controller: ptr.To(true),
		}}
		return ms
	}

	managedMS := (&LWSController{}).constructModelServing(lws)
	managedMS.Spec.Replicas = ptr.To(int32(3))

	tests := []struct {
		name         string
		existing     *workloadv1alpha1.ModelServing
		wantErr      string
		wantReplicas int32
		wantVerbs    []string
	}{
		{
			name:         "creates the ModelServing when none exists",
			wantReplicas: 1,
			wantVerbs:    []string{"create"},
		},
		{
			name:         "updates a ModelServing controlled by this LWS",
			existing:     managedMS,
			wantReplicas: 1,
			wantVerbs:    []string{"update"},
		},
		{
			name:         "preserves an owned child with an incompatible immutable plugin declaration",
			existing:     ownedBy(userMS(), "lws-uid"),
			wantErr:      "MigrationRequired",
			wantReplicas: 3,
		},
		{
			name:         "does not touch a ModelServing with no owner",
			existing:     userMS(),
			wantErr:      "is not controlled by LeaderWorkerSet team-llm",
			wantReplicas: 3,
		},
		{
			name:         "does not touch a ModelServing controlled by another LWS with the same name",
			existing:     ownedBy(userMS(), "older-lws-uid"),
			wantErr:      "is not controlled by LeaderWorkerSet team-llm",
			wantReplicas: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msObjs []runtime.Object
			if tt.existing != nil {
				msObjs = append(msObjs, tt.existing)
			}
			kthenaClient := kthenafake.NewSimpleClientset(msObjs...)
			// The LWS status is already up to date in these cases, so the LWS client
			// is never called and the informer is never started.
			var lwsClient lwsclientset.Interface
			lwsFactory := lwsinformers.NewSharedInformerFactory(lwsClient, 0)
			kthenaFactory := kthenainformers.NewSharedInformerFactory(kthenaClient, 0)

			c, err := NewLWSController(kubefake.NewSimpleClientset(), kthenaClient, lwsClient, lwsFactory, kthenaFactory)
			require.NoError(t, err)
			require.NoError(t, lwsFactory.Leaderworkerset().V1().LeaderWorkerSets().Informer().GetIndexer().Add(lws))
			if tt.existing != nil {
				require.NoError(t, kthenaFactory.Workload().V1alpha1().ModelServings().Informer().GetIndexer().Add(tt.existing))
			}
			kthenaClient.ClearActions()

			err = c.syncHandler(context.Background(), "default/team-llm")
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}

			var verbs []string
			for _, action := range kthenaClient.Actions() {
				if verb := action.GetVerb(); verb == "create" || verb == "update" || verb == "patch" || verb == "delete" || verb == "delete-collection" {
					verbs = append(verbs, action.GetVerb())
				}
			}
			assert.Equal(t, tt.wantVerbs, verbs)

			got, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Get(context.Background(), "team-llm", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, tt.wantReplicas, *got.Spec.Replicas)
			if tt.wantErr != "" {
				assert.Equal(t, tt.existing, got, "rejected synchronization must preserve the entire stored child")
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
	kubeClient := kubefake.NewSimpleClientset()
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

	tests := []struct {
		name   string
		mutate func(*corev1.Service)
	}{
		{name: "no owner", mutate: func(s *corev1.Service) { s.OwnerReferences = nil }},
		{name: "different owner UID", mutate: func(s *corev1.Service) { s.OwnerReferences[0].UID = "old-uid" }},
		{name: "not headless", mutate: func(s *corev1.Service) { s.Spec.ClusterIP = "10.0.0.1" }},
		{name: "does not publish unready addresses", mutate: func(s *corev1.Service) { s.Spec.PublishNotReadyAddresses = false }},
		{name: "wrong selector", mutate: func(s *corev1.Service) { s.Spec.Selector[lwsv1.SetNameLabelKey] = "other" }},
		{name: "extra selector", mutate: func(s *corev1.Service) { s.Spec.Selector["extra"] = "value" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := service.DeepCopy()
			tt.mutate(existing)
			client := kubefake.NewSimpleClientset(existing)
			controller := &LWSController{kubeClient: client}
			err := controller.ensureLWSHeadlessService(context.Background(), lws)
			require.ErrorContains(t, err, "service default/sample")
			// Invalid services must not be adopted, modified, or deleted.
			require.Len(t, client.Actions(), 1)
			assert.Equal(t, "get", client.Actions()[0].GetVerb())
		})
	}

	t.Run("concurrent creation is retried", func(t *testing.T) {
		client := kubefake.NewSimpleClientset()
		client.PrependReactor("create", "services", func(action clienttesting.Action) (bool, runtime.Object, error) {
			// Another actor creates a conflicting Service after our GET.
			existing := service.DeepCopy()
			existing.OwnerReferences = nil
			require.NoError(t, client.Tracker().Add(existing))
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "services"}, lws.Name)
		})
		controller := &LWSController{kubeClient: client}
		err := controller.ensureLWSHeadlessService(context.Background(), lws)
		require.True(t, apierrors.IsAlreadyExists(err), "expected a retryable creation conflict, got %v", err)
		require.ErrorContains(t, controller.ensureLWSHeadlessService(context.Background(), lws), "is not controlled by LeaderWorkerSet")
	})
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
	kubeClient := kubefake.NewSimpleClientset(service)
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
