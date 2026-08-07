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
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

func TestHandlePodAfterGraceTime(t *testing.T) {
	const (
		namespace = "default"
		podName   = "test-model-0-prefill-0-0"
	)
	gracePeriod := int64(1)
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      podName,
			UID:       types.UID("failed-pod"),
		},
	}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "test-model",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				RestartGracePeriodSeconds: &gracePeriod,
			},
		},
	}

	tests := []struct {
		name          string
		currentPodUID types.UID
		wantDeleted   bool
	}{
		{
			name:          "deletes failed pod",
			currentPodUID: failedPod.UID,
			wantDeleted:   true,
		},
		{
			name:          "keeps replacement pod",
			currentPodUID: types.UID("replacement-pod"),
			wantDeleted:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			currentPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      podName,
					UID:       tt.currentPodUID,
				},
				Status: corev1.PodStatus{Phase: corev1.PodPending},
			}
			controller, kubeClient := newGracePeriodTestController(t, currentPod)

			controller.handlePodAfterGraceTime(ms, failedPod)

			_, err := kubeClient.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
			if tt.wantDeleted {
				assert.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
			}

			actions := kubeClient.Actions()
			if !tt.wantDeleted {
				for _, action := range actions {
					assert.False(t, action.Matches("delete", "pods"))
				}
				return
			}
			require.Len(t, actions, 2)
			deleteAction, ok := actions[0].(kubetesting.DeleteAction)
			require.True(t, ok)
			require.NotNil(t, deleteAction.GetDeleteOptions().Preconditions)
			require.NotNil(t, deleteAction.GetDeleteOptions().Preconditions.UID)
			assert.Equal(t, failedPod.UID, *deleteAction.GetDeleteOptions().Preconditions.UID)
		})
	}
}

func TestHandlePodWithoutGraceTimeUsesUIDPrecondition(t *testing.T) {
	const (
		namespace = "default"
		podName   = "test-model-0-prefill-0-0"
	)
	podUID := types.UID("failed-pod")
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      podName,
			UID:       podUID,
		},
	}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "test-model",
		},
	}
	controller, kubeClient := newGracePeriodTestController(t, failedPod)

	controller.handlePodAfterGraceTime(ms, failedPod)

	actions := kubeClient.Actions()
	require.Len(t, actions, 1)
	deleteAction, ok := actions[0].(kubetesting.DeleteAction)
	require.True(t, ok)
	require.NotNil(t, deleteAction.GetDeleteOptions().Preconditions)
	require.NotNil(t, deleteAction.GetDeleteOptions().Preconditions.UID)
	assert.Equal(t, podUID, *deleteAction.GetDeleteOptions().Preconditions.UID)
}

func TestHandleErrorPodTracksReplacementByUID(t *testing.T) {
	const (
		namespace = "default"
		podName   = "test-model-0-prefill-0-0"
	)
	gracePeriod := int64(1)
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      podName,
			UID:       types.UID("failed-pod"),
		},
	}
	replacementPod := failedPod.DeepCopy()
	replacementPod.UID = types.UID("replacement-pod")
	replacementPod.Status.Phase = corev1.PodPending

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "test-model",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				RestartGracePeriodSeconds: &gracePeriod,
			},
		},
	}
	controller, kubeClient := newGracePeriodTestController(t, replacementPod)
	controller.store = datastore.New()
	controller.workqueue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()) //nolint:staticcheck
	t.Cleanup(controller.workqueue.ShutDown)

	require.NoError(t, controller.handleErrorPod(ms, "test-model-0", failedPod))
	require.NoError(t, controller.handleErrorPod(ms, "test-model-0", replacementPod))

	_, failedPodTracked := controller.graceMap.Load(getPodGracePeriodKey(failedPod))
	_, replacementPodTracked := controller.graceMap.Load(getPodGracePeriodKey(replacementPod))
	require.True(t, failedPodTracked)
	require.True(t, replacementPodTracked)

	require.Eventually(t, func() bool {
		for _, action := range kubeClient.Actions() {
			deleteAction, ok := action.(kubetesting.DeleteAction)
			if !ok || !action.Matches("delete", "pods") {
				continue
			}
			preconditions := deleteAction.GetDeleteOptions().Preconditions
			if preconditions != nil && preconditions.UID != nil && *preconditions.UID == replacementPod.UID {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)
}

func newGracePeriodTestController(t *testing.T, pod *corev1.Pod) (*ModelServingController, *kubefake.Clientset) {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(pod.DeepCopy()))

	kubeClient := kubefake.NewSimpleClientset(pod.DeepCopy())
	controller := &ModelServingController{
		kubeClientSet: kubeClient,
		podsLister:    corelisters.NewPodLister(indexer),
	}
	return controller, kubeClient
}
