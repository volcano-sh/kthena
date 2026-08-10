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

package plugins

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestHeadlessServicePluginLifecycle(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       types.UID("test-ms-uid"),
		},
	}
	role := &workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       ptr.To[int32](1),
		WorkerReplicas: 1,
		WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{},
	}
	kubeClient := kubefake.NewSimpleClientset()
	serviceInformer := informers.NewSharedInformerFactory(kubeClient, 0).Core().V1().Services()
	plugin, err := NewHeadlessServicePlugin(
		workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
	)
	require.NoError(t, err)

	req := &HookRequest{
		ModelServing: ms,
		ServingGroup: "test-ms-0",
		RoleName:     role.Name,
		RoleID:       "prefill-0",
		Role:         role,
		IsEntry:      true,
		Pod: &corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "entry", Env: []corev1.EnvVar{{
				Name: workloadv1alpha1.EntryAddressEnv, Value: "user-value",
			}}}},
			InitContainers: []corev1.Container{{Name: "entry-init"}},
		}},
		KubeClient:    kubeClient,
		ServiceLister: serviceInformer.Lister(),
	}
	require.NoError(t, plugin.OnPodCreate(ctx, req))
	entryAddress := "test-ms-0-prefill-0-0.default"
	assert.Equal(t, entryAddress, envMap(req.Pod.Spec.Containers[0].Env)[workloadv1alpha1.EntryAddressEnv])
	assert.Equal(t, entryAddress, envMap(req.Pod.Spec.InitContainers[0].Env)[workloadv1alpha1.EntryAddressEnv])

	workerPod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}
	require.NoError(t, plugin.OnPodCreate(ctx, &HookRequest{
		ModelServing: ms,
		ServingGroup: req.ServingGroup,
		RoleName:     req.RoleName,
		RoleID:       req.RoleID,
		Role:         role,
		Pod:          workerPod,
	}))
	assert.Equal(t, entryAddress, envMap(workerPod.Spec.Containers[0].Env)[workloadv1alpha1.EntryAddressEnv])

	serviceName := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	service, err := kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, service.Spec.ClusterIP)
	assert.True(t, service.Spec.PublishNotReadyAddresses)
	assert.Equal(t, utils.Entry, service.Spec.Selector[workloadv1alpha1.EntryLabelKey])
	assert.Equal(t, HeadlessServicePluginName, service.Labels[HeadlessServicePluginLabelKey])
	assert.True(t, utils.IsOwnedByModelServingWithUID(service, ms.UID))

	require.NoError(t, serviceInformer.Informer().GetIndexer().Add(service))
	require.NoError(t, plugin.OnRoleSync(ctx, &HookRequest{
		ModelServing:  ms,
		ServingGroup:  req.ServingGroup,
		RoleName:      req.RoleName,
		RoleID:        req.RoleID,
		RoleIndex:     0,
		Role:          role,
		KubeClient:    kubeClient,
		ServiceLister: serviceInformer.Lister(),
	}))

	require.NoError(t, plugin.OnRoleDelete(ctx, &HookRequest{
		ModelServing:  ms,
		ServingGroup:  req.ServingGroup,
		RoleName:      req.RoleName,
		RoleID:        req.RoleID,
		RoleIndex:     0,
		Role:          role,
		KubeClient:    kubeClient,
		ServiceLister: serviceInformer.Lister(),
	}))
	_, err = kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	assert.Error(t, err)
}

func envMap(envVars []corev1.EnvVar) map[string]string {
	env := make(map[string]string, len(envVars))
	for _, item := range envVars {
		env[item.Name] = item.Value
	}
	return env
}

func TestHeadlessServicePluginSkipsUnsupportedRoles(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	plugin, err := NewHeadlessServicePlugin(
		workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
	)
	require.NoError(t, err)

	require.NoError(t, plugin.OnPodCreate(context.Background(), &HookRequest{
		ModelServing: &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default"}},
		ServingGroup: "test-ms-0",
		RoleName:     "prefill",
		RoleID:       "prefill-0",
		Role:         &workloadv1alpha1.Role{Name: "prefill"},
		IsEntry:      true,
		KubeClient:   kubeClient,
	}))
	services, err := kubeClient.CoreV1().Services("default").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, services.Items)
}

