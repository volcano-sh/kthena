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
	"maps"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	msutils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	lwsutils "sigs.k8s.io/lws/pkg/utils"
	lwspodutils "sigs.k8s.io/lws/pkg/utils/pod"
)

const LWSLabelsPluginName = "lws-standard-labels"

type LWSLabelsPlugin struct {
	BasePlugin
	name string
}

func init() {
	DefaultRegistry.Register(LWSLabelsPluginName, NewLWSLabelsPlugin)
}

func NewLWSLabelsPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	return &LWSLabelsPlugin{name: spec.Name}, nil
}

func (p *LWSLabelsPlugin) Name() string { return p.name }

// ExpectedLWSLabels computes the LeaderWorkerSet compatibility labels for a Pod
// when ms is owned by a LeaderWorkerSet, or returns nil when not applicable.
func ExpectedLWSLabels(ms *workloadv1alpha1.ModelServing, servingGroup, podName, namespace string, isEntry bool) (map[string]string, error) {
	if ms == nil {
		return nil, nil
	}

	lwsName, ok := getOwningLWSName(ms.OwnerReferences)
	if !ok {
		return nil, nil
	}

	_, groupIndex := msutils.GetParentNameAndOrdinal(servingGroup)
	if groupIndex < 0 {
		return nil, fmt.Errorf("invalid servingGroup %q for pod %s", servingGroup, podName)
	}

	workerIndex, err := deriveWorkerIndex(isEntry, podName)
	if err != nil {
		return nil, err
	}

	leaderHostname := fmt.Sprintf("%s-%d", lwsName, groupIndex)
	groupKey := lwsutils.Sha1Hash(fmt.Sprintf("%s/%s", namespace, leaderHostname))
	return map[string]string{
		leaderworkerset.SetNameLabelKey:         lwsName,
		leaderworkerset.GroupIndexLabelKey:      strconv.Itoa(groupIndex),
		leaderworkerset.WorkerIndexLabelKey:     strconv.Itoa(workerIndex),
		leaderworkerset.GroupUniqueHashLabelKey: groupKey,
	}, nil
}

func (p *LWSLabelsPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.Pod == nil || req.ModelServing == nil {
		return nil
	}

	lwsLabels, err := ExpectedLWSLabels(req.ModelServing, req.ServingGroup, req.Pod.Name, req.Pod.Namespace, req.IsEntry)
	if err != nil {
		return err
	}
	if lwsLabels == nil {
		return nil
	}

	if req.Role == nil {
		return fmt.Errorf("missing role for LWS pod %s", req.Pod.Name)
	}
	lwsName := lwsLabels[leaderworkerset.SetNameLabelKey]
	_, groupIndex := msutils.GetParentNameAndOrdinal(req.ServingGroup)
	workerIndex, err := deriveWorkerIndex(req.IsEntry, req.Pod.Name)
	if err != nil {
		return err
	}
	groupSize := int(req.Role.WorkerReplicas) + 1
	leaderHostname := fmt.Sprintf("%s-%d", lwsName, groupIndex)

	if req.Pod.Labels == nil {
		req.Pod.Labels = map[string]string{}
	}
	maps.Copy(req.Pod.Labels, lwsLabels)
	if req.Pod.Annotations == nil {
		req.Pod.Annotations = map[string]string{}
	}
	req.Pod.Annotations[leaderworkerset.SizeAnnotationKey] = strconv.Itoa(groupSize)

	req.Pod.Spec.Hostname = leaderHostname
	if workerIndex > 0 {
		req.Pod.Spec.Hostname = fmt.Sprintf("%s-%d", leaderHostname, workerIndex)
		req.Pod.Annotations[leaderworkerset.LeaderPodNameAnnotationKey] = leaderHostname
	}
	req.Pod.Spec.Subdomain = lwsName
	req.lwsPodDNS = true

	return lwspodutils.AddLWSVariables(req.Pod)
}

// SupportsInPlaceUpdate is true, because an image patch does not change these labels.
func (p *LWSLabelsPlugin) SupportsInPlaceUpdate() bool {
	return true
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
