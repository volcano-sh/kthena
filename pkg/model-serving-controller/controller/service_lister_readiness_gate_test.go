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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const testReadinessGateConditionType corev1.PodConditionType = "test.kthena.volcano.sh/role-ready"

type readinessGateTestPlugin struct {
	plugins.BasePlugin
	name string

	mu                  sync.Mutex
	observedServiceName string
	observedServiceOK   bool
}

func (p *readinessGateTestPlugin) Name() string { return p.name }

func (p *readinessGateTestPlugin) OnModelServingSync(_ context.Context, req *plugins.HookRequest) plugins.HookResult {
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

func (p *readinessGateTestPlugin) OnPodCreate(_ context.Context, req *plugins.HookRequest) error {
	if !req.IsEntry {
		return nil
	}
	req.Pod.Spec.ReadinessGates = append(req.Pod.Spec.ReadinessGates, corev1.PodReadinessGate{
		ConditionType: testReadinessGateConditionType,
	})
	return nil
}

func (p *readinessGateTestPlugin) OnRoleSync(ctx context.Context, req *plugins.HookRequest) error {
	selector := labels.SelectorFromSet(labels.Set{
		workloadv1alpha1.GroupNameLabelKey: req.ServingGroup,
		workloadv1alpha1.RoleLabelKey:      req.RoleName,
		workloadv1alpha1.RoleIDKey:         req.RoleID,
	})
	podList, err := req.KubeClient.CoreV1().Pods(req.ModelServing.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return err
	}

	var entryPod *corev1.Pod
	expectedPods := 1
	if req.Role != nil {
		expectedPods += int(req.Role.WorkerReplicas)
	}
	readyPods := 0
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry {
			entryPod = pod
			if pod.Status.Phase == corev1.PodRunning && utils.IsPodContainersReady(pod) {
				readyPods++
			}
			continue
		}
		if utils.IsPodRunningAndReady(pod) {
			readyPods++
		}
	}
	if entryPod == nil {
		return nil
	}

	desired := corev1.ConditionFalse
	if readyPods == expectedPods {
		desired = corev1.ConditionTrue
	}

	updated := entryPod.DeepCopy()
	setPodCondition(&updated.Status.Conditions, corev1.PodCondition{
		Type:   testReadinessGateConditionType,
		Status: desired,
	})
	_, err = req.KubeClient.CoreV1().Pods(updated.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	return err
}

func setPodCondition(conditions *[]corev1.PodCondition, cond corev1.PodCondition) {
	for i := range *conditions {
		if (*conditions)[i].Type == cond.Type {
			(*conditions)[i].Status = cond.Status
			return
		}
	}
	*conditions = append(*conditions, cond)
}

func getPodConditionStatus(pod *corev1.Pod, condType corev1.PodConditionType) (corev1.ConditionStatus, bool) {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == condType {
			return pod.Status.Conditions[i].Status, true
		}
	}
	return "", false
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
	plugin := &readinessGateTestPlugin{
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

func TestMultiNodeReadinessGateUpdatePodTransitions(t *testing.T) {
	const pluginName = "readiness-gate-test-plugin"

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "rg-ms",
			UID:       types.UID("rg-ms-uid"),
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](1),
			SchedulerName: "volcano",
			Plugins: []workloadv1alpha1.PluginSpec{
				{Name: pluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:           "decode",
						Replicas:       ptr.To[int32](1),
						WorkerReplicas: 1,
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "entry", Image: "test-image:latest"}},
							},
						},
						WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "worker", Image: "test-image:latest"}},
							},
						},
					},
				},
			},
		},
	}

	h := newTestController(t, ms)
	plugin := &readinessGateTestPlugin{name: pluginName}
	registry := plugins.NewRegistry()
	registry.Register(pluginName, func(spec workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
		return plugin, nil
	})
	h.controller.pluginsRegistry = registry

	msKey := namespacedKey(ms.Namespace, ms.Name)
	drainWorkqueue(t, h.controller.workqueue)

	// Reconcile once to create the gated entry Pod and worker Pod.
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPodName := "rg-ms-0-decode-0-0"
	workerPodName := "rg-ms-0-decode-0-1"

	require.Eventually(t, func() bool {
		entry, err1 := h.controller.podsLister.Pods(ms.Namespace).Get(entryPodName)
		worker, err2 := h.controller.podsLister.Pods(ms.Namespace).Get(workerPodName)
		return err1 == nil && err2 == nil && entry != nil && worker != nil
	}, 5*time.Second, 10*time.Millisecond, "entry and worker pods should be cached in podsLister")

	entryPod, err := h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, []corev1.PodReadinessGate{{ConditionType: testReadinessGateConditionType}}, entryPod.Spec.ReadinessGates)

	// Step 1: Worker becomes Ready BEFORE the gated entry Pod reaches ContainersReady=True.
	workerPod, err := h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, workerPodName, metav1.GetOptions{})
	require.NoError(t, err)
	workerPod.Status.Phase = corev1.PodRunning
	workerPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, workerPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.expectQueuedKey(msKey)
	drainWorkqueue(t, h.controller.workqueue)

	// Step 2: Gated entry Pod reaches ContainersReady=True while PodReady remains False.
	// updatePod must enqueue ModelServing so OnRoleSync sets the readiness gate condition True.
	entryPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	entryPod.Status.Phase = corev1.PodRunning
	entryPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, entryPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.expectQueuedKey(msKey)
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	gateStatus, found := getPodConditionStatus(entryPod, testReadinessGateConditionType)
	require.True(t, found, "entry pod should have readiness gate condition set")
	assert.Equal(t, corev1.ConditionTrue, gateStatus, "OnRoleSync should set readiness gate condition True once worker is ready and entry containers are ready")

	// Simulate kubelet marking the entry Pod Ready after the readiness gate becomes True.
	setPodCondition(&entryPod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	})
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, entryPod, metav1.UpdateOptions{})
	require.NoError(t, err)
	// Wait for handleReadyPod to process the Ready entry Pod and emit its ServingGroupRunning enqueue
	// before draining, so the next expectQueuedKey strictly proves the worker drop enqueued msKey.
	h.expectQueuedKey(msKey)
	drainWorkqueue(t, h.controller.workqueue)

	// Step 3: Running worker drops ContainersReady/PodReady to False.
	// updatePod must enqueue ModelServing so OnRoleSync flips the readiness gate back to False.
	workerPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, workerPodName, metav1.GetOptions{})
	require.NoError(t, err)
	workerPod.Status.Phase = corev1.PodRunning
	workerPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionFalse},
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, workerPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.expectQueuedKey(msKey)
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	gateStatus, found = getPodConditionStatus(entryPod, testReadinessGateConditionType)
	require.True(t, found, "entry pod should retain readiness gate condition")
	assert.Equal(t, corev1.ConditionFalse, gateStatus, "OnRoleSync should flip readiness gate condition back to False when worker becomes NotReady")

	// Simulate kubelet marking the entry Pod NotReady after the readiness gate flips to False.
	setPodCondition(&entryPod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodReady,
		Status: corev1.ConditionFalse,
	})
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, entryPod, metav1.UpdateOptions{})
	require.NoError(t, err)
	h.expectQueuedKey(msKey)
	drainWorkqueue(t, h.controller.workqueue)

	// Step 4: Worker recovers to Running+Ready while entry Pod still has PodReady=False.
	// updatePod must enqueue ModelServing so OnRoleSync flips the readiness gate back to True.
	workerPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, workerPodName, metav1.GetOptions{})
	require.NoError(t, err)
	workerPod.Status.Phase = corev1.PodRunning
	workerPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, workerPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.expectQueuedKey(msKey)
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	gateStatus, found = getPodConditionStatus(entryPod, testReadinessGateConditionType)
	require.True(t, found, "entry pod should retain readiness gate condition after worker recovery")
	assert.Equal(t, corev1.ConditionTrue, gateStatus, "OnRoleSync should flip readiness gate back to True when worker recovers")

	// Step 5: Unrelated update to an already-Ready Pod must NOT enqueue ModelServing.
	require.Eventually(t, func() bool {
		p, getErr := h.controller.podsLister.Pods(ms.Namespace).Get(entryPodName)
		if getErr != nil {
			return false
		}
		st, ok := getPodConditionStatus(p, testReadinessGateConditionType)
		return ok && st == corev1.ConditionTrue
	}, 5*time.Second, 10*time.Millisecond)
	drainWorkqueue(t, h.controller.workqueue)

	workerPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, workerPodName, metav1.GetOptions{})
	require.NoError(t, err)
	if workerPod.Annotations == nil {
		workerPod.Annotations = map[string]string{}
	}
	workerPod.Annotations["test.kthena.volcano.sh/unrelated"] = "noop"
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Update(h.ctx, workerPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		p, getErr := h.controller.podsLister.Pods(ms.Namespace).Get(workerPodName)
		return getErr == nil && p.Annotations["test.kthena.volcano.sh/unrelated"] == "noop"
	}, 5*time.Second, 10*time.Millisecond)

	assert.Never(t, func() bool {
		return h.controller.workqueue.Len() > 0
	}, 100*time.Millisecond, 10*time.Millisecond, "unrelated update to already-Ready Pod must not enqueue ModelServing")
}

