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
	"crypto/sha256"
	"fmt"
	"maps"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	HeadlessServicePluginName     = "headless-service"
	HeadlessServicePluginLabelKey = "modelserving.volcano.sh/plugin"
)

// RoleCleanup retains only in-process auxiliary deletion targets. It never
// establishes workload membership and is discarded when the Role is gone.
// After restart, normal reconciliation independently identifies orphan resources.
type RoleCleanup struct {
	mu      sync.Mutex
	targets map[string]types.UID
	done    map[string]bool
}

type HeadlessServicePlugin struct{ name string }

func init() {
	DefaultRegistry.Register(HeadlessServicePluginName, NewHeadlessServicePlugin)
	DefaultRegistry.RegisterRoleDelete(HeadlessServicePluginName)
}

func NewHeadlessServicePlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	if spec.Scope != nil && spec.Scope.Target != "" && spec.Scope.Target != workloadv1alpha1.PluginTargetAll {
		return nil, fmt.Errorf("scope.target must be All or omitted")
	}
	return &HeadlessServicePlugin{name: spec.Name}, nil
}

func (p *HeadlessServicePlugin) Name() string { return p.name }

// DiscoveryServiceName preserves the entry Service name and bounds the new name
// to a DNS label even when the existing entry name already uses all 63 bytes.
func DiscoveryServiceName(entryName string) string {
	if len(entryName)+len("-discovery") <= 63 {
		return entryName + "-discovery"
	}
	sum := sha256.Sum256([]byte(entryName))
	return fmt.Sprintf("discovery-%x", sum[:24])
}

func (p *HeadlessServicePlugin) OnPodCreate(ctx context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil || req.Pod == nil || !requiresHeadlessService(req.Role) {
		return nil
	}
	if err := p.ensureService(ctx, req, req.RoleIndex); err != nil {
		return err
	}
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	req.Pod.Spec.Hostname = req.Pod.Name
	req.Pod.Spec.Subdomain = DiscoveryServiceName(entry)
	utils.AddPodEnvVars(req.Pod, corev1.EnvVar{Name: workloadv1alpha1.EntryAddressEnv, Value: entry + "." + req.ModelServing.Namespace})
	return nil
}

func (p *HeadlessServicePlugin) OnPodReady(_ context.Context, _ *HookRequest) error { return nil }
func (p *HeadlessServicePlugin) OnRoleSync(ctx context.Context, req *HookRequest) error {
	if req == nil || !requiresHeadlessService(req.Role) {
		return nil
	}
	return p.ensureService(ctx, req, req.RoleIndex)
}

func roleSelector(req *HookRequest) map[string]string {
	return map[string]string{
		workloadv1alpha1.GroupNameLabelKey: req.ServingGroup,
		workloadv1alpha1.RoleLabelKey:      req.RoleName,
		workloadv1alpha1.RoleIDKey:         req.RoleID,
	}
}

// Capture freezes auxiliary UIDs before any Pod deletion can trigger callbacks.
func (c *RoleCleanup) Capture(ctx context.Context, req *HookRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capture(ctx, req)
}

func (c *RoleCleanup) capture(ctx context.Context, req *HookRequest) error {
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	services := req.KubeClient.CoreV1().Services(req.ModelServing.Namespace)
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	if c.targets == nil {
		targets := map[string]types.UID{}
		for _, name := range []string{entry, DiscoveryServiceName(entry)} {
			service, err := services.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("get Headless Service %s/%s: %w", req.ModelServing.Namespace, name, err)
			}
			if isManagedHeadlessService(service, req.ModelServing, req.ServingGroup, req.RoleName, req.RoleID) {
				targets[name] = service.UID
			}
		}
		c.targets, c.done = targets, map[string]bool{}
	}
	return nil
}

func (p *HeadlessServicePlugin) OnRoleDelete(ctx context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil {
		return nil
	}
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	cleanup := req.Cleanup
	if cleanup == nil {
		cleanup = &RoleCleanup{}
	}
	cleanup.mu.Lock()
	defer cleanup.mu.Unlock()
	services := req.KubeClient.CoreV1().Services(req.ModelServing.Namespace)
	if err := cleanup.capture(ctx, req); err != nil {
		return err
	}

	for name, uid := range cleanup.targets {
		if cleanup.done[name] {
			continue
		}
		service, err := services.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cleanup.done[name] = true
			continue
		}
		if err != nil {
			return err
		}
		// A retry never transfers an old deletion decision to a replacement UID.
		if service.UID != uid || !isManagedHeadlessService(service, req.ModelServing, req.ServingGroup, req.RoleName, req.RoleID) {
			cleanup.done[name] = true
			continue
		}
		err = services.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Headless Service %s/%s: %w", req.ModelServing.Namespace, name, err)
		}
		cleanup.done[name] = true
	}
	return nil
}

