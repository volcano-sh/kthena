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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type overwritePodIdentityPlugin struct{}

func (overwritePodIdentityPlugin) Name() string { return "overwrite-pod-identity" }

func (overwritePodIdentityPlugin) OnPodCreate(_ context.Context, req *plugins.HookRequest) error {
	for _, key := range []string{
		workloadv1alpha1.ModelServingNameLabelKey,
		workloadv1alpha1.GroupNameLabelKey,
		workloadv1alpha1.RoleLabelKey,
		workloadv1alpha1.RoleIDKey,
		workloadv1alpha1.EntryLabelKey,
		workloadv1alpha1.RevisionLabelKey,
		workloadv1alpha1.RoleTemplateHashLabelKey,
	} {
		req.Pod.Labels[key] = "plugin-value"
	}
	controller := true
	req.Pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "plugin-owner",
		UID:        types.UID("plugin-owner"),
		Controller: &controller,
	}}
	return nil
}

func (overwritePodIdentityPlugin) OnPodReady(context.Context, *plugins.HookRequest) error {
	return nil
}

func TestCreatePodRestoresReservedMetadataAfterTemplateAndPlugin(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reserved-metadata",
			Namespace: "default",
			UID:       types.UID("model-serving-uid"),
		},
	}
	groupName := "reserved-metadata-0"
	roleName := "decode"
	roleID := "decode-0"
	revision := "revision-1"
	roleTemplateHash := "role-hash-1"
	reserved := map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: "template-value",
		workloadv1alpha1.GroupNameLabelKey:        "template-value",
		workloadv1alpha1.RoleLabelKey:             "template-value",
		workloadv1alpha1.RoleIDKey:                "template-value",
		workloadv1alpha1.EntryLabelKey:            "template-value",
		workloadv1alpha1.RevisionLabelKey:         "template-value",
		workloadv1alpha1.RoleTemplateHashLabelKey: "template-value",
	}
	role := workloadv1alpha1.Role{
		Name:     roleName,
		Replicas: ptr.To[int32](1),
		EntryTemplate: workloadv1alpha1.PodTemplateSpec{
			Metadata: &workloadv1alpha1.Metadata{Labels: reserved},
			Spec:     corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "image:v1"}}},
		},
	}
	ms.Spec.Template.Roles = []workloadv1alpha1.Role{role}
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
		Name: "overwrite-pod-identity",
		Type: workloadv1alpha1.PluginTypeBuiltIn,
	}}

	kubeClient := kubefake.NewSimpleClientset()
	controller := &ModelServingController{
		kubeClientSet:   kubeClient,
		pluginsRegistry: plugins.NewRegistry(),
	}
	controller.pluginsRegistry.Register("overwrite-pod-identity", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
		return overwritePodIdentityPlugin{}, nil
	})
	chain, err := controller.buildPluginChain(ms)
	require.NoError(t, err)
	pod := utils.GenerateEntryPod(role, ms, groupName, roleID, revision, roleTemplateHash)
	require.NoError(t, controller.createPod(ctx, ms, groupName, roleName, roleID, revision, roleTemplateHash, pod, true, chain, "entry"))

	created, err := kubeClient.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	for key, value := range map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.GroupNameLabelKey:        groupName,
		workloadv1alpha1.RoleLabelKey:             roleName,
		workloadv1alpha1.RoleIDKey:                roleID,
		workloadv1alpha1.EntryLabelKey:            utils.Entry,
		workloadv1alpha1.RevisionLabelKey:         revision,
		workloadv1alpha1.RoleTemplateHashLabelKey: roleTemplateHash,
	} {
		assert.Equal(t, value, created.Labels[key])
	}
	require.Len(t, created.OwnerReferences, 1)
	owner := metav1.GetControllerOf(created)
	require.NotNil(t, owner)
	assert.Equal(t, workloadv1alpha1.ModelServingKind.GroupVersion().String(), owner.APIVersion)
	assert.Equal(t, workloadv1alpha1.ModelServingKind.Kind, owner.Kind)
	assert.Equal(t, ms.Name, owner.Name)
	assert.Equal(t, ms.UID, owner.UID)
}
