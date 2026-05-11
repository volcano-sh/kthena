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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	msutils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	lwsutils "sigs.k8s.io/lws/pkg/utils"
)

const LWSLabelsPluginName = "lws-standard-labels"

type LWSLabelsPlugin struct {
	name string
}

func init() {
	DefaultRegistry.Register(LWSLabelsPluginName, NewLWSLabelsPlugin)
}

func NewLWSLabelsPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	return &LWSLabelsPlugin{name: spec.Name}, nil
}

func (p *LWSLabelsPlugin) Name() string { return p.name }

func (p *LWSLabelsPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.Pod == nil || req.ModelServing == nil {
		return nil
	}

	lwsName, ok := getOwningLWSName(req.ModelServing.OwnerReferences)
	if !ok {
		return nil
	}

	_, groupIndex := msutils.GetParentNameAndOrdinal(req.ServingGroup)
	if groupIndex < 0 {
		return fmt.Errorf("invalid servingGroup %q for pod %s", req.ServingGroup, req.Pod.Name)
	}

	workerIndex, err := deriveWorkerIndex(req.IsEntry, req.Pod.Name)
	if err != nil {
		return err
	}

	groupKey := lwsutils.Sha1Hash(fmt.Sprintf("%s-%d", lwsName, groupIndex))

	if req.Pod.Labels == nil {
		req.Pod.Labels = map[string]string{}
	}
	req.Pod.Labels[leaderworkerset.SetNameLabelKey] = lwsName
	req.Pod.Labels[leaderworkerset.GroupIndexLabelKey] = strconv.Itoa(groupIndex)
	req.Pod.Labels[leaderworkerset.WorkerIndexLabelKey] = strconv.Itoa(workerIndex)
	req.Pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupKey

	return nil
}

func (p *LWSLabelsPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

func getOwningLWSName(ownerRefs []metav1.OwnerReference) (string, bool) {
	for _, ref := range ownerRefs {
		if ref.Kind == "LeaderWorkerSet" && ref.Name != "" {
			return ref.Name, true
		}
	}
	return "", false
}

func deriveWorkerIndex(isEntry bool, podName string) (int, error) {
	if isEntry {
		return 0, nil
	}
	lastDash := strings.LastIndex(podName, "-")
	if lastDash < 0 || lastDash == len(podName)-1 {
		return 0, fmt.Errorf("cannot derive worker-index from pod name %q", podName)
	}
	n, err := strconv.Atoi(podName[lastDash+1:])
	if err != nil {
		return 0, fmt.Errorf("cannot derive worker-index from pod name %q: %w", podName, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid worker-index %d derived from pod name %q", n, podName)
	}
	return n, nil
}
