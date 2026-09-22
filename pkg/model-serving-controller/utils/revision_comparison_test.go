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

package utils

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestEqualRevisionHashAndSemantic(t *testing.T) {
	type projection struct {
		CPU resource.Quantity `json:"cpu"`
	}
	for _, tc := range []struct {
		name, lhsHash, rhsHash, lhs, rhs string
		equal, invalid                   bool
	}{
		{"fast identity", "same", "same", `{"cpu":"1"}`, `{"cpu":"1"}`, true, false},
		{"hash drift", "old", "new", `{"cpu":"1"}`, `{"cpu":"1"}`, true, false},
		{"semantic quantities", "old", "new", `{"cpu":"1"}`, `{"cpu":"1000m"}`, true, false},
		{"hash collision", "same", "same", `{"cpu":"1"}`, `{"cpu":"2"}`, false, false},
		{"real change", "old", "new", `{"cpu":"1"}`, `{"cpu":"2"}`, false, false},
		{"invalid observed", "old", "new", `{`, `{"cpu":"1"}`, false, true},
		{"invalid desired", "old", "new", `{"cpu":"1"}`, `{`, false, true},
		{"invalid matching bytes", "same", "same", `{`, `{`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			equal, err := EqualRevision[projection](tc.lhsHash, tc.rhsHash, []byte(tc.lhs), []byte(tc.rhs))
			require.Equal(t, tc.invalid, err != nil)
			require.Equal(t, tc.equal, equal)
		})
	}
}

func TestRevisionProjectionsTrackApplicablePluginsWithoutMutatingSpec(t *testing.T) {
	ms := revisionTestModelServing(revisionTestRole("prefill", "image"), revisionTestRole("decode", "image"))
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
		Name: "demo", Type: workloadv1alpha1.PluginTypeBuiltIn,
		Config: &apiextensionsv1.JSON{Raw: []byte(`{"annotations":{"version":"one"}}`)},
		Scope:  &workloadv1alpha1.PluginScope{Roles: []string{"decode"}},
	}}
	before := ms.DeepCopy()
	for _, tc := range []struct {
		name                                  string
		mutate                                func(*workloadv1alpha1.ModelServing)
		modelEqual, prefillEqual, decodeEqual bool
	}{
		{"unchanged", func(*workloadv1alpha1.ModelServing) {}, true, true, true},
		{"plugin config", func(m *workloadv1alpha1.ModelServing) {
			m.Spec.Plugins[0].Config.Raw = []byte(`{"annotations":{"version":"two"}}`)
		}, false, true, false},
		{"plugin removal", func(m *workloadv1alpha1.ModelServing) { m.Spec.Plugins = nil }, false, true, false},
		{"scheduler", func(m *workloadv1alpha1.ModelServing) { m.Spec.SchedulerName = "other" }, false, false, false},
		{"effective Pod default", func(m *workloadv1alpha1.ModelServing) { m.Spec.SchedulerName = defaultSchedulerName }, true, true, true},
		{"scope normalization", func(m *workloadv1alpha1.ModelServing) { m.Spec.Plugins[0].Scope.Roles = []string{"decode", "decode"} }, true, true, true},
		{"scope adds another Role", func(m *workloadv1alpha1.ModelServing) { m.Spec.Plugins[0].Scope.Roles = []string{"decode", "prefill"} }, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := ms.DeepCopy()
			tc.mutate(current)
			original := current.DeepCopy()
			for i := 0; i < 2; i++ {
				equal, err := EqualModelServingRevisions(ModelServingRevision(ms), ModelServingRevision(current), ms, current)
				require.NoError(t, err)
				require.Equal(t, tc.modelEqual, equal)
				for role, expected := range map[string]bool{"prefill": tc.prefillEqual, "decode": tc.decodeEqual} {
					leftHash, err := RoleRevisionHash(ms, role)
					require.NoError(t, err)
					rightHash, err := RoleRevisionHash(current, role)
					require.NoError(t, err)
					equal, err := EqualRoleRevisions(leftHash, rightHash, ms, current, role)
					require.NoError(t, err)
					require.Equal(t, expected, equal, role)
					require.Equal(t, expected, leftHash == rightHash, role)
				}
			}
			require.True(t, reflect.DeepEqual(original.Spec, current.Spec))
		})
	}
	require.True(t, reflect.DeepEqual(before.Spec, ms.Spec))
}

