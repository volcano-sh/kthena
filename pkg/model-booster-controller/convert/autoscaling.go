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
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	icUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BuildAutoscalingPolicy(autoscalingConfig *workload.AutoscalingPolicySpec, model *workload.ModelBooster) *workload.AutoscalingPolicy {
	spec := *autoscalingConfig
	backend := model.Spec.Backend
	targetName := utils.GetBackendResourceName(model.Name, backend.Name)
	spec.HomogeneousTarget = &workload.HomogeneousTarget{
		Target: workload.Target{
			TargetRef: corev1.ObjectReference{
				Name: targetName,
				Kind: workload.ModelServingKind.Kind,
			},
			MetricSources: buildDefaultPodMetricSources(autoscalingConfig),
		},
		MinReplicas: backend.MinReplicas,
		MaxReplicas: backend.MaxReplicas,
	}
	return &workload.AutoscalingPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: workload.AutoscalingPolicyKind.GroupVersion().String(),
			Kind:       workload.AutoscalingPolicyKind.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   targetName,
			Labels: utils.GetModelControllerLabels(model, backend.Name, icUtils.Revision(spec)),
			OwnerReferences: []metav1.OwnerReference{
				utils.NewModelOwnerRef(model),
			},
			Namespace: model.Namespace,
		},
		Spec: spec,
	}
}

func buildDefaultPodMetricSources(autoscalingPolicy *workload.AutoscalingPolicySpec) map[string]workload.MetricSource {
	sources := make(map[string]workload.MetricSource)
	if autoscalingPolicy == nil {
		return sources
	}
	for _, metric := range autoscalingPolicy.Metrics {
		sources[metric.Name] = workload.MetricSource{
			Type: workload.PodMetricSourceType,
			Pod: &workload.PodMetricSource{
				Name: metric.Name,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					workload.RoleLabelKey: workload.ModelServingEntryPodLeaderLabel,
				}},
			},
		}
	}
	return sources
}
