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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// HookResult describes the outcome of a ModelServing sync hook invocation.
type HookResult struct {
	// RequeueAfter requests re-enqueuing the ModelServing after the given duration.
	// A zero or non-positive duration means do not requeue.
	RequeueAfter time.Duration
	// Err contains the error returned by the hook, if any.
	Err error
	// Continue indicates whether reconciliation should proceed when Err is non-nil.
	Continue bool
}

// HookRequest carries the context for plugin hook invocations.
type HookRequest struct {
	ModelServing  *workloadv1alpha1.ModelServing
	ServingGroup  string
	RoleName      string
	RoleID        string
	RoleIndex     int
	Role          *workloadv1alpha1.Role
	IsEntry       bool
	Pod           *corev1.Pod
	KubeClient    kubernetes.Interface
	ServiceLister listerv1.ServiceLister
	// lwsPodDNS records that the LWS plugin configured this Pod for its shared Service.
	lwsPodDNS bool
	// Cleanup retains auxiliary UID targets across retries of one Role deletion.
	Cleanup *RoleCleanup
}

// Plugin defines the lifecycle hooks implemented by a plugin.
type Plugin interface {
	Name() string
	// OnModelServingSync is invoked on each ModelServing reconcile before ServingGroups and Roles are synced.
	OnModelServingSync(ctx context.Context, req *HookRequest) HookResult
	// OnPodCreate is invoked before the controller creates the Pod. Mutations are applied in-place to req.Pod.
	OnPodCreate(ctx context.Context, req *HookRequest) error
	// OnPodReady is invoked when the controller observes the Pod running and ready.
	OnPodReady(ctx context.Context, req *HookRequest) error
	// OnRoleSync idempotently ensures resources for an active Role.
	OnRoleSync(ctx context.Context, req *HookRequest) error
	// OnRoleDelete deletes resources associated with a Role. It must not wait for
	// the underlying resources to disappear.
	OnRoleDelete(ctx context.Context, req *HookRequest) error
}

// BasePlugin provides default no-op implementations for all Plugin lifecycle hooks.
// Plugins can embed BasePlugin and override only the hooks they need.
type BasePlugin struct{}

func (BasePlugin) OnModelServingSync(_ context.Context, _ *HookRequest) HookResult {
	return HookResult{}
}

func (BasePlugin) OnPodCreate(_ context.Context, _ *HookRequest) error {
	return nil
}

func (BasePlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

func (BasePlugin) OnRoleSync(_ context.Context, _ *HookRequest) error {
	return nil
}

func (BasePlugin) OnRoleDelete(_ context.Context, _ *HookRequest) error {
	return nil
}

// InPlaceUpdatePlugin is implemented by plugins whose Pod mutations stay valid after an in-place image patch.
// Without it, outdated ServingGroups are recreated instead.
type InPlaceUpdatePlugin interface {
	SupportsInPlaceUpdate() bool
}
