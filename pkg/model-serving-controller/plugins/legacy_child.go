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
	"bytes"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// PreserveChildPluginConfiguration keeps a parent controller from changing an
// existing child's immutable plugin declaration. Legacy children without
// headless discovery can still synchronize unchanged intent or safely reduce
// SG/Role counts, but must migrate before requesting new workload capacity or a
// template change. This does not restore implicit DNS for a no-plugin child.
func PreserveChildPluginConfiguration(old, desired *workload.ModelServing) error {
	hasHeadless := false
	for _, plugin := range old.Spec.Plugins {
		if plugin.Name == HeadlessServicePluginName {
			hasHeadless = true
		}
	}
	expectedPlugins := old.DeepCopy().Spec.Plugins
	if !hasHeadless {
		expectedPlugins = append(expectedPlugins, workload.PluginSpec{Name: HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn})
	}
	expectedData, err := utils.BuildRevisionData(&workload.ModelServing{Spec: workload.ModelServingSpec{Plugins: expectedPlugins}})
	if err != nil {
		return err
	}
	desiredData, err := utils.BuildRevisionData(&workload.ModelServing{Spec: workload.ModelServingSpec{Plugins: desired.Spec.Plugins}})
	if err != nil {
		return err
	}
	if !bytes.Equal(expectedData, desiredData) {
		return fmt.Errorf("MigrationRequired: ModelServing %s/%s has a different immutable plugin declaration; create a new parent workload and migrate traffic instead of changing plugins", old.Namespace, old.Name)
	}
	desired.Spec.Plugins = old.DeepCopy().Spec.Plugins
	if hasHeadless {
		return nil
	}

	previous, next := old.DeepCopy(), desired.DeepCopy()
	normalizeChildDefaults(previous)
	normalizeChildDefaults(next)
	countsOnly := *next.Spec.Replicas <= *previous.Spec.Replicas && len(next.Spec.Template.Roles) == len(previous.Spec.Template.Roles)
	if countsOnly {
		next.Spec.Replicas = previous.Spec.Replicas
		for i := range next.Spec.Template.Roles {
			a, b := &previous.Spec.Template.Roles[i], &next.Spec.Template.Roles[i]
			if a.Name != b.Name || *b.Replicas > *a.Replicas {
				countsOnly = false
				break
			}
			b.Replicas = a.Replicas
		}
	}
	if !countsOnly || !apiequality.Semantic.DeepEqual(previous.Spec, next.Spec) {
		return fmt.Errorf("MigrationRequired: ModelServing %s/%s was created without %s; preserve its plugin declaration and create a new parent workload for DNS-dependent capacity or template changes, then migrate traffic", old.Namespace, old.Name, HeadlessServicePluginName)
	}

	// Retain stored representations and defaults, avoiding a no-op update loop.
	// Only the explicitly requested counts change; metadata remains with the caller.
	spec := old.DeepCopy().Spec
	if ptr.Deref(desired.Spec.Replicas, 1) != ptr.Deref(spec.Replicas, 1) {
		spec.Replicas = desired.Spec.Replicas
	}
	for i := range spec.Template.Roles {
		for _, role := range desired.Spec.Template.Roles {
			if role.Name == spec.Template.Roles[i].Name {
				if ptr.Deref(role.Replicas, 1) != ptr.Deref(spec.Template.Roles[i].Replicas, 1) {
					spec.Template.Roles[i].Replicas = role.Replicas
				}
				break
			}
		}
	}
	desired.Spec = spec
	return nil
}

// Normalize only defaults in the ModelServing CRD, including its embedded Pod
// schema. Do not apply built-in Pod admission defaults or erase template edits.
func normalizeChildDefaults(ms *workload.ModelServing) {
	if ms.Spec.Replicas == nil {
		ms.Spec.Replicas = ptr.To[int32](1)
	}
	if ms.Spec.SchedulerName == "" {
		ms.Spec.SchedulerName = "volcano"
	}
	if ms.Spec.RecoveryPolicy == "" {
		ms.Spec.RecoveryPolicy = workload.RoleRecreate
	}
	if ms.Spec.RevisionHistoryLimit == nil {
		ms.Spec.RevisionHistoryLimit = ptr.To[int32](10)
	}
	if ms.Spec.Template.RestartGracePeriodSeconds == nil {
		ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](0)
	}
	for i := range ms.Spec.Template.Roles {
		role := &ms.Spec.Template.Roles[i]
		if role.Replicas == nil {
			role.Replicas = ptr.To[int32](1)
		}
		if role.MaxUnavailable == nil {
			role.MaxUnavailable = ptr.To(intstr.FromInt32(1))
		}
		if role.MaxSurge == nil {
			role.MaxSurge = ptr.To(intstr.FromInt32(0))
		}
		if role.Partition == nil {
			role.Partition = ptr.To(intstr.FromInt32(0))
		}
		normalizeChildPodSchemaDefaults(&role.EntryTemplate.Spec)
		if role.WorkerTemplate != nil {
			normalizeChildPodSchemaDefaults(&role.WorkerTemplate.Spec)
		}
	}
	slices.SortFunc(ms.Spec.Template.Roles, func(a, b workload.Role) int { return strings.Compare(a.Name, b.Name) })
}

func normalizeChildPodSchemaDefaults(spec *corev1.PodSpec) {
	normalizeContainer := func(ports []corev1.ContainerPort, env []corev1.EnvVar, probes ...*corev1.Probe) {
		for i := range ports {
			if ports[i].Protocol == "" {
				ports[i].Protocol = corev1.ProtocolTCP
			}
		}
		for i := range env {
			if source := env[i].ValueFrom; source != nil && source.FileKeyRef != nil && source.FileKeyRef.Optional == nil {
				source.FileKeyRef.Optional = ptr.To(false)
			}
		}
		for _, probe := range probes {
			if probe != nil && probe.GRPC != nil && probe.GRPC.Service == nil {
				probe.GRPC.Service = ptr.To("")
			}
		}
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		normalizeContainer(c.Ports, c.Env, c.LivenessProbe, c.ReadinessProbe, c.StartupProbe)
	}
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		normalizeContainer(c.Ports, c.Env, c.LivenessProbe, c.ReadinessProbe, c.StartupProbe)
	}
	for i := range spec.EphemeralContainers {
		c := &spec.EphemeralContainers[i]
		normalizeContainer(c.Ports, c.Env, c.LivenessProbe, c.ReadinessProbe, c.StartupProbe)
	}
	for i := range spec.Volumes {
		v := &spec.Volumes[i]
		if v.AzureDisk != nil {
			if v.AzureDisk.FSType == nil {
				v.AzureDisk.FSType = ptr.To("ext4")
			}
			if v.AzureDisk.ReadOnly == nil {
				v.AzureDisk.ReadOnly = ptr.To(false)
			}
		}
		if v.ISCSI != nil && v.ISCSI.ISCSIInterface == "" {
			v.ISCSI.ISCSIInterface = "default"
		}
		if v.RBD != nil {
			if v.RBD.RBDPool == "" {
				v.RBD.RBDPool = "rbd"
			}
			if v.RBD.RadosUser == "" {
				v.RBD.RadosUser = "admin"
			}
			if v.RBD.Keyring == "" {
				v.RBD.Keyring = "/etc/ceph/keyring"
			}
		}
		if v.ScaleIO != nil {
			if v.ScaleIO.FSType == "" {
				v.ScaleIO.FSType = "xfs"
			}
			if v.ScaleIO.StorageMode == "" {
				v.ScaleIO.StorageMode = "ThinProvisioned"
			}
		}
	}
}
