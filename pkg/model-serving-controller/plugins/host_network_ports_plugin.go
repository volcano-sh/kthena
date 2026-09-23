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
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	HostNetworkPortsPluginName = "host-network-ports"
	defaultHostNetworkPortEnv  = "KTHENA_HTTP_PORT"
)

// HostNetworkPortRole reserves a stable region of ports for one role in every
// ServingGroup. PodsPerReplica includes the entry Pod at index zero.
type HostNetworkPortRole struct {
	Offset              int32  `json:"offset"`
	MaxReplicas         int32  `json:"maxReplicas"`
	PodsPerReplica      int32  `json:"podsPerReplica"`
	EntryContainerName  string `json:"entryContainerName,omitempty"`
	WorkerContainerName string `json:"workerContainerName,omitempty"`
}

type HostNetworkPortsConfig struct {
	BasePort    int32                          `json:"basePort"`
	GroupStride int32                          `json:"groupStride"`
	PortName    string                         `json:"portName"`
	EnvName     string                         `json:"envName,omitempty"`
	Roles       map[string]HostNetworkPortRole `json:"roles"`
}

type HostNetworkPortsPlugin struct {
	name string
	cfg  HostNetworkPortsConfig
}

func init() {
	DefaultRegistry.Register(HostNetworkPortsPluginName, NewHostNetworkPortsPlugin)
}

func NewHostNetworkPortsPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	if spec.Scope != nil && (spec.Scope.Target != "" && spec.Scope.Target != workloadv1alpha1.PluginTargetAll || len(spec.Scope.Roles) != 0) {
		return nil, fmt.Errorf("host-network-ports requires scope.target All or omitted and no role filter")
	}
	var cfg HostNetworkPortsConfig
	if err := DecodeJSON(spec.Config, &cfg); err != nil {
		return nil, err
	}
	if cfg.BasePort < 1 || cfg.BasePort > 65535 {
		return nil, fmt.Errorf("basePort must be between 1 and 65535")
	}
	if cfg.GroupStride < 1 {
		return nil, fmt.Errorf("groupStride must be positive")
	}
	if reasons := validation.IsValidPortName(cfg.PortName); len(reasons) != 0 {
		return nil, fmt.Errorf("invalid portName %q: %s", cfg.PortName, strings.Join(reasons, ", "))
	}
	if cfg.EnvName == "" {
		cfg.EnvName = defaultHostNetworkPortEnv
	}
	if reasons := validation.IsEnvVarName(cfg.EnvName); len(reasons) != 0 {
		return nil, fmt.Errorf("invalid envName %q: %s", cfg.EnvName, strings.Join(reasons, ", "))
	}
	if len(cfg.Roles) == 0 {
		return nil, fmt.Errorf("roles must contain at least one role")
	}
	type region struct {
		name       string
		start, end int64
	}
	regions := make([]region, 0, len(cfg.Roles))
	for name, role := range cfg.Roles {
		if role.Offset < 0 || role.MaxReplicas < 1 || role.PodsPerReplica < 1 {
			return nil, fmt.Errorf("role %q requires non-negative offset and positive maxReplicas and podsPerReplica", name)
		}
		end := int64(role.Offset) + int64(role.MaxReplicas)*int64(role.PodsPerReplica)
		if end > int64(cfg.GroupStride) {
			return nil, fmt.Errorf("role %q region exceeds groupStride", name)
		}
		regions = append(regions, region{name: name, start: int64(role.Offset), end: end})
	}
	slices.SortFunc(regions, func(a, b region) int { return strings.Compare(a.name, b.name) })
	for i := range regions {
		for j := i + 1; j < len(regions); j++ {
			if regions[i].start < regions[j].end && regions[j].start < regions[i].end {
				return nil, fmt.Errorf("role port regions %q and %q overlap", regions[i].name, regions[j].name)
			}
		}
	}
	return &HostNetworkPortsPlugin{name: spec.Name, cfg: cfg}, nil
}

func (p *HostNetworkPortsPlugin) Name() string { return p.name }

