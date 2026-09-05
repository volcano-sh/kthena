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

	corev1 "k8s.io/api/core/v1"
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

	groupSize, err := getGroupSize(req)
	if err != nil {
		return err
	}
	leaderHostname := fmt.Sprintf("%s-%d", lwsName, groupIndex)
	groupKey := lwsutils.Sha1Hash(fmt.Sprintf("%s/%s", req.Pod.Namespace, leaderHostname))

	if req.Pod.Labels == nil {
		req.Pod.Labels = map[string]string{}
	}
	req.Pod.Labels[leaderworkerset.SetNameLabelKey] = lwsName
	req.Pod.Labels[leaderworkerset.GroupIndexLabelKey] = strconv.Itoa(groupIndex)
	req.Pod.Labels[leaderworkerset.WorkerIndexLabelKey] = strconv.Itoa(workerIndex)
	req.Pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = groupKey
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

	lwsEnv := []corev1.EnvVar{
		{
			Name:  leaderworkerset.LwsLeaderAddress,
			Value: fmt.Sprintf("%s.%s.%s", leaderHostname, lwsName, req.Pod.Namespace),
		},
		{Name: leaderworkerset.LwsGroupSize, Value: strconv.Itoa(groupSize)},
		{Name: leaderworkerset.LwsWorkerIndex, Value: strconv.Itoa(workerIndex)},
	}
	for i := range req.Pod.Spec.Containers {
		prependEnvVars(&req.Pod.Spec.Containers[i], lwsEnv)
	}
	for i := range req.Pod.Spec.InitContainers {
		prependEnvVars(&req.Pod.Spec.InitContainers[i], lwsEnv)
	}

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

func getGroupSize(req *HookRequest) (int, error) {
	for _, role := range req.ModelServing.Spec.Template.Roles {
		if role.Name == req.RoleName {
			return int(role.WorkerReplicas) + 1, nil
		}
	}
	return 0, fmt.Errorf("role %q not found in modelServing %s/%s", req.RoleName, req.ModelServing.Namespace, req.ModelServing.Name)
}

func prependEnvVars(container *corev1.Container, envVars []corev1.EnvVar) {
	names := make(map[string]struct{}, len(envVars))
	for _, env := range envVars {
		names[env.Name] = struct{}{}
	}

	retained := make([]corev1.EnvVar, 0, len(container.Env))
	for _, env := range container.Env {
		if _, replaced := names[env.Name]; !replaced {
			retained = append(retained, env)
		}
	}
	container.Env = append(envVars, retained...)
}
