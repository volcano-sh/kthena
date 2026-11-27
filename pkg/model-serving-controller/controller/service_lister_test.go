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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

type serviceListerTestPlugin struct {
	plugins.BasePlugin
	name string

	mu                  sync.Mutex
	observedServiceName string
	observedServiceOK   bool
}

func (p *serviceListerTestPlugin) Name() string { return p.name }

func (p *serviceListerTestPlugin) OnModelServingSync(_ context.Context, req *plugins.HookRequest) plugins.HookResult {
	p.mu.Lock()
	svcName := p.observedServiceName
	p.mu.Unlock()
	if svcName != "" && req.ServiceLister != nil {
		svc, err := req.ServiceLister.Services(req.ModelServing.Namespace).Get(svcName)
		p.mu.Lock()
		p.observedServiceOK = err == nil && svc != nil && svc.Name == svcName
		p.mu.Unlock()
	}
	return plugins.HookResult{}
}

func TestModelServingLevelServiceListerAndEventFilter(t *testing.T) {
	const (
		pluginName = "service-lister-test-plugin"
		msSvcName  = "svc-lister-ms-service"
	)

	ms := createStandardModelServing("svc-lister-ms", 1, 1)
	ms.UID = types.UID("svc-lister-ms-uid")
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{
		{Name: pluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
	}

	h := newTestController(t, ms)
	plugin := &serviceListerTestPlugin{
		name:                pluginName,
		observedServiceName: msSvcName,
	}
	registry := plugins.NewRegistry()
	registry.Register(pluginName, func(spec workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
		return plugin, nil
	})
	h.controller.pluginsRegistry = registry

	ownerRef := metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
		Kind:       workloadv1alpha1.ModelServingKind.Kind,
		Name:       ms.Name,
		UID:        ms.UID,
		Controller: ptr.To(true),
	}

	// Create a ModelServing-level Service without GroupNameLabelKey.
	msSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      msSvcName,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Name: "http", Port: 8000}},
		},
	}
	_, err := h.kubeClient.CoreV1().Services(ms.Namespace).Create(h.ctx, msSvc, metav1.CreateOptions{})
	require.NoError(t, err)

	// Verify servicesLister caches the ModelServing-level Service despite missing group-name.
	require.Eventually(t, func() bool {
		svc, getErr := h.controller.servicesLister.Services(ms.Namespace).Get(msSvcName)
		return getErr == nil && svc != nil && svc.Name == msSvcName
	}, 5*time.Second, 10*time.Millisecond, "servicesLister must cache ModelServing-level Services")

	drainWorkqueue(t, h.controller.workqueue)

	// Verify OnModelServingSync receives the ModelServing-level Service through req.ServiceLister.
	require.NoError(t, h.controller.syncModelServing(h.ctx, namespacedKey(ms.Namespace, ms.Name)))
	plugin.mu.Lock()
	observed := plugin.observedServiceOK
	plugin.mu.Unlock()
	assert.True(t, observed, "OnModelServingSync should see ModelServing-level Service via ServiceLister")

	require.Eventually(t, func() bool {
		_, getErr := h.controller.podsLister.Pods(ms.Namespace).Get(fmt.Sprintf("%s-0-prefill-0-0", ms.Name))
		return getErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	drainWorkqueue(t, h.controller.workqueue)

	// Updating and deleting the ModelServing-level Service (without group-name) must NOT enqueue ModelServing.
	updatedSvc := msSvc.DeepCopy()
	updatedSvc.Annotations = map[string]string{"updated": "true"}
	_, err = h.kubeClient.CoreV1().Services(ms.Namespace).Update(h.ctx, updatedSvc, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.kubeClient.CoreV1().Services(ms.Namespace).Delete(h.ctx, msSvcName, metav1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		_, getErr := h.controller.servicesLister.Services(ms.Namespace).Get(msSvcName)
		return getErr != nil
	}, 5*time.Second, 10*time.Millisecond, "deleted ModelServing-level Service should be removed from cache")

	assert.Never(t, func() bool {
		return h.controller.workqueue.Len() > 0
	}, 100*time.Millisecond, 10*time.Millisecond, "ModelServing-level Service update/delete must not enqueue ModelServing")

	// Deleting a group-scoped Service (with group-name) must still enqueue ModelServing.
	groupSvcName := fmt.Sprintf("%s-0-prefill-0", ms.Name)
	groupSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      groupSvcName,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey:        fmt.Sprintf("%s-0", ms.Name),
				workloadv1alpha1.RoleLabelKey:             "prefill",
				workloadv1alpha1.RoleIDKey:                "prefill-0",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	_, err = h.kubeClient.CoreV1().Services(ms.Namespace).Create(h.ctx, groupSvc, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, getErr := h.controller.servicesLister.Services(ms.Namespace).Get(groupSvcName)
		return getErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	drainWorkqueue(t, h.controller.workqueue)

	require.NoError(t, h.kubeClient.CoreV1().Services(ms.Namespace).Delete(h.ctx, groupSvcName, metav1.DeleteOptions{}))
	h.expectQueuedKey(namespacedKey(ms.Namespace, ms.Name))
}
