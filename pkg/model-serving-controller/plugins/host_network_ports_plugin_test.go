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
	"encoding/json"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func hostNetworkPluginSpec(t *testing.T, cfg HostNetworkPortsConfig) workloadv1alpha1.PluginSpec {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return workloadv1alpha1.PluginSpec{Name: HostNetworkPortsPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn, Config: &apiextensionsv1.JSON{Raw: raw}}
}

func testHostNetworkPortsConfig() HostNetworkPortsConfig {
	return HostNetworkPortsConfig{
		BasePort: 7100, GroupStride: 12, PortName: "inference",
		Roles: map[string]HostNetworkPortRole{
			"prefill": {Offset: 0, MaxReplicas: 2, PodsPerReplica: 3},
			"decode":  {Offset: 6, MaxReplicas: 2, PodsPerReplica: 3},
		},
	}
}

func TestHostNetworkPortsPluginAssignsStablePortsForEveryPod(t *testing.T) {
	plugin, err := NewHostNetworkPortsPlugin(hostNetworkPluginSpec(t, testHostNetworkPortsConfig()))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, group, role, roleID string
		podIndex                  int
		wantPort                  int32
	}{
		{"prefill entry", "model-0", "prefill", "prefill-0", 0, 7100},
		{"prefill worker", "model-0", "prefill", "prefill-0", 1, 7101},
		{"second prefill entry", "model-0", "prefill", "prefill-1", 0, 7103},
		{"decode entry", "model-0", "decode", "decode-0", 0, 7106},
		{"decode worker", "model-0", "decode", "decode-0", 2, 7108},
		{"next group", "model-1", "prefill", "prefill-0", 0, 7112},
	}
	seen := map[int32]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: tc.group + "-" + tc.roleID + "-" + strconv.Itoa(tc.podIndex)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
			}
			req := &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model"}}, ServingGroup: tc.group, RoleName: tc.role, RoleID: tc.roleID, IsEntry: tc.podIndex == 0, Pod: pod}
			if err := plugin.OnPodCreate(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if !pod.Spec.HostNetwork || pod.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
				t.Fatalf("host network settings: %+v", pod.Spec)
			}
			port := pod.Spec.Containers[0].Ports
			if len(port) != 1 || port[0].Name != "inference" || port[0].ContainerPort != tc.wantPort || port[0].HostPort != tc.wantPort {
				t.Fatalf("port = %+v, want %d", port, tc.wantPort)
			}
			env := pod.Spec.Containers[0].Env
			if len(env) != 1 || env[0].Name != defaultHostNetworkPortEnv || env[0].Value != stringPort(tc.wantPort) {
				t.Fatalf("env = %+v", env)
			}
			if seen[tc.wantPort] {
				t.Fatalf("port %d was assigned twice", tc.wantPort)
			}
			seen[tc.wantPort] = true
			if err := plugin.OnPodCreate(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if len(pod.Spec.Containers[0].Ports) != 1 || len(pod.Spec.Containers[0].Env) != 1 {
				t.Fatalf("repeated hook duplicated port or env: %+v", pod.Spec.Containers[0])
			}
		})
	}
}

func stringPort(port int32) string { return strconv.FormatInt(int64(port), 10) }

func TestHostNetworkPortsPluginRejectsInvalidRegionsAndOrdinals(t *testing.T) {
	cfg := testHostNetworkPortsConfig()
	cfg.Roles["decode"] = HostNetworkPortRole{Offset: 5, MaxReplicas: 2, PodsPerReplica: 3}
	if _, err := NewHostNetworkPortsPlugin(hostNetworkPluginSpec(t, cfg)); err == nil {
		t.Fatal("overlapping role regions were accepted")
	}

	plugin, err := NewHostNetworkPortsPlugin(hostNetworkPluginSpec(t, testHostNetworkPortsConfig()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, roleID string
		isEntry      bool
	}{
		{"model-0-prefill-2-0", "prefill-2", true},
		{"model-0-prefill-0-3", "prefill-0", false},
		{"model-0-prefill-0-invalid", "prefill-0", false},
	} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: tc.name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}}}
		req := &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model"}}, ServingGroup: "model-0", RoleName: "prefill", RoleID: tc.roleID, IsEntry: tc.isEntry, Pod: pod}
		if err := plugin.OnPodCreate(context.Background(), req); err == nil {
			t.Fatalf("invalid Pod %q was accepted", tc.name)
		}
		if pod.Spec.HostNetwork || len(pod.Spec.Containers[0].Ports) != 0 {
			t.Fatalf("invalid Pod %q was mutated", tc.name)
		}
	}
}

