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
	"strconv"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	// LWSLabelsPluginName is the registered name of the LWS labels plugin.
	LWSLabelsPluginName = "lws-labels"

	// Standard LWS label keys as defined by the LeaderWorkerSet API.
	LWSLabelName        = "leaderworkerset.sigs.k8s.io/name"
	LWSLabelGroupIndex  = "leaderworkerset.sigs.k8s.io/group-index"
	LWSLabelWorkerIndex = "leaderworkerset.sigs.k8s.io/worker-index"
	LWSLabelGroupKey    = "leaderworkerset.sigs.k8s.io/group-key"
)

// LWSLabelsPlugin injects standard LeaderWorkerSet labels into pods
// created for LWS workloads, enabling compatibility with the LWS
// ecosystem (monitoring, logging, controllers).
type LWSLabelsPlugin struct {
	name string
}

func init() {
	DefaultRegistry.Register(LWSLabelsPluginName, NewLWSLabelsPlugin)
}

// NewLWSLabelsPlugin constructs the LWS labels plugin from a PluginSpec.
// This plugin does not require any configuration.
func NewLWSLabelsPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	return &LWSLabelsPlugin{name: spec.Name}, nil
}

func (p *LWSLabelsPlugin) Name() string { return p.name }

// OnPodCreate injects the four standard LWS labels into the pod.
// Labels are merged safely: existing user-defined labels are never overwritten.
func (p *LWSLabelsPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.Pod == nil || req.ModelServing == nil {
		return nil
	}

	// Derive label values from the HookRequest context.
	lwsName := req.ModelServing.Name

	// Extract group index from the serving group name (e.g. "my-lws-0" → "0").
	_, groupIndex := utils.GetParentNameAndOrdinal(req.ServingGroup)
	if groupIndex < 0 {
		return fmt.Errorf("cannot extract group index from serving group name %q", req.ServingGroup)
	}
	groupIndexStr := strconv.Itoa(groupIndex)

	// Extract worker index from the pod name (trailing ordinal, e.g. "my-lws-0-default-0-1" → "1").
	_, workerIndex := utils.GetParentNameAndOrdinal(req.Pod.Name)
	if workerIndex < 0 {
		return fmt.Errorf("cannot extract worker index from pod name %q", req.Pod.Name)
	}
	workerIndexStr := strconv.Itoa(workerIndex)

	// Group key uniquely identifies the group within the LWS.
	groupKey := fmt.Sprintf("%s-%s", lwsName, groupIndexStr)

	// Ensure labels map is initialized.
	if req.Pod.Labels == nil {
		req.Pod.Labels = map[string]string{}
	}

	// Merge safely: do not overwrite existing user-defined labels.
	setIfAbsent(req.Pod.Labels, LWSLabelName, lwsName)
	setIfAbsent(req.Pod.Labels, LWSLabelGroupIndex, groupIndexStr)
	setIfAbsent(req.Pod.Labels, LWSLabelWorkerIndex, workerIndexStr)
	setIfAbsent(req.Pod.Labels, LWSLabelGroupKey, groupKey)

	return nil
}

// OnPodReady is a no-op for the LWS labels plugin.
func (p *LWSLabelsPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

// setIfAbsent sets a label only if the key is not already present,
// preserving any user-defined value.
func setIfAbsent(labels map[string]string, key, value string) {
	if _, exists := labels[key]; !exists {
		labels[key] = value
	}
}
