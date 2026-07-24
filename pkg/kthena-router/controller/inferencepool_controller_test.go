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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

func TestInferencePoolControllerSyncRestoresPodAfterModelServerRemoval(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	dynamicClient := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme())
	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	store := newStoreWithMockBackend()
	controller, err := NewInferencePoolController(dynamicInformerFactory, kubeInformerFactory, store)
	require.NoError(t, err)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pool-pod",
			Labels: map[string]string{
				"pool": "chat",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model1",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
		},
	}
	require.NoError(t, store.AddOrUpdateModelServer(ms, nil))
	require.NoError(t, store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms}))
	require.NotNil(t, store.GetPodInfo(utils.GetNamespaceName(pod)))
	require.NoError(t, store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{}))
	require.Nil(t, store.GetPodInfo(utils.GetNamespaceName(pod)))

	podIndexer := kubeInformerFactory.Core().V1().Pods().Informer().GetIndexer()
	require.NoError(t, podIndexer.Add(pod))

	inferencePool := &inferencev1.InferencePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pool1",
		},
		Spec: inferencev1.InferencePoolSpec{
			Selector: inferencev1.LabelSelector{
				MatchLabels: map[inferencev1.LabelKey]inferencev1.LabelValue{
					"pool": "chat",
				},
			},
			TargetPorts: []inferencev1.Port{{Number: 9000}},
		},
	}
	obj, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(inferencePool)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(inferencev1.SchemeGroupVersion.WithKind("InferencePool"))
	poolInformer := dynamicInformerFactory.ForResource(inferencev1.SchemeGroupVersion.WithResource("inferencepools")).Informer()
	require.NoError(t, poolInformer.GetIndexer().Add(u))

	require.NoError(t, controller.syncHandler("default/pool1"))
	pods, err := store.GetPodsByInferencePool(utils.GetNamespaceName(inferencePool))
	require.NoError(t, err)
	require.Len(t, pods, 1)
	assert.Equal(t, "pool-pod", pods[0].GetPod().Name)
}
