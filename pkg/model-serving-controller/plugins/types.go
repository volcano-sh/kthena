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

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// HookRequest carries the context for plugin hook invocations.
type HookRequest struct {
	ModelServing *workloadv1alpha1.ModelServing
	ServingGroup string
	RoleName     string
	RoleID       string
	IsEntry      bool
	Pod          *corev1.Pod
}

// Plugin defines the lifecycle hooks a plugin may implement.
type Plugin interface {
	Name() string
	// OnPodCreate is invoked before the controller creates the Pod. Mutations are applied in-place to req.Pod.
	OnPodCreate(ctx context.Context, req *HookRequest) error
	// OnPodReady is invoked when the controller observes the Pod running and ready.
	OnPodReady(ctx context.Context, req *HookRequest) error
}