func TestHostNetworkPortsPluginSelectsServingContainerAndRejectsDuplicatePort(t *testing.T) {
	cfg := testHostNetworkPortsConfig()
	role := cfg.Roles["prefill"]
	role.EntryContainerName = "server"
	cfg.Roles["prefill"] = role
	plugin, err := NewHostNetworkPortsPlugin(hostNetworkPluginSpec(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "model-0-prefill-0-0"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9000}}},
			{Name: "server", Ports: []corev1.ContainerPort{{Name: "inference", ContainerPort: 8000}}},
		}},
	}
	req := &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model"}}, ServingGroup: "model-0", RoleName: "prefill", RoleID: "prefill-0", IsEntry: true, Pod: pod}
	if err := plugin.OnPodCreate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.Containers[0].Ports[0].ContainerPort != 9000 || len(pod.Spec.Containers[0].Env) != 0 || pod.Spec.Containers[1].Ports[0].ContainerPort != 7100 {
		t.Fatalf("unexpected container mutation: %+v", pod.Spec.Containers)
	}
	pod.Spec.Containers[1].Ports = append(pod.Spec.Containers[1].Ports, corev1.ContainerPort{Name: "inference", ContainerPort: 8001})
	before := pod.DeepCopy()
	if err := plugin.OnPodCreate(context.Background(), req); err == nil {
		t.Fatal("duplicate named port was accepted")
	}
	if pod.Spec.HostNetwork != before.Spec.HostNetwork || pod.Spec.Containers[1].Ports[1].ContainerPort != before.Spec.Containers[1].Ports[1].ContainerPort {
		t.Fatal("invalid Pod was mutated")
	}
}

func TestHostNetworkPortsPluginReservesRoleRollingUpdateSurgePorts(t *testing.T) {
	plugin, err := NewHostNetworkPortsPlugin(hostNetworkPluginSpec(t, testHostNetworkPortsConfig()))
	if err != nil {
		t.Fatal(err)
	}
	replicas := int32(2)
	maxSurge := intstr.FromInt32(1)
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "model"},
		Spec: workloadv1alpha1.ModelServingSpec{
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate},
			Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{{
				Name: "prefill", Replicas: &replicas,
				RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{MaxSurge: &maxSurge},
			}}},
		},
	}

	create := func(group, roleID string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: group + "-" + roleID + "-0"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "server"}}},
		}
		req := &HookRequest{ModelServing: ms, ServingGroup: group, RoleName: "prefill", RoleID: roleID, IsEntry: true, Pod: pod}
		if err := plugin.OnPodCreate(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return pod
	}

	surgePod := create("model-0", "prefill-2")
	nextGroupPod := create("model-1", "prefill-0")
	if got, want := surgePod.Spec.Containers[0].Ports[0].ContainerPort, int32(7112); got != want {
		t.Fatalf("surge port = %d, want %d", got, want)
	}
	if got, want := nextGroupPod.Spec.Containers[0].Ports[0].ContainerPort, int32(7115); got != want {
		t.Fatalf("next group port = %d, want %d", got, want)
	}
}

func TestHostNetworkPortsPluginSynchronizesServingProbePorts(t *testing.T) {
	plugin, err := NewHostNetworkPortsPlugin(hostNetworkPluginSpec(t, testHostNetworkPortsConfig()))
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "model-0-prefill-0-0"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "server",
			Ports: []corev1.ContainerPort{{Name: "inference", ContainerPort: 8000, HostPort: 8000}},
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Port: intstr.FromInt32(8000),
			}}},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(8000),
			}}},
			StartupProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{
				Port: 8000,
			}}},
		}}},
	}
	req := &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model"}}, ServingGroup: "model-0", RoleName: "prefill", RoleID: "prefill-0", IsEntry: true, Pod: pod}
	if err := plugin.OnPodCreate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	container := pod.Spec.Containers[0]
	if got := container.LivenessProbe.HTTPGet.Port.IntVal; got != 7100 {
		t.Fatalf("liveness probe port = %d, want 7100", got)
	}
	if got := container.ReadinessProbe.TCPSocket.Port.IntVal; got != 7100 {
		t.Fatalf("readiness probe port = %d, want 7100", got)
	}
	if got := container.StartupProbe.GRPC.Port; got != 7100 {
		t.Fatalf("startup probe port = %d, want 7100", got)
	}
}
