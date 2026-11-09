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
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestLegacyRevisionKeepsRecordedReplicaMetadata(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy counts", true: "existing annotation"}[explicit], func(t *testing.T) {
			ctx := context.Background()
			ms := revisionTestModelServing(revisionTestRole("decode", "image"))
			ms.ObjectMeta = metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "owner"}
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate, RoleCoordination: &workloadv1alpha1.RoleCoordination{}}
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](4)
			old := ms.DeepCopy()
			old.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old-image"
			client := kubefake.NewSimpleClientset()
			source, err := CreateControllerRevision(ctx, client, ms, "old", old.Spec.Template.Roles)
			require.NoError(t, err)
			source.UID = "source-uid"
			if explicit {
				source.Annotations = map[string]string{CoordinatedRoleReplicaBaselineAnnotation: `{"decode":3}`}
			}
			source, err = client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, source, metav1.UpdateOptions{})
			require.NoError(t, err)
			original := source.DeepCopy()
			client.ClearActions()
			for i := 0; i < 2; i++ {
				reloaded, err := GetControllerRevision(ctx, client, ms, "old")
				require.NoError(t, err)
				require.Equal(t, original, reloaded, "reading history must not pin or rewrite its source")
				recorded, err := GetRolesFromControllerRevision(reloaded)
				require.NoError(t, err)
				require.Len(t, recorded, 1)
				require.EqualValues(t, 2, *recorded[0].Replicas, "legacy Data retains its recorded population")
				rendered, err := ModelServingForControllerRevision(ms, reloaded)
				require.NoError(t, err)
				require.EqualValues(t, 4, *rendered.Spec.Template.Roles[0].Replicas, "rendering follows current operational counts")
				require.Equal(t, "old-image", rendered.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)
				require.Equal(t, original, reloaded, "decoding must also leave annotation counts unchanged")
			}
			all, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, all.Items, 1, "legacy reads must not create an auxiliary revision")
			require.Equal(t, source.Name, all.Items[0].Name)
			for _, action := range client.Actions() {
				require.Contains(t, []string{"get", "list"}, action.GetVerb(), "reading history must not write metadata or objects")
			}
		})
	}
}

func TestZeroWorkerHeadlessTemplatePresence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		headless  bool
		excluded  bool
		present   bool
		wantEqual bool
	}{
		{name: "no plugin ignores unused template", wantEqual: true},
		{name: "headless entry starts using generated DNS", headless: true},
		{name: "headless outside Role scope", headless: true, excluded: true, wantEqual: true},
		{name: "headless ignores unused template contents", headless: true, present: true, wantEqual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := revisionTestModelServing(revisionTestRole("decode", "image"))
			old.Spec.Template.Roles[0].WorkerReplicas = 0
			old.Spec.Template.Roles[0].WorkerTemplate = nil
			if tc.headless {
				old.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "headless-service"}}
				if tc.excluded {
					old.Spec.Plugins[0].Scope = &workloadv1alpha1.PluginScope{Roles: []string{"other"}}
				}
			}
			if tc.present {
				old.Spec.Template.Roles[0].WorkerTemplate = old.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			}
			current := old.DeepCopy()
			current.Spec.Template.Roles[0].WorkerTemplate = current.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			current.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "unused:new"
			before := current.DeepCopy()
			for _, pair := range [][2]*workloadv1alpha1.ModelServing{{old, current}, {current, old}} {
				equal, err := EqualModelServingRevisions(ModelServingRevision(pair[0]), ModelServingRevision(pair[1]), pair[0], pair[1])
				require.NoError(t, err)
				require.Equal(t, tc.wantEqual, equal, "whole workload")
				left, err := RoleRevisionHash(pair[0], "decode")
				require.NoError(t, err)
				right, err := RoleRevisionHash(pair[1], "decode")
				require.NoError(t, err)
				equal, err = EqualRoleRevisions(left, right, pair[0], pair[1], "decode")
				require.NoError(t, err)
				require.Equal(t, tc.wantEqual, equal, "Role projection")
				require.Equal(t, tc.wantEqual, left == right)
			}
			require.Equal(t, before.Spec, current.Spec)
		})
	}
}

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
	// These are defensive comparisons of historical or admission-bypassed
	// objects, not examples of permitted plugin or scheduler updates.
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
	// Admission rejects this plugin edit even without workers. The historical
	// comparator still describes effective Pod inputs rather than API mutability.
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

func TestModelServingForControllerRevisionRejectsUnusableSources(t *testing.T) {
	ctx := context.Background()
	ms := revisionTestModelServing(revisionTestRole("decode", "image"))
	ms.ObjectMeta = metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "owner"}
	client := kubefake.NewSimpleClientset()
	source, err := CreateControllerRevision(ctx, client, ms, "legacy", ms.Spec.Template.Roles)
	require.NoError(t, err)
	for _, scenario := range []string{"nil owner", "nil source", "nil data", "empty data", "malformed data", "empty roles", "null roles", "wrapped empty roles", "wrapped null roles", "unknown version", "foreign UID", "wrong owner name", "wrong owner kind", "wrong owner API", "not controller", "wrong namespace"} {
		t.Run(scenario, func(t *testing.T) {
			owner, cr := ms.DeepCopy(), source.DeepCopy()
			switch scenario {
			case "nil owner":
				owner = nil
			case "nil source":
				cr = nil
			case "nil data":
				cr.Data.Raw = nil
			case "empty data":
				cr.Data.Raw = []byte{}
			case "malformed data":
				cr.Data.Raw = []byte(`{`)
			case "empty roles":
				cr.Data.Raw = []byte(`[]`)
			case "null roles":
				cr.Data.Raw = []byte(`null`)
			case "wrapped empty roles":
				cr.Data.Raw = []byte(`{"data":[]}`)
			case "wrapped null roles":
				cr.Data.Raw = []byte(`{"data":null}`)
			case "unknown version":
				cr.Annotations = map[string]string{ControllerRevisionDataVersionAnnotation: "future"}
			case "foreign UID":
				cr.OwnerReferences[0].UID = "other"
			case "wrong owner name":
				cr.OwnerReferences[0].Name = "other"
			case "wrong owner kind":
				cr.OwnerReferences[0].Kind = "Other"
			case "wrong owner API":
				cr.OwnerReferences[0].APIVersion = "other/v1"
			case "not controller":
				cr.OwnerReferences[0].Controller = nil
			case "wrong namespace":
				cr.Namespace = "other"
			}
			_, err := ModelServingForControllerRevision(owner, cr)
			require.Error(t, err)
			if scenario == "unknown version" {
				require.ErrorContains(t, err, "unsupported revision data version", "otherwise valid wrapped Roles cannot bypass the format guard")
			}
		})
	}
}
