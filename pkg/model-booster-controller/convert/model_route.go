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

package convert

import (
	networking "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	icUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BuildModelRoute(model *workload.ModelBooster) *networking.ModelRoute {
	routeName := model.Name
	rules := getRules(model)
	route := &networking.ModelRoute{
		TypeMeta: metav1.TypeMeta{
			Kind:       networking.ModelRouteKind,
			APIVersion: networking.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: model.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workload.GroupVersion.String(),
					Kind:       workload.ModelKind.Kind,
					Name:       model.Name,
					UID:        model.UID,
				},
			},
		},
		Spec: networking.ModelRouteSpec{
			ModelName: model.Name,
			Rules:     rules,
		},
	}
	route.Labels = utils.GetModelControllerLabels(model, "", icUtils.Revision(route.Spec))
	return route
}

// getRules generates routing rules based on the model's backend.
func getRules(model *workload.ModelBooster) []*networking.Rule {
	targetModels := getTargetModels(model)

	var rules []*networking.Rule
	rules = append(rules, &networking.Rule{
		Name:         modelRouteRuleName,
		ModelMatch:   model.Spec.ModelMatch,
		TargetModels: targetModels,
	})
	return rules
}

// getTargetModels returns the target models.
func getTargetModels(model *workload.ModelBooster) []*networking.TargetModel {
	var targetModels []*networking.TargetModel
	backend := model.Spec.Backend
	targetModels = append(targetModels, &networking.TargetModel{
		ModelServerName: utils.GetBackendResourceName(model.Name, backend.Name),
	})
	return targetModels
}
