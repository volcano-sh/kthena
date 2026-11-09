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

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCoordinatedHistoryKeepsRecordedReplicaCounts(t *testing.T) {
	for _, format := range []string{"legacy", "annotated legacy", "canonical"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			ms := mergeReadyModelServing("recorded-counts", 1, 4)
			c := newRevisionTestController(t, ms)
			old := ms.DeepCopy()
			for i := range old.Spec.Template.Roles {
				old.Spec.Template.Roles[i].Replicas = ptr.To[int32](2)
				old.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "old-image"
			}
			if format == "canonical" {
				recordTestRevision(t, c.kubeClientSet, old, "historical")
			} else {
				_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, old, "historical", old.Spec.Template.Roles)
				require.NoError(t, err)
			}
			source, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, "historical")
			require.NoError(t, err)
			wantOld := int32(2)
			if format == "annotated legacy" {
				source.Annotations = map[string]string{coordinatedRoleReplicaBaselineAnnotation: `{"decode":3,"prefill":3}`}
				source, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, source, metav1.UpdateOptions{})
				require.NoError(t, err)
				wantOld = 3
			}
			observation := c.withRevisionHistory(withCoordinatedRound(ctx, ms), ms)
			roles, err := c.roleSpecsFromRevisionInRound(observation, ms, "historical")
			require.NoError(t, err)
			rendering, err := c.modelServingForRevision(observation, ms, "historical")
			require.NoError(t, err)
			for _, role := range rendering.Spec.Template.Roles {
				require.EqualValues(t, 4, *role.Replicas, "rendering follows current desired counts")
				require.Equal(t, wantOld, *roles[role.Name].Replicas, "coordination uses the recorded population")
				require.Equal(t, "old-image", role.EntryTemplate.Spec.Containers[0].Image)
			}
			unchanged, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, "historical")
			require.NoError(t, err)
			require.Equal(t, source, unchanged)
			if format == "canonical" {
				delete(source.Annotations, coordinatedRoleReplicaBaselineAnnotation)
				_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, source, metav1.UpdateOptions{})
				require.NoError(t, err)
				_, err = c.readCoordinatedRevision(ctx, ms, "historical")
				require.ErrorContains(t, err, "no coordinated Role replica baseline", "a missing count must not become the current population")
			}
			require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, source.Name, metav1.DeleteOptions{}))
			_, err = c.readCoordinatedRevision(ctx, ms, "historical")
			require.Error(t, err, "a missing source must not be reconstructed from current counts")
		})
	}
}

func TestCanonicalCoordinatedHistoryReusesLegacyIdentityAndAnchorsLiveEntry(t *testing.T) {
	ms := mergeReadyModelServing("canonical-alias", 1, 1)
	c := newRevisionTestController(t, ms)
	require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(context.Background(), utils.GenerateControllerRevisionName(ms.Name, utils.ModelServingRevision(ms)), metav1.DeleteOptions{}))
	old := ms.DeepCopy()
	// Core-owned environment spelling differs, but rendered inputs are equal.
	for i := range old.Spec.Template.Roles {
		old.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: api.GroupSizeEnv, Value: "obsolete-user-value"}}
	}
	_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, old, "legacy-alias", old.Spec.Template.Roles)
	require.NoError(t, err)
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "legacy-alias", "legacy-alias"
	ctx := c.withRevisionHistory(withCoordinatedRound(context.Background(), ms), ms)
	target, err := c.revisionHistory(ctx, ms).desiredRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, "legacy-alias", target)
	require.Equal(t, target, c.revisionTarget(ctx, ms))
	require.NoError(t, c.persistCoordinatedRoleRevision(ctx, ms, target))
	group := ms.Name + "-0"
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, target)
	for _, role := range old.Spec.Template.Roles {
		addMergeReadyInstance(t, c, ms, group, role, 0, target, "stale-missing", "stale-hash")
	}
	observations := withCoordinatedRound(ctx, ms)
	for _, role := range ms.Spec.Template.Roles {
		instance := datastore.Role{Name: utils.GenerateRoleID(role.Name, 0), Revision: "stale-missing", RoleTemplateHash: "stale-hash", Status: datastore.RoleRunning}
		require.Equal(t, templateEquivalent, c.compareRoleTemplate(observations, ms, datastore.ServingGroup{Name: group, Revision: target}, role.Name, instance), "live entry identity must precede canonical history comparison")
	}
	policy, err := c.resolveRoleRolloutPolicy(observations, ms, target)
	require.NoError(t, err)
	require.Empty(t, policy.groups, "semantically equal legacy identity must not initiate coordinated replacement")
}
