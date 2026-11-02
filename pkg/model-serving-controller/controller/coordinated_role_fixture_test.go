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
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func recordDifferentRevision(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, revision string) {
	t.Helper()
	old := ms.DeepCopy()
	if len(old.Spec.Template.Roles) == 0 {
		old.Spec.Template.Roles = []workloadv1alpha1.Role{{Name: "historical-role"}}
	}
	for i := range old.Spec.Template.Roles {
		old.Spec.Template.Roles[i].EntryTemplate.Metadata = &workloadv1alpha1.Metadata{
			Annotations: map[string]string{"test.kthena.io/template-version": revision},
		}
	}
	_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, revision, old.Spec.Template.Roles)
	require.NoError(t, err)
}

func newRevisionTestController(t *testing.T, ms *workloadv1alpha1.ModelServing) *ModelServingController {
	t.Helper()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(ms.DeepCopy()), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	_, err = utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, utils.ModelServingRevision(ms), ms.Spec.Template.Roles)
	require.NoError(t, err)
	t.Cleanup(c.workqueue.ShutDown)
	return c
}
