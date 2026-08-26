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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	HeadlessServicePluginName     = "headless-service"
	HeadlessServicePluginLabelKey = "modelserving.volcano.sh/plugin"
)

// HeadlessServicePlugin manages the Headless Service associated with each Role
// replica that has a worker template.
type HeadlessServicePlugin struct {
	name string
}

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

func (p *HeadlessServicePlugin) OnPodCreate(ctx context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil || req.Pod == nil || !requiresHeadlessService(req.Role) {
		return nil
	}
	entryServiceName := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	req.Pod.Spec.Hostname = req.Pod.Name
	req.Pod.Spec.Subdomain = entryServiceName
	utils.AddPodEnvVars(req.Pod, corev1.EnvVar{
		Name:  workloadv1alpha1.EntryAddressEnv,
		Value: entryServiceName + "." + req.ModelServing.Namespace,
	})
	if !req.IsEntry {
		return nil
	}
	_, roleIndex := utils.GetParentNameAndOrdinal(req.RoleID)
	if roleIndex < 0 {
		return fmt.Errorf("invalid Role ID %q", req.RoleID)
	}
	return p.ensureService(ctx, req, roleIndex)
}

func (p *HeadlessServicePlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

func (p *HeadlessServicePlugin) OnRoleSync(ctx context.Context, req *HookRequest) error {
	if req == nil || !requiresHeadlessService(req.Role) {
		return nil
	}
	return p.ensureService(ctx, req, req.RoleIndex)
}

func (p *HeadlessServicePlugin) OnRoleDelete(ctx context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil {
		return nil
	}
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	serviceName := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	services := req.KubeClient.CoreV1().Services(req.ModelServing.Namespace)
	service, err := services.Get(ctx, serviceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Headless Service %s/%s: %w", req.ModelServing.Namespace, serviceName, err)
	}
	if !isManagedHeadlessService(service, req.ModelServing, req.ServingGroup, req.RoleName, req.RoleID) {
		return nil
	}
	if err := services.Delete(ctx, serviceName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Headless Service %s/%s: %w", req.ModelServing.Namespace, serviceName, err)
	}
	return nil
}

func (p *HeadlessServicePlugin) ensureService(
	ctx context.Context,
	req *HookRequest,
	roleIndex int,
) error {
	if req == nil {
		return nil
	}
	ms := req.ModelServing
	if ms == nil {
		return nil
	}
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	servingGroup, roleName, roleID := req.ServingGroup, req.RoleName, req.RoleID
	serviceName := utils.GeneratePodName(servingGroup, roleID, 0)
	var service *corev1.Service
	var err error
	if req.ServiceLister != nil {
		service, err = req.ServiceLister.Services(ms.Namespace).Get(serviceName)
	} else {
		service, err = req.KubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	}
	if err == nil {
		if !isManagedHeadlessService(service, ms, servingGroup, roleName, roleID) {
			return fmt.Errorf("Headless Service name %s/%s is occupied by an object not managed by plugin %s", ms.Namespace, serviceName, HeadlessServicePluginName)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Headless Service %s/%s: %w", ms.Namespace, serviceName, err)
	}

	selector := map[string]string{
		workloadv1alpha1.GroupNameLabelKey: servingGroup,
		workloadv1alpha1.RoleLabelKey:      roleName,
		workloadv1alpha1.RoleIDKey:         roleID,
	}
	err = utils.CreateHeadlessService(ctx, req.KubeClient, ms, selector, servingGroup, roleName, roleIndex, map[string]string{
		HeadlessServicePluginLabelKey: HeadlessServicePluginName,
	})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Headless Service %s/%s: %w", ms.Namespace, serviceName, err)
	}

	// Close the cache-miss/create race without accepting an unmanaged object
	// that appeared under the generated name.
	service, err = req.KubeClient.CoreV1().Services(ms.Namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get existing Headless Service %s/%s after create conflict: %w", ms.Namespace, serviceName, err)
	}
	if !isManagedHeadlessService(service, ms, servingGroup, roleName, roleID) {
		return fmt.Errorf("Headless Service name %s/%s is occupied by an object not managed by plugin %s", ms.Namespace, serviceName, HeadlessServicePluginName)
	}
	return nil
}

func requiresHeadlessService(role *workloadv1alpha1.Role) bool {
	return role != nil && role.WorkerTemplate != nil
}

func isManagedHeadlessService(
	service *corev1.Service,
	ms *workloadv1alpha1.ModelServing,
	servingGroup, roleName, roleID string,
) bool {
	if service == nil || ms == nil || !utils.IsOwnedByModelServingWithUID(service, ms.UID) {
		return false
	}
	if service.Labels[HeadlessServicePluginLabelKey] == HeadlessServicePluginName {
		return true
	}
	// Services created before plugin identity labeling are recognized only by
	// their complete legacy Headless Service shape.
	return service.Spec.ClusterIP == corev1.ClusterIPNone &&
		service.Labels[workloadv1alpha1.ModelServingNameLabelKey] == ms.Name &&
		service.Labels[workloadv1alpha1.GroupNameLabelKey] == servingGroup &&
		service.Labels[workloadv1alpha1.RoleLabelKey] == roleName &&
		service.Labels[workloadv1alpha1.RoleIDKey] == roleID
}
