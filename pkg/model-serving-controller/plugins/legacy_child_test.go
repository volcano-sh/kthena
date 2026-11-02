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
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func TestPreserveChildPluginConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*workload.ModelServing)
		allowed bool
	}{
		{"unchanged", func(*workload.ModelServing) {}, true},
		{"metadata", func(ms *workload.ModelServing) { ms.Annotations = map[string]string{"note": "new"} }, true},
		{"SG shrink", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) }, true},
		{"SG zero", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](0) }, true},
		{"Role shrink", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1) }, true},
		{"equivalent defaults", func(ms *workload.ModelServing) { normalizeChildDefaults(ms) }, true},
		{"SG expansion", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](3) }, false},
		{"Role expansion", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3) }, false},
		{"worker shrink is a template change", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].WorkerReplicas = 0 }, false},
		{"image change with SG shrink", func(ms *workload.ModelServing) {
			ms.Spec.Replicas = ptr.To[int32](1)
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "new"
		}, false},
		{"Pod annotation", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Metadata = &workload.Metadata{Annotations: map[string]string{"user": "new"}}
		}, false},
		{"Role rename", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Name = "new" }, false},
		{"explicit plugin scope", func(ms *workload.ModelServing) {
			ms.Spec.Plugins[0].Scope = &workload.PluginScope{Roles: []string{"serve"}}
		}, false},
		{"additional plugin", func(ms *workload.ModelServing) {
			ms.Spec.Plugins = append(ms.Spec.Plugins, workload.PluginSpec{Name: "custom", Type: workload.PluginTypeBuiltIn})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "default", UID: "old-uid"}, Spec: workload.ModelServingSpec{
				Replicas: ptr.To[int32](2), Template: workload.ServingGroup{Roles: []workload.Role{{Name: "serve", Replicas: ptr.To[int32](2), WorkerReplicas: 1,
					EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "old"}}}},
					WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "old"}}}},
				}}},
			}}
			original := old.DeepCopy()
			desired := old.DeepCopy()
			desired.Spec.Plugins = []workload.PluginSpec{{Name: HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}}
			tc.change(desired)
			err := PreserveChildPluginConfiguration(old, desired)
			if tc.allowed {
				require.NoError(t, err)
				assert.Equal(t, old.Spec.Plugins, desired.Spec.Plugins)
			} else {
				require.ErrorContains(t, err, "MigrationRequired")
			}
			assert.Equal(t, original, old, "lister object must remain unchanged")
		})
	}
}

// Read defaults from the actual CRD instead of mirroring the production
// normalizer: fake API clients do not expose admission-added template fields.
func defaultLegacyChildWithCRD(t *testing.T, ms *workload.ModelServing) *workload.ModelServing {
	t.Helper()
	data, err := os.ReadFile("../../../charts/kthena/charts/workload/crds/workload.serving.volcano.sh_modelservings.yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))
	data, err = json.Marshal(ms)
	require.NoError(t, err)
	var object map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &object))
	applyChildSchemaDefaults(t, object, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
	data, err = json.Marshal(object)
	require.NoError(t, err)
	var result workload.ModelServing
	require.NoError(t, json.Unmarshal(data, &result))
	return &result
}

func applyChildSchemaDefaults(t *testing.T, value interface{}, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()
	switch value := value.(type) {
	case map[string]interface{}:
		for name, property := range schema.Properties {
			child, present := value[name]
			if property.Default != nil && (!present || child == nil && !property.Nullable) {
				require.NoError(t, json.Unmarshal(property.Default.Raw, &child))
				value[name] = child
			}
			applyChildSchemaDefaults(t, child, &property)
		}
	case []interface{}:
		if schema.Items != nil && schema.Items.Schema != nil {
			for _, child := range value {
				applyChildSchemaDefaults(t, child, schema.Items.Schema)
			}
		}
	}
}