func TestMultiNodeReadinessGateEntryFirstStartup(t *testing.T) {
	const pluginName = "readiness-gate-entry-first-plugin"

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "rg-entry-first-ms",
			UID:       types.UID("rg-entry-first-ms-uid"),
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](1),
			SchedulerName: "volcano",
			Plugins: []workloadv1alpha1.PluginSpec{
				{Name: pluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:           "decode",
						Replicas:       ptr.To[int32](1),
						WorkerReplicas: 1,
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "entry", Image: "test-image:latest"}},
							},
						},
						WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "worker", Image: "test-image:latest"}},
							},
						},
					},
				},
			},
		},
	}

	h := newTestController(t, ms)
	plugin := &readinessGateTestPlugin{name: pluginName}
	registry := plugins.NewRegistry()
	registry.Register(pluginName, func(spec workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
		return plugin, nil
	})
	h.controller.pluginsRegistry = registry

	msKey := namespacedKey(ms.Namespace, ms.Name)
	drainWorkqueue(t, h.controller.workqueue)

	// Reconcile once to create the gated entry Pod and worker Pod.
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPodName := "rg-entry-first-ms-0-decode-0-0"
	workerPodName := "rg-entry-first-ms-0-decode-0-1"

	require.Eventually(t, func() bool {
		entry, err1 := h.controller.podsLister.Pods(ms.Namespace).Get(entryPodName)
		worker, err2 := h.controller.podsLister.Pods(ms.Namespace).Get(workerPodName)
		return err1 == nil && err2 == nil && entry != nil && worker != nil
	}, 5*time.Second, 10*time.Millisecond)
	drainWorkqueue(t, h.controller.workqueue)

	// Step 1: Entry Pod reaches ContainersReady=True BEFORE worker is Ready.
	entryPod, err := h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	entryPod.Status.Phase = corev1.PodRunning
	entryPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, entryPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.expectQueuedKey(msKey)
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	gateStatus, found := getPodConditionStatus(entryPod, testReadinessGateConditionType)
	require.True(t, found)
	assert.Equal(t, corev1.ConditionFalse, gateStatus, "readiness gate must remain False while worker is unready")

	require.Eventually(t, func() bool {
		p, getErr := h.controller.podsLister.Pods(ms.Namespace).Get(entryPodName)
		if getErr != nil {
			return false
		}
		st, ok := getPodConditionStatus(p, testReadinessGateConditionType)
		return ok && st == corev1.ConditionFalse
	}, 5*time.Second, 10*time.Millisecond)
	drainWorkqueue(t, h.controller.workqueue)

	// Step 2: Worker later transitions to Running+Ready.
	// updatePod must enqueue ModelServing even though entry Pod still has PodReady=False,
	// so OnRoleSync runs and flips the entry Pod's readiness gate to True.
	workerPod, err := h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, workerPodName, metav1.GetOptions{})
	require.NoError(t, err)
	workerPod.Status.Phase = corev1.PodRunning
	workerPod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.ContainersReady, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	_, err = h.kubeClient.CoreV1().Pods(ms.Namespace).UpdateStatus(h.ctx, workerPod, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.expectQueuedKey(msKey)
	require.NoError(t, h.controller.syncModelServing(h.ctx, msKey))

	entryPod, err = h.kubeClient.CoreV1().Pods(ms.Namespace).Get(h.ctx, entryPodName, metav1.GetOptions{})
	require.NoError(t, err)
	gateStatus, found = getPodConditionStatus(entryPod, testReadinessGateConditionType)
	require.True(t, found)
	assert.Equal(t, corev1.ConditionTrue, gateStatus, "OnRoleSync should flip readiness gate to True once worker becomes Ready")
}