func TestHeadlessServicePluginRequiresKubeClientAtHookTime(t *testing.T) {
	plugin, err := NewHeadlessServicePlugin(
		workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
	)
	require.NoError(t, err)

	err = plugin.OnRoleSync(context.Background(), &HookRequest{
		ModelServing: &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default"}},
		ServingGroup: "test-ms-0",
		RoleName:     "prefill",
		RoleID:       "prefill-0",
		Role:         &workloadv1alpha1.Role{WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{}},
	})
	require.ErrorContains(t, err, "Kubernetes client is not initialized")
}

func TestRoleDeleteChainCleansServiceAfterPluginRemoval(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: types.UID("test-ms-uid")},
	}
	serviceName := utils.GeneratePodName("test-ms-0", "prefill-0", 0)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      serviceName,
		Namespace: ms.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
			Kind:       workloadv1alpha1.ModelServingKind.Kind,
			Name:       ms.Name,
			UID:        ms.UID,
			Controller: ptr.To(true),
		}},
		Labels: map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			workloadv1alpha1.GroupNameLabelKey:        "test-ms-0",
			workloadv1alpha1.RoleLabelKey:             "prefill",
			workloadv1alpha1.RoleIDKey:                "prefill-0",
		},
	}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone}}
	kubeClient := kubefake.NewSimpleClientset(service)
	chain, err := NewRoleDeleteChain(DefaultRegistry, nil)
	require.NoError(t, err)
	require.NoError(t, chain.OnRoleDelete(ctx, &HookRequest{
		ModelServing: ms,
		ServingGroup: "test-ms-0",
		RoleName:     "prefill",
		RoleID:       "prefill-0",
		RoleIndex:    0,
		KubeClient:   kubeClient,
	}))
	_, err = kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	assert.Error(t, err)
}

func TestHeadlessServicePluginRejectsPodTargetScope(t *testing.T) {
	_, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{
		Name: HeadlessServicePluginName,
		Type: workloadv1alpha1.PluginTypeBuiltIn,
		Scope: &workloadv1alpha1.PluginScope{
			Target: workloadv1alpha1.PluginTargetWorker,
		},
	})
	require.Error(t, err)
}

func TestHeadlessServicePluginPreservesUnmanagedServices(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{
		Name: "test-ms", Namespace: "default", UID: types.UID("current-uid"),
	}}
	serviceName := utils.GeneratePodName("test-ms-0", "prefill-0", 0)
	tests := []struct {
		name    string
		service *corev1.Service
	}{
		{
			name: "user managed",
			service: &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: serviceName, Namespace: ms.Namespace,
			}},
		},
		{
			name: "stale ModelServing UID",
			service: &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: serviceName, Namespace: ms.Namespace,
				Labels: map[string]string{HeadlessServicePluginLabelKey: HeadlessServicePluginName},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
					Name: ms.Name, UID: types.UID("stale-uid"), Controller: ptr.To(true),
				}},
			}},
		},
		{
			name: "owned but not Headless Service",
			service: &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: serviceName, Namespace: ms.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
					Name: ms.Name, UID: ms.UID, Controller: ptr.To(true),
				}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset(tt.service.DeepCopy())
			plugin, err := NewHeadlessServicePlugin(
				workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
			)
			require.NoError(t, err)
			require.NoError(t, plugin.OnRoleDelete(ctx, &HookRequest{
				ModelServing: ms, ServingGroup: "test-ms-0", RoleName: "prefill", RoleID: "prefill-0", KubeClient: kubeClient,
			}))
			_, err = kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestHeadlessServicePluginRejectsUnmanagedNameCollision(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{
		Name: "test-ms", Namespace: "default", UID: types.UID("current-uid"),
	}}
	serviceName := utils.GeneratePodName("test-ms-0", "prefill-0", 0)
	kubeClient := kubefake.NewSimpleClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: serviceName, Namespace: ms.Namespace,
	}})
	serviceInformer := informers.NewSharedInformerFactory(kubeClient, 0).Core().V1().Services()
	plugin, err := NewHeadlessServicePlugin(
		workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
	)
	require.NoError(t, err)

	err = plugin.OnRoleSync(ctx, &HookRequest{
		ModelServing:  ms,
		ServingGroup:  "test-ms-0",
		RoleName:      "prefill",
		RoleID:        "prefill-0",
		RoleIndex:     0,
		Role:          &workloadv1alpha1.Role{WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{}},
		KubeClient:    kubeClient,
		ServiceLister: serviceInformer.Lister(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "occupied")
}