func (p *HostNetworkPortsPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil || req.Pod == nil {
		return fmt.Errorf("host-network-ports requires ModelServing and Pod")
	}
	role, ok := p.cfg.Roles[req.RoleName]
	if !ok {
		return fmt.Errorf("role %q has no host-network-ports configuration", req.RoleName)
	}
	groupIndex, err := portOrdinal(req.ServingGroup, req.ModelServing.Name+"-")
	if err != nil {
		return fmt.Errorf("ServingGroup: %w", err)
	}
	roleIndex, err := portOrdinal(req.RoleID, req.RoleName+"-")
	if err != nil {
		return fmt.Errorf("Role: %w", err)
	}
	podIndex, err := portOrdinal(req.Pod.Name, req.ServingGroup+"-"+req.RoleID+"-")
	if err != nil {
		return fmt.Errorf("Pod: %w", err)
	}
	if req.IsEntry != (podIndex == 0) {
		return fmt.Errorf("Pod %q entry flag disagrees with ordinal %d", req.Pod.Name, podIndex)
	}
	if podIndex >= int64(role.PodsPerReplica) {
		return fmt.Errorf("Pod %q exceeds configured role capacity", req.Pod.Name)
	}
	effectiveStride, roleExtraOffsets, roleCapacities, err := p.portLayout(req.ModelServing)
	if err != nil {
		return err
	}
	if roleIndex >= roleCapacities[req.RoleName] {
		return fmt.Errorf("Pod %q exceeds configured role capacity plus automatic rollout headroom", req.Pod.Name)
	}
	roleSlot := int64(role.Offset) + roleIndex*int64(role.PodsPerReplica)
	if roleIndex >= int64(role.MaxReplicas) {
		roleSlot = int64(p.cfg.GroupStride) + roleExtraOffsets[req.RoleName] +
			(roleIndex-int64(role.MaxReplicas))*int64(role.PodsPerReplica)
	}
	port := int64(p.cfg.BasePort) + groupIndex*effectiveStride + roleSlot + podIndex
	if port < 1 || port > 65535 {
		return fmt.Errorf("Pod %q calculated port %d is outside 1-65535", req.Pod.Name, port)
	}
	containerIndex, err := hostNetworkServingContainer(req.Pod, role, req.IsEntry)
	if err != nil {
		return err
	}
	portIndex := -1
	for i, container := range req.Pod.Spec.Containers {
		for j, declared := range container.Ports {
			if declared.Name == p.cfg.PortName && i != containerIndex {
				return fmt.Errorf("Pod %q portName %q is declared by another container", req.Pod.Name, p.cfg.PortName)
			}
			if declared.Name == p.cfg.PortName {
				if portIndex != -1 {
					return fmt.Errorf("Pod %q declares portName %q more than once", req.Pod.Name, p.cfg.PortName)
				}
				portIndex = j
			}
		}
	}

	req.Pod.Spec.HostNetwork = true
	if req.Pod.Spec.DNSPolicy == "" || req.Pod.Spec.DNSPolicy == corev1.DNSClusterFirst {
		req.Pod.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}
	container := &req.Pod.Spec.Containers[containerIndex]
	oldServingPorts := servingPortNumbers(container, portIndex, p.cfg.EnvName)
	if portIndex == -1 {
		container.Ports = append(container.Ports, corev1.ContainerPort{Name: p.cfg.PortName, ContainerPort: int32(port), HostPort: int32(port), Protocol: corev1.ProtocolTCP})
	} else {
		container.Ports[portIndex].ContainerPort = int32(port)
		container.Ports[portIndex].HostPort = int32(port)
		container.Ports[portIndex].Protocol = corev1.ProtocolTCP
	}
	envFound := false
	for i := range container.Env {
		if container.Env[i].Name != p.cfg.EnvName {
			continue
		}
		container.Env[i].Value = strconv.FormatInt(port, 10)
		container.Env[i].ValueFrom = nil
		envFound = true
	}
	if !envFound {
		container.Env = append(container.Env, corev1.EnvVar{Name: p.cfg.EnvName, Value: strconv.FormatInt(port, 10)})
	}
	syncServingProbePorts(container, oldServingPorts, int32(port))
	return nil
}