func TestShouldEnqueueOnPodReadinessChange(t *testing.T) {
	podWith := func(phase corev1.PodPhase, gated bool, containersReady, podReady corev1.ConditionStatus) *corev1.Pod {
		pod := &corev1.Pod{
			Status: corev1.PodStatus{
				Phase: phase,
			},
		}
		if gated {
			pod.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: testReadinessGateConditionType}}
		}
		if containersReady != "" {
			pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
				Type:   corev1.ContainersReady,
				Status: containersReady,
			})
		}
		if podReady != "" {
			pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: podReady,
			})
		}
		return pod
	}

	tests := []struct {
		name   string
		oldPod *corev1.Pod
		newPod *corev1.Pod
		want   bool
	}{
		{
			name:   "nil newPod returns false",
			oldPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			newPod: nil,
			want:   false,
		},
		{
			name:   "add running ready pod enqueues",
			oldPod: nil,
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			want:   true,
		},
		{
			name:   "add gated running pod with ContainersReady=True enqueues",
			oldPod: nil,
			newPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionFalse),
			want:   true,
		},
		{
			name:   "add gated pending pod with ContainersReady=True does not enqueue",
			oldPod: nil,
			newPod: podWith(corev1.PodPending, true, corev1.ConditionTrue, corev1.ConditionFalse),
			want:   false,
		},
		{
			name:   "add gated running pod with ContainersReady=False does not enqueue",
			oldPod: nil,
			newPod: podWith(corev1.PodRunning, true, corev1.ConditionFalse, corev1.ConditionFalse),
			want:   false,
		},
		{
			name:   "add ungated running pod with ContainersReady=True does not enqueue",
			oldPod: nil,
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionFalse),
			want:   false,
		},
		{
			name:   "update ContainersReady False to True enqueues",
			oldPod: podWith(corev1.PodRunning, true, corev1.ConditionFalse, corev1.ConditionFalse),
			newPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionFalse),
			want:   true,
		},
		{
			name:   "update ContainersReady True to False enqueues",
			oldPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionFalse),
			newPod: podWith(corev1.PodRunning, true, corev1.ConditionFalse, corev1.ConditionFalse),
			want:   true,
		},
		{
			name:   "update running ready pod dropping PodReady to False while ContainersReady stays True enqueues",
			oldPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionFalse),
			want:   true,
		},
		{
			name:   "update running ready pod dropping both ContainersReady and PodReady to False enqueues",
			oldPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionFalse, corev1.ConditionFalse),
			want:   true,
		},
		{
			name:   "update unready pod entering Running+Ready enqueues",
			oldPod: podWith(corev1.PodRunning, false, corev1.ConditionFalse, corev1.ConditionFalse),
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			want:   true,
		},
		{
			name:   "update gated pod entering Running+Ready with ContainersReady already True enqueues",
			oldPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionFalse),
			newPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionTrue),
			want:   true,
		},
		{
			name:   "update unchanged Running+Ready pod does not enqueue",
			oldPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionTrue, corev1.ConditionTrue),
			want:   false,
		},
		{
			name:   "update gated pod with steady ContainersReady=True and PodReady=False does not re-enqueue",
			oldPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionFalse),
			newPod: podWith(corev1.PodRunning, true, corev1.ConditionTrue, corev1.ConditionFalse),
			want:   false,
		},
		{
			name:   "update pending pod staying unready does not enqueue",
			oldPod: podWith(corev1.PodPending, false, corev1.ConditionFalse, corev1.ConditionFalse),
			newPod: podWith(corev1.PodRunning, false, corev1.ConditionFalse, corev1.ConditionFalse),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldEnqueueOnPodReadinessChange(tt.oldPod, tt.newPod))
		})
	}
}