func TestPreserveLegacyChildWithCRDDefaultedPodTemplates(t *testing.T) {
	container := corev1.Container{Name: "engine", Image: "old", Ports: []corev1.ContainerPort{{ContainerPort: 8100}, {ContainerPort: 8101}},
		Env:            []corev1.EnvVar{{Name: "CONFIG", ValueFrom: &corev1.EnvVarSource{FileKeyRef: &corev1.FileKeySelector{VolumeName: "config", Path: "env", Key: "KEY"}}}, {Name: "USER", Value: "unchanged"}},
		LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 8100}}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 8100}}},
		StartupProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 8100}}},
	}
	pod := corev1.PodSpec{Containers: []corev1.Container{container}, InitContainers: []corev1.Container{container},
		EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "debug", Image: "debug", Ports: container.DeepCopy().Ports, Env: container.DeepCopy().Env,
			LivenessProbe: container.LivenessProbe.DeepCopy(), ReadinessProbe: container.ReadinessProbe.DeepCopy(), StartupProbe: container.StartupProbe.DeepCopy(),
		}}},
		Volumes: []corev1.Volume{
			{Name: "azure", VolumeSource: corev1.VolumeSource{AzureDisk: &corev1.AzureDiskVolumeSource{DiskName: "disk", DataDiskURI: "uri"}}},
			{Name: "iscsi", VolumeSource: corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{TargetPortal: "portal", IQN: "iqn"}}},
			{Name: "rbd", VolumeSource: corev1.VolumeSource{RBD: &corev1.RBDVolumeSource{CephMonitors: []string{"ceph"}, RBDImage: "image"}}},
			{Name: "scaleio", VolumeSource: corev1.VolumeSource{ScaleIO: &corev1.ScaleIOVolumeSource{Gateway: "gateway", System: "system", VolumeName: "volume", SecretRef: &corev1.LocalObjectReference{Name: "secret"}}}},
		},
	}
	generated := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default"}, Spec: workload.ModelServingSpec{
		Replicas: ptr.To[int32](3), Template: workload.ServingGroup{Roles: []workload.Role{{Name: "serve", Replicas: ptr.To[int32](2), WorkerReplicas: 1,
			EntryTemplate: workload.PodTemplateSpec{Spec: *pod.DeepCopy()}, WorkerTemplate: &workload.PodTemplateSpec{Spec: *pod.DeepCopy()},
		}}},
	}}
	old := defaultLegacyChildWithCRD(t, generated)
	require.Empty(t, generated.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports[0].Protocol)
	require.Equal(t, corev1.ProtocolTCP, old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports[0].Protocol)
	require.NotNil(t, old.Spec.Template.Roles[0].WorkerTemplate.Spec.InitContainers[0].ReadinessProbe.GRPC.Service)
	original := old.DeepCopy()
	for _, tc := range []struct {
		name    string
		change  func(*workload.ModelServing)
		allowed bool
	}{
		{"unchanged", func(*workload.ModelServing) {}, true},
		{"SG shrink", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) }, true},
		{"SG zero", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](0) }, true},
		{"Role shrink", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1) }, true},
		{"explicit TCP", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
		}, true},
		{"TCP to UDP", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolUDP
		}, false},
		{"worker init TCP to UDP", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].WorkerTemplate.Spec.InitContainers[0].Ports[0].Protocol = corev1.ProtocolUDP
		}, false},
		{"port order", func(ms *workload.ModelServing) {
			p := ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports
			p[0], p[1] = p[1], p[0]
		}, false},
		{"env order", func(ms *workload.ModelServing) {
			e := ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env
			e[0], e[1] = e[1], e[0]
		}, false},
		{"gRPC service change", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].ReadinessProbe.GRPC.Service = ptr.To("health")
		}, false},
		{"file key optional change", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env[0].ValueFrom.FileKeyRef.Optional = ptr.To(true)
		}, false},
		{"volume filesystem change", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Volumes[0].AzureDisk.FSType = ptr.To("xfs")
		}, false},
		{"unrelated Pod admission default", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.DNSPolicy = corev1.DNSClusterFirst
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := generated.DeepCopy()
			desired.Spec.Plugins = []workload.PluginSpec{{Name: HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}}
			tc.change(desired)
			err := PreserveChildPluginConfiguration(old, desired)
			if tc.allowed {
				require.NoError(t, err)
				require.Empty(t, desired.Spec.Plugins)
				for i := range old.Spec.Template.Roles {
					require.Equal(t, old.Spec.Template.Roles[i].EntryTemplate, desired.Spec.Template.Roles[i].EntryTemplate)
					require.Equal(t, old.Spec.Template.Roles[i].WorkerTemplate, desired.Spec.Template.Roles[i].WorkerTemplate)
				}
			} else {
				require.ErrorContains(t, err, "MigrationRequired")
			}
			require.Equal(t, original, old, "stored object must not be mutated")
		})
	}
}

func TestPreserveChildWithHeadlessPluginThroughWorkerTransition(t *testing.T) {
	old := &workload.ModelServing{Spec: workload.ModelServingSpec{Plugins: []workload.PluginSpec{{Name: HeadlessServicePluginName}}, Template: workload.ServingGroup{Roles: []workload.Role{{Name: "serve"}}}}}
	desired := old.DeepCopy()
	desired.Spec.Plugins[0].Type = workload.PluginTypeBuiltIn
	desired.Spec.Template.Roles[0].WorkerReplicas = 2
	require.NoError(t, PreserveChildPluginConfiguration(old, desired))
	assert.Equal(t, old.Spec.Plugins, desired.Spec.Plugins)
	assert.Equal(t, int32(2), desired.Spec.Template.Roles[0].WorkerReplicas)
	desired.Spec.Plugins = nil
	require.ErrorContains(t, PreserveChildPluginConfiguration(old, desired), "immutable plugin")
}