func (p *HeadlessServicePlugin) ensureService(ctx context.Context, req *HookRequest, _ int) error {
	if req == nil || req.ModelServing == nil {
		return nil
	}
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	ms := req.ModelServing
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	pods, err := req.KubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(roleSelector(req)).String()})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if utils.IsOwnedByModelServingWithUID(&pod, ms.UID) && pod.Spec.Subdomain == entry && pod.Labels[workloadv1alpha1.EntryLabelKey] != "true" {
			return fmt.Errorf("MigrationRequired: Role %s/%s still has worker %s using legacy subdomain %s; create a new ModelServing and migrate traffic", req.ServingGroup, req.RoleID, pod.Name, entry)
		}
	}
	services := req.KubeClient.CoreV1().Services(ms.Namespace)
	for _, name := range []string{entry, DiscoveryServiceName(entry)} {
		selector := roleSelector(req)
		if name == entry {
			selector[workloadv1alpha1.EntryLabelKey] = "true"
		}
		managedLabels := roleSelector(req)
		managedLabels[workloadv1alpha1.ModelServingNameLabelKey] = ms.Name
		managedLabels[HeadlessServicePluginLabelKey] = HeadlessServicePluginName
		service, err := services.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			service, err = services.Create(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ms.Namespace, Labels: managedLabels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)}},
				Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: selector, PublishNotReadyAddresses: true},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				service, err = services.Get(ctx, name, metav1.GetOptions{})
			}
		}
		if err != nil {
			return fmt.Errorf("ensure Headless Service %s/%s: %w", ms.Namespace, name, err)
		}
		if !isManagedHeadlessService(service, ms, req.ServingGroup, req.RoleName, req.RoleID) {
			return fmt.Errorf("Headless Service name %s/%s is occupied by an object not managed by plugin %s", ms.Namespace, name, HeadlessServicePluginName)
		}
		if service.DeletionTimestamp != nil {
			return fmt.Errorf("Headless Service %s/%s is terminating", ms.Namespace, name)
		}
		copy := service.DeepCopy()
		if copy.Labels == nil {
			copy.Labels = map[string]string{}
		}
		maps.Copy(copy.Labels, managedLabels)
		copy.Spec.Selector = selector
		copy.Spec.PublishNotReadyAddresses = true
		if !maps.Equal(copy.Labels, service.Labels) || !maps.Equal(selector, service.Spec.Selector) || !service.Spec.PublishNotReadyAddresses {
			// Update carries resourceVersion and UID; conflicts are retried by the queue.
			if _, err := services.Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func requiresHeadlessService(role *workloadv1alpha1.Role) bool {
	return role != nil && role.WorkerTemplate != nil
}

// ManagedHeadlessServiceIdentity recognizes only known names and complete Role
// identities. A plugin label alone never authorizes adoption or deletion.
func ManagedHeadlessServiceIdentity(service *corev1.Service, ms *workloadv1alpha1.ModelServing) (group, role, id string, ok bool) {
	if service == nil || ms == nil {
		return "", "", "", false
	}
	group, role, id = service.Labels[workloadv1alpha1.GroupNameLabelKey], service.Labels[workloadv1alpha1.RoleLabelKey], service.Labels[workloadv1alpha1.RoleIDKey]
	return group, role, id, isManagedHeadlessService(service, ms, group, role, id)
}

func isManagedHeadlessService(service *corev1.Service, ms *workloadv1alpha1.ModelServing, group, role, id string) bool {
	if service == nil || ms == nil || group == "" || role == "" || id == "" {
		return false
	}
	owner := metav1.GetControllerOf(service)
	if owner == nil || owner.UID != ms.UID || owner.Name != ms.Name || owner.Kind != workloadv1alpha1.ModelServingKind.Kind || owner.APIVersion != workloadv1alpha1.SchemeGroupVersion.String() {
		return false
	}
	parent, ordinal := utils.GetParentNameAndOrdinal(id)
	groupParent, groupOrdinal := utils.GetParentNameAndOrdinal(group)
	if parent != role || ordinal < 0 || groupParent != ms.Name || groupOrdinal < 0 {
		return false
	}
	entry := utils.GeneratePodName(group, id, 0)
	return (service.Name == entry || service.Name == DiscoveryServiceName(entry)) && service.Spec.ClusterIP == corev1.ClusterIPNone &&
		service.Labels[workloadv1alpha1.ModelServingNameLabelKey] == ms.Name && service.Labels[workloadv1alpha1.GroupNameLabelKey] == group &&
		service.Labels[workloadv1alpha1.RoleLabelKey] == role && service.Labels[workloadv1alpha1.RoleIDKey] == id
}
