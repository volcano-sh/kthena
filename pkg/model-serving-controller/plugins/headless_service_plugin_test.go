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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
		Pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-ms-0-prefill-0-0"}, Spec: corev1.PodSpec{
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
	assert.Equal(t, req.Pod.Name, req.Pod.Spec.Hostname)
	assert.Equal(t, DiscoveryServiceName(req.Pod.Name), req.Pod.Spec.Subdomain)

	workerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms-0-prefill-0-1"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
	}
	require.NoError(t, plugin.OnPodCreate(ctx, &HookRequest{
		ModelServing: ms,
		ServingGroup: req.ServingGroup,
		RoleName:     req.RoleName,
		RoleID:       req.RoleID,
		Role:         role,
		Pod:          workerPod,
		KubeClient:   kubeClient,
	}))
	assert.Equal(t, entryAddress, envMap(workerPod.Spec.Containers[0].Env)[workloadv1alpha1.EntryAddressEnv])
	assert.Equal(t, workerPod.Name, workerPod.Spec.Hostname)
	assert.Equal(t, DiscoveryServiceName(req.Pod.Name), workerPod.Spec.Subdomain)

	serviceName := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	service, err := kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, service.Spec.ClusterIP)
	assert.True(t, service.Spec.PublishNotReadyAddresses)
	assert.Equal(t, req.ServingGroup, service.Spec.Selector[workloadv1alpha1.GroupNameLabelKey])
	assert.Equal(t, req.RoleName, service.Spec.Selector[workloadv1alpha1.RoleLabelKey])
	assert.Equal(t, req.RoleID, service.Spec.Selector[workloadv1alpha1.RoleIDKey])
	assert.Equal(t, "true", service.Spec.Selector[workloadv1alpha1.EntryLabelKey])
	assert.Equal(t, HeadlessServicePluginName, service.Labels[HeadlessServicePluginLabelKey])
	assert.True(t, utils.IsOwnedByModelServingWithUID(service, ms.UID))
	discovery, err := kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, DiscoveryServiceName(serviceName), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, discovery.Spec.ClusterIP)
	assert.True(t, discovery.Spec.PublishNotReadyAddresses)
	assert.NotContains(t, discovery.Spec.Selector, workloadv1alpha1.EntryLabelKey)
	assert.Equal(t, req.RoleID, discovery.Spec.Selector[workloadv1alpha1.RoleIDKey])

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
	_, err = kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, DiscoveryServiceName(serviceName), metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func envMap(envVars []corev1.EnvVar) map[string]string {
	env := make(map[string]string, len(envVars))
	for _, item := range envVars {
		env[item.Name] = item.Value
	}
	return env
}

func TestHeadlessServicePluginSetsPodDNSWithZeroWorkerReplicas(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{
		Name: "test-ms", Namespace: "default", UID: types.UID("test-ms-uid"),
	}}
	role := &workloadv1alpha1.Role{
		Name:           "prefill",
		WorkerReplicas: 0,
		WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-ms-0-prefill-0-0"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "entry"}}}}
	kubeClient := kubefake.NewSimpleClientset()
	plugin, err := NewHeadlessServicePlugin(
		workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
	)
	require.NoError(t, err)

	require.NoError(t, plugin.OnPodCreate(ctx, &HookRequest{
		ModelServing: ms,
		ServingGroup: "test-ms-0",
		RoleName:     role.Name,
		RoleID:       "prefill-0",
		Role:         role,
		IsEntry:      true,
		Pod:          pod,
		KubeClient:   kubeClient,
	}))
	assert.Equal(t, pod.Name, pod.Spec.Hostname)
	assert.Equal(t, DiscoveryServiceName(pod.Name), pod.Spec.Subdomain)
	assert.Equal(t, pod.Name+".default", envMap(pod.Spec.Containers[0].Env)[workloadv1alpha1.EntryAddressEnv])
	_, err = kubeClient.CoreV1().Services(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestHeadlessServicePluginSkipsUnsupportedRoles(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-ms-0-prefill-0-0"}, Spec: corev1.PodSpec{Hostname: "user-host", Subdomain: "user-domain", Containers: []corev1.Container{{Name: "entry", Env: []corev1.EnvVar{{Name: workloadv1alpha1.EntryAddressEnv, Value: "user-entry"}}}}}}
	original := pod.DeepCopy()
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
		Pod:          pod,
	}))
	assert.Equal(t, original, pod)
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

