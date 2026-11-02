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
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/convert"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestModelBoosterLegacyChildUpdates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*workload.ModelBooster)
		allowed bool
		update  bool
	}{
		{"unchanged sync", func(*workload.ModelBooster) {}, true, false},
		{"parent metadata", func(m *workload.ModelBooster) { m.Annotations = map[string]string{"note": "changed"} }, true, false},
		{"SG shrink", func(m *workload.ModelBooster) { m.Spec.Backend.Replicas = 1 }, true, true},
		{"SG zero", func(m *workload.ModelBooster) { m.Spec.Backend.Replicas = 0 }, true, true},
		{"SG expand", func(m *workload.ModelBooster) { m.Spec.Backend.Replicas = 4 }, false, false},
		{"image plus shrink", func(m *workload.ModelBooster) {
			m.Spec.Backend.Replicas = 1
			m.Spec.Backend.Workers[0].Image = "different"
		}, false, false},
		{"worker shrink", func(m *workload.ModelBooster) { m.Spec.Backend.Workers[0].Pods = 1 }, false, false},
		// The old generated minRoleReplicas equals its Role count. Changing it
		// is immutable, while retaining it would exceed the new Role count.
		{"Role shrink changes immutable gang minimum", func(m *workload.ModelBooster) { m.Spec.Backend.Workers[0].Replicas = 1 }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := loadYaml[workload.ModelBooster](t, "../convert/testdata/input/model.yaml")
			model.Spec.Backend.Replicas = 3
			model.Spec.Backend.Workers[0].Replicas = 2
			old, err := convert.BuildModelServing(model)
			require.NoError(t, err)
			old.Spec.Plugins = nil
			// The real ModelServing CRD defaults the generated runtime port.
			// The converter omits protocol, including during unchanged sync.
			require.NotEmpty(t, old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports)
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Ports[0].Protocol = corev1.ProtocolTCP
			old.UID = "legacy-child"
			old.ResourceVersion = "1"
			original := old.DeepCopy()
			client := kthenafake.NewSimpleClientset(old)
			controller := NewModelBoosterController(fake.NewClientset(), client)
			require.NoError(t, controller.modelServingInformer.GetIndexer().Add(old))
			tc.change(model)
			err = controller.createOrUpdateModelServing(context.Background(), model)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "MigrationRequired")
			}
			current, err := client.WorkloadV1alpha1().ModelServings(old.Namespace).Get(context.Background(), old.Name, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, old.UID, current.UID)
			assert.Empty(t, current.Spec.Plugins)
			assert.Equal(t, original, old, "cached child must not be mutated")
			updates := 0
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "modelservings" {
					continue
				}
				assert.NotEqual(t, "create", action.GetVerb())
				assert.NotEqual(t, "delete", action.GetVerb())
				if action.GetVerb() == "update" {
					updates++
				}
			}
			if tc.update {
				assert.Equal(t, 1, updates)
				assert.Equal(t, model.Spec.Backend.Replicas, *current.Spec.Replicas)
				assert.Equal(t, original.Spec.Template, current.Spec.Template, "safe shrink must not roll Pod templates")
				assert.NotEqual(t, old.Labels[utils.RevisionLabelKey], current.Labels[utils.RevisionLabelKey])
			} else {
				assert.Zero(t, updates)
				assert.Equal(t, original.Spec, current.Spec)
			}
		})
	}
}

func TestModelBoosterLegacyUnusedWorkerTemplateDoesNotBlockSync(t *testing.T) {
	model := loadYaml[workload.ModelBooster](t, "../convert/testdata/input/model.yaml")
	model.Spec.Backend.Workers[0].Pods = 1
	old, err := convert.BuildModelServing(model)
	require.NoError(t, err)
	old.Spec.Plugins = nil
	old.Spec.Template.Roles[0].WorkerTemplate = &workload.PodTemplateSpec{}
	client := kthenafake.NewSimpleClientset(old)
	controller := NewModelBoosterController(fake.NewClientset(), client)
	require.NoError(t, controller.modelServingInformer.GetIndexer().Add(old))
	require.NoError(t, controller.createOrUpdateModelServing(context.Background(), model))
	for _, action := range client.Actions() {
		assert.NotEqual(t, "update", action.GetVerb())
		assert.NotEqual(t, "create", action.GetVerb())
		assert.NotEqual(t, "delete", action.GetVerb())
	}
}