func TestInactivePluginChangeReusesModelServingHistory(t *testing.T) {
	ms := revisionTestModelServing(revisionTestRole("decode", "image"))
	ms.Spec.Template.Roles[0].WorkerReplicas = 0
	ms.Spec.Template.Roles[0].WorkerTemplate = nil
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
		Name: "demo", Scope: &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetWorker},
		Config: &apiextensionsv1.JSON{Raw: []byte(`{"annotations":{"value":"one"}}`)},
	}}
	client := kubefake.NewSimpleClientset()
	before, err := BuildRevisionData(ms)
	require.NoError(t, err)
	original, _, err := RecordModelServingRevision(context.Background(), client, ms, before)
	require.NoError(t, err)
	ms.Spec.Plugins[0].Config.Raw = []byte(`{"annotations":{"value":"two"}}`)
	after, err := BuildRevisionData(ms)
	require.NoError(t, err)
	require.NotEqual(t, before, after, "snapshot inputs changed, but no existing Pod is affected")
	reused, _, err := RecordModelServingRevision(context.Background(), client, ms, after)
	require.NoError(t, err)
	require.Equal(t, original.Name, reused.Name)
	require.Equal(t, original.Data.Raw, reused.Data.Raw)
}

func TestInactiveWorkerTemplateDoesNotRoll(t *testing.T) {
	ms := revisionTestModelServing(revisionTestRole("decode", "image"))
	ms.Spec.Template.Roles[0].WorkerReplicas = 0
	changed := ms.DeepCopy()
	changed.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "unused-change"
	equal, err := EqualModelServingRevisions(ModelServingRevision(ms), ModelServingRevision(changed), ms, changed)
	require.NoError(t, err)
	require.True(t, equal)
	left, err := RoleRevisionHash(ms, "decode")
	require.NoError(t, err)
	right, err := RoleRevisionHash(changed, "decode")
	require.NoError(t, err)
	require.Equal(t, left, right)
	changed.Spec.Template.Roles[0].WorkerReplicas = 1
	equal, err = EqualModelServingRevisions(ModelServingRevision(ms), ModelServingRevision(changed), ms, changed)
	require.NoError(t, err)
	require.False(t, equal)
}

func TestLegacyRevisionBaselineIsDurableAndImmutable(t *testing.T) {
	ctx := context.Background()
	ms := revisionTestModelServing(revisionTestRole("decode", "image"))
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "original"}}
	client := kubefake.NewSimpleClientset()
	source, err := CreateControllerRevision(ctx, client, ms, "legacy", ms.Spec.Template.Roles)
	require.NoError(t, err)
	baseline, err := EnsureRevisionBaseline(ctx, client, ms, source)
	require.NoError(t, err)
	unchanged, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, source.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, source.Data, unchanged.Data)
	require.Equal(t, source.UID, unchanged.UID)
	require.Equal(t, source.Labels, unchanged.Labels)
	require.Equal(t, source.Revision, unchanged.Revision)
	require.Equal(t, baseline.Name, unchanged.Annotations[legacyRevisionBaseline])
	require.Equal(t, "legacy", baseline.Labels[ControllerRevisionRevisionLabelKey])

	ms.Spec.Plugins[0].Name = "changed-after-migration"
	reloaded, err := EnsureRevisionBaseline(ctx, client, ms, source)
	require.NoError(t, err)
	require.Equal(t, baseline, reloaded, "a restart must not redefine the compatibility baseline")
	historical, err := ModelServingForControllerRevision(ms, reloaded)
	require.NoError(t, err)
	require.Equal(t, "original", historical.Spec.Plugins[0].Name)
	equal, err := EqualModelServingRevisions("legacy", ModelServingRevision(ms), historical, ms)
	require.NoError(t, err)
	require.False(t, equal, "subsequent plugin edits must not be masked by migration")

	source.UID = "recreated-source"
	_, err = EnsureRevisionBaseline(ctx, client, ms, source)
	require.ErrorContains(t, err, "conflicting identity")

	require.NoError(t, client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, baseline.Name, metav1.DeleteOptions{}))
	_, err = EnsureRevisionBaseline(ctx, client, ms, unchanged)
	require.ErrorContains(t, err, "previously established legacy baseline")
}