func headlessTestRequest() *HookRequest {
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: "ms-uid"}}
	return &HookRequest{ModelServing: ms, ServingGroup: "test-ms-0", RoleName: "serve", RoleID: "serve-0", Role: &workloadv1alpha1.Role{Name: "serve", WorkerReplicas: 1, WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{}}, Cleanup: &RoleCleanup{}}
}

func headlessTestService(req *HookRequest) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: utils.GeneratePodName(req.ServingGroup, req.RoleID, 0), Namespace: req.ModelServing.Namespace, UID: "service-old", ResourceVersion: "1",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(req.ModelServing, workloadv1alpha1.ModelServingKind)},
		Labels:          map[string]string{workloadv1alpha1.ModelServingNameLabelKey: req.ModelServing.Name, workloadv1alpha1.GroupNameLabelKey: req.ServingGroup, workloadv1alpha1.RoleLabelKey: req.RoleName, workloadv1alpha1.RoleIDKey: req.RoleID, HeadlessServicePluginLabelKey: HeadlessServicePluginName},
	}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone}}
}

func TestDiscoveryServiceNameBoundaries(t *testing.T) {
	names := []string{"test-ms-0-serve-0-0", strings.Repeat("a", 53), strings.Repeat("a", 54), strings.Repeat("a", 63), strings.Repeat("b", 63)}
	seen := map[string]bool{}
	for _, entry := range names {
		name := DiscoveryServiceName(entry)
		assert.Empty(t, validation.IsDNS1035Label(name))
		assert.NotEqual(t, entry, name)
		assert.Equal(t, name, DiscoveryServiceName(entry))
		assert.False(t, seen[name])
		seen[name] = true
	}
}

func TestHeadlessLegacyWorkerSubdomainBlocksBeforeServiceWrites(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing service %t", existing), func(t *testing.T) {
			req := headlessTestRequest()
			oldService := headlessTestService(req)
			oldService.Spec.Selector = roleSelector(req)
			oldWorker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-worker", Namespace: req.ModelServing.Namespace, UID: "worker-old", Labels: roleSelector(req), OwnerReferences: oldService.OwnerReferences}, Spec: corev1.PodSpec{Hostname: "old-worker", Subdomain: oldService.Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			objects := []runtime.Object{oldWorker}
			if existing {
				objects = append(objects, oldService)
			}
			client := kubefake.NewSimpleClientset(objects...)
			req.KubeClient = client
			req.Pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-worker"}}
			plugin, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
			require.NoError(t, err)
			require.ErrorContains(t, plugin.OnRoleSync(context.Background(), req), "MigrationRequired")
			require.ErrorContains(t, plugin.OnPodCreate(context.Background(), req), "MigrationRequired")
			assert.Empty(t, req.Pod.Spec.Subdomain)
			assert.Empty(t, req.Pod.Spec.Hostname)
			for _, action := range client.Actions() {
				assert.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
			}
			current, err := client.CoreV1().Pods(req.ModelServing.Namespace).Get(context.Background(), oldWorker.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, oldWorker, current)
		})
	}
}

func TestHeadlessServiceAdoptsLegacyShapeInPlace(t *testing.T) {
	req := headlessTestRequest()
	service := headlessTestService(req)
	delete(service.Labels, HeadlessServicePluginLabelKey)
	service.Labels["user"] = "preserved"
	service.Annotations = map[string]string{"user": "preserved"}
	service.Spec.Selector = roleSelector(req)
	client := kubefake.NewSimpleClientset(service)
	req.KubeClient = client
	plugin, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
	require.NoError(t, err)
	require.NoError(t, plugin.OnRoleSync(context.Background(), req))
	current, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, service.UID, current.UID)
	assert.Equal(t, service.ResourceVersion, current.ResourceVersion)
	assert.Equal(t, service.Annotations, current.Annotations)
	assert.Equal(t, "preserved", current.Labels["user"])
	assert.Equal(t, "true", current.Spec.Selector[workloadv1alpha1.EntryLabelKey])
	assert.True(t, current.Spec.PublishNotReadyAddresses)
	client.ClearActions()
	require.NoError(t, plugin.OnRoleSync(context.Background(), req))
	for _, action := range client.Actions() {
		assert.NotContains(t, []string{"create", "update", "patch", "delete"}, action.GetVerb())
	}
}

func TestHeadlessServiceStrictIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Service)
	}{
		{"same owner different role", func(s *corev1.Service) { s.Labels[workloadv1alpha1.RoleLabelKey] = "other" }},
		{"missing role identity", func(s *corev1.Service) { delete(s.Labels, workloadv1alpha1.RoleIDKey) }},
		{"not headless", func(s *corev1.Service) { s.Spec.ClusterIP = "10.0.0.1" }},
		{"wrong MS name", func(s *corev1.Service) { s.Labels[workloadv1alpha1.ModelServingNameLabelKey] = "other" }},
		{"noncontroller owner", func(s *corev1.Service) { s.OwnerReferences[0].Controller = ptr.To(false) }},
		{"stale owner UID", func(s *corev1.Service) { s.OwnerReferences[0].UID = "previous" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := headlessTestRequest()
			service := headlessTestService(req)
			tc.change(service)
			client := kubefake.NewSimpleClientset(service)
			req.KubeClient = client
			plugin, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
			require.NoError(t, err)
			require.ErrorContains(t, plugin.OnRoleSync(context.Background(), req), "occupied")
			require.NoError(t, plugin.OnRoleDelete(context.Background(), req))
			current, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, service, current)
			_, _, _, ok := ManagedHeadlessServiceIdentity(service, req.ModelServing)
			assert.False(t, ok)
		})
	}
}

func TestHeadlessRoleCleanupRetainsUIDAcrossFailedDelete(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement %t", replace), func(t *testing.T) {
			req := headlessTestRequest()
			service := headlessTestService(req)
			client := kubefake.NewSimpleClientset(service)
			req.KubeClient = client
			resource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}
			deletes := 0
			client.PrependReactor("delete", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
				deletes++
				options := action.(k8stesting.DeleteAction).GetDeleteOptions()
				require.NotNil(t, options.Preconditions)
				require.NotNil(t, options.Preconditions.UID)
				assert.Equal(t, service.UID, *options.Preconditions.UID)
				if deletes == 1 {
					if replace {
						replacement := service.DeepCopy()
						replacement.UID = "service-replacement"
						require.NoError(t, client.Tracker().Update(resource, replacement, service.Namespace))
						return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "services"}, service.Name, fmt.Errorf("UID changed"))
					}
					return true, nil, apierrors.NewServiceUnavailable("temporary failure")
				}
				return false, nil, nil
			})
			plugin, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
			require.NoError(t, err)
			require.Error(t, plugin.OnRoleDelete(context.Background(), req))
			require.NoError(t, plugin.OnRoleDelete(context.Background(), req))
			current, err := client.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
			if replace {
				require.NoError(t, err)
				assert.Equal(t, types.UID("service-replacement"), current.UID)
				assert.Equal(t, 1, deletes, "retry must not adopt the replacement UID")
			} else {
				assert.True(t, apierrors.IsNotFound(err))
				assert.Equal(t, 2, deletes)
			}
		})
	}
}

func TestHeadlessServiceRestoresMissingDiscoveryWithoutChangingEntry(t *testing.T) {
	req := headlessTestRequest()
	client := kubefake.NewSimpleClientset()
	req.KubeClient = client
	plugin, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
	require.NoError(t, err)
	require.NoError(t, plugin.OnRoleSync(context.Background(), req))
	entryName := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	entry, err := client.CoreV1().Services(req.ModelServing.Namespace).Get(context.Background(), entryName, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, client.CoreV1().Services(req.ModelServing.Namespace).Delete(context.Background(), DiscoveryServiceName(entryName), metav1.DeleteOptions{}))
	client.ClearActions()
	require.NoError(t, plugin.OnRoleSync(context.Background(), req))
	discovery, err := client.CoreV1().Services(req.ModelServing.Namespace).Get(context.Background(), DiscoveryServiceName(entryName), metav1.GetOptions{})
	require.NoError(t, err)
	assert.True(t, discovery.Spec.PublishNotReadyAddresses)
	current, err := client.CoreV1().Services(req.ModelServing.Namespace).Get(context.Background(), entryName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, entry, current)
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "pods" {
			assert.Equal(t, "list", action.GetVerb(), "restoring discovery must not modify Pods")
		}
	}
}