// portLayout keeps each configured role region unchanged and appends only the
// extra role ordinals required by the current desired replicas and maxSurge to
// the end of each ServingGroup region. This lets maxReplicas keep its natural
// steady-state meaning while RoleRollingUpdate can create temporary replicas.
func (p *HostNetworkPortsPlugin) portLayout(ms *workloadv1alpha1.ModelServing) (int64, map[string]int64, map[string]int64, error) {
	roleExtras := make(map[string]int64, len(p.cfg.Roles))
	roleCapacities := make(map[string]int64, len(p.cfg.Roles))
	desiredByRole := make(map[string]int64, len(p.cfg.Roles))
	surgeByRole := make(map[string]int64, len(p.cfg.Roles))
	if ms != nil {
		for _, role := range ms.Spec.Template.Roles {
			desired := int64(1)
			if role.Replicas != nil {
				desired = int64(*role.Replicas)
			}
			desiredByRole[role.Name] = desired
			if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate || role.MaxSurge == nil {
				continue
			}
			resolved, err := intstr.GetScaledValueFromIntOrPercent(role.MaxSurge, int(desired), true)
			if err != nil {
				return 0, nil, nil, fmt.Errorf("role %q has invalid maxSurge: %w", role.Name, err)
			}
			surgeByRole[role.Name] = int64(resolved)
		}
	}

	roleNames := make([]string, 0, len(p.cfg.Roles))
	for name := range p.cfg.Roles {
		roleNames = append(roleNames, name)
	}
	slices.Sort(roleNames)
	extraSlots := int64(0)
	for _, name := range roleNames {
		role := p.cfg.Roles[name]
		capacity := int64(role.MaxReplicas)
		required := desiredByRole[name] + surgeByRole[name]
		if required > capacity {
			capacity = required
		}
		roleCapacities[name] = capacity
		roleExtras[name] = extraSlots
		extraSlots += (capacity - int64(role.MaxReplicas)) * int64(role.PodsPerReplica)
	}
	return int64(p.cfg.GroupStride) + extraSlots, roleExtras, roleCapacities, nil
}

func servingPortNumbers(container *corev1.Container, portIndex int, envName string) map[int32]struct{} {
	ports := make(map[int32]struct{}, 3)
	if portIndex >= 0 {
		declared := container.Ports[portIndex]
		if declared.ContainerPort > 0 {
			ports[declared.ContainerPort] = struct{}{}
		}
		if declared.HostPort > 0 {
			ports[declared.HostPort] = struct{}{}
		}
	}
	for _, env := range container.Env {
		if env.Name != envName || env.ValueFrom != nil {
			continue
		}
		value, err := strconv.ParseInt(env.Value, 10, 32)
		if err == nil && value > 0 && value <= 65535 {
			ports[int32(value)] = struct{}{}
		}
	}
	return ports
}

func syncServingProbePorts(container *corev1.Container, oldPorts map[int32]struct{}, port int32) {
	if len(oldPorts) == 0 {
		return
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
		if probe == nil {
			continue
		}
		if probe.HTTPGet != nil && probe.HTTPGet.Port.Type == intstr.Int {
			if _, ok := oldPorts[probe.HTTPGet.Port.IntVal]; ok {
				probe.HTTPGet.Port = intstr.FromInt32(port)
			}
		}
		if probe.TCPSocket != nil && probe.TCPSocket.Port.Type == intstr.Int {
			if _, ok := oldPorts[probe.TCPSocket.Port.IntVal]; ok {
				probe.TCPSocket.Port = intstr.FromInt32(port)
			}
		}
		if probe.GRPC != nil {
			if _, ok := oldPorts[probe.GRPC.Port]; ok {
				probe.GRPC.Port = port
			}
		}
	}
}

func portOrdinal(value, prefix string) (int64, error) {
	suffix, ok := strings.CutPrefix(value, prefix)
	if !ok || suffix == "" {
		return 0, fmt.Errorf("%q does not have expected prefix %q and ordinal", value, prefix)
	}
	index, err := strconv.ParseInt(suffix, 10, 32)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("%q has invalid ordinal", value)
	}
	return index, nil
}

func hostNetworkServingContainer(pod *corev1.Pod, role HostNetworkPortRole, isEntry bool) (int, error) {
	name := role.WorkerContainerName
	if isEntry {
		name = role.EntryContainerName
	}
	if name == "" && len(pod.Spec.Containers) == 1 {
		return 0, nil
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name && name != "" {
			return i, nil
		}
	}
	return 0, fmt.Errorf("Pod %q needs an unambiguous serving container name", pod.Name)
}

func (p *HostNetworkPortsPlugin) OnPodRunning(context.Context, *HookRequest) error { return nil }
func (p *HostNetworkPortsPlugin) OnPodReady(context.Context, *HookRequest) error   { return nil }
func (p *HostNetworkPortsPlugin) OnPodDelete(context.Context, *HookRequest) error  { return nil }
func (p *HostNetworkPortsPlugin) OnRoleSync(context.Context, *HookRequest) error   { return nil }
func (p *HostNetworkPortsPlugin) OnRoleDelete(context.Context, *HookRequest) error { return nil }
func (p *HostNetworkPortsPlugin) OnServingGroupDelete(context.Context, *HookRequest) error {
	return nil
}
