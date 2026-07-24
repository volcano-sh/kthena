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

package autoscaler

import (
	"context"
	"testing"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetDisaggregatedMetricTargets_UsesPerRoleMetrics(t *testing.T) {
	policy := &workload.AutoscalingPolicy{
		Spec: workload.AutoscalingPolicySpec{
			DisaggregatedTarget: &workload.DisaggregatedTarget{
				Roles: map[string]workload.RoleScalingParam{
					"prefill": {Metrics: []workload.AutoscalingPolicyMetric{{Name: "prefill_load", TargetValue: resource.MustParse("5")}}},
					"decode":  {Metrics: []workload.AutoscalingPolicyMetric{{Name: "decode_load", TargetValue: resource.MustParse("80")}}},
				},
			},
		},
	}

	got := GetDisaggregatedMetricTargets(policy)
	if got["prefill"]["prefill_load"] != 5 {
		t.Fatalf("prefill target = %v, want 5", got["prefill"]["prefill_load"])
	}
	if got["decode"]["decode_load"] != 80 {
		t.Fatalf("decode target = %v, want 80", got["decode"]["decode_load"])
	}
}

func TestGetDisaggregatedMetricTargets_UsesSharedMetrics(t *testing.T) {
	policy := &workload.AutoscalingPolicy{
		Spec: workload.AutoscalingPolicySpec{
			Metrics: []workload.AutoscalingPolicyMetric{{Name: "shared_load", TargetValue: resource.MustParse("10")}},
			DisaggregatedTarget: &workload.DisaggregatedTarget{
				Roles: map[string]workload.RoleScalingParam{
					"prefill": {},
					"decode":  {},
				},
			},
		},
	}

	got := GetDisaggregatedMetricTargets(policy)
	if got["prefill"]["shared_load"] != 10 || got["decode"]["shared_load"] != 10 {
		t.Fatalf("shared targets not applied to every role: %#v", got)
	}
}

func TestGetDisaggregatedMetricTargets_RoleMetricsOverrideSharedMetrics(t *testing.T) {
	policy := &workload.AutoscalingPolicy{
		Spec: workload.AutoscalingPolicySpec{
			Metrics: []workload.AutoscalingPolicyMetric{{Name: "shared_load", TargetValue: resource.MustParse("10")}},
			DisaggregatedTarget: &workload.DisaggregatedTarget{
				Roles: map[string]workload.RoleScalingParam{
					"prefill": {Metrics: []workload.AutoscalingPolicyMetric{{Name: "prefill_load", TargetValue: resource.MustParse("5")}}},
					"decode":  {},
				},
			},
		},
	}

	got := GetDisaggregatedMetricTargets(policy)
	if got["prefill"]["prefill_load"] != 5 {
		t.Fatalf("prefill role target = %v, want 5", got["prefill"]["prefill_load"])
	}
	if _, inherited := got["prefill"]["shared_load"]; inherited {
		t.Fatalf("prefill should override shared metrics, got %#v", got["prefill"])
	}
	if got["decode"]["shared_load"] != 10 {
		t.Fatalf("decode shared target = %v, want 10", got["decode"]["shared_load"])
	}
}

func TestGetDisaggregatedMetricTargets_FixedRoleDoesNotInheritSharedMetrics(t *testing.T) {
	policy := &workload.AutoscalingPolicy{
		Spec: workload.AutoscalingPolicySpec{
			Metrics: []workload.AutoscalingPolicyMetric{{Name: "shared_load", TargetValue: resource.MustParse("10")}},
			DisaggregatedTarget: &workload.DisaggregatedTarget{
				Roles: map[string]workload.RoleScalingParam{
					"prefill": {MinReplicas: 1, MaxReplicas: 8},
					"decode":  {MinReplicas: 2, MaxReplicas: 2},
				},
			},
		},
	}

	got := GetDisaggregatedMetricTargets(policy)
	if got["prefill"]["shared_load"] != 10 {
		t.Fatalf("prefill shared target = %v, want 10", got["prefill"]["shared_load"])
	}
	if len(got["decode"]) != 0 {
		t.Fatalf("fixed role should not inherit shared metrics, got %#v", got["decode"])
	}
}

func TestDisaggregatedAutoscalerScale_FixedSingleRoleWithoutMetrics(t *testing.T) {
	policy := &workload.AutoscalingPolicy{
		Spec: workload.AutoscalingPolicySpec{
			DisaggregatedTarget: &workload.DisaggregatedTarget{
				Roles: map[string]workload.RoleScalingParam{
					"decode": {MinReplicas: 2, MaxReplicas: 2},
				},
			},
		},
	}
	scaler := NewDisaggregatedAutoscaler(policy)

	result, err := scaler.Scale(context.Background(), nil, policy, map[string]int32{"decode": 5})
	if err != nil {
		t.Fatalf("Scale error: %v", err)
	}
	if len(result.Roles) != 1 {
		t.Fatalf("roles len = %d, want 1", len(result.Roles))
	}
	role := result.Roles[0]
	if role.Name != "decode" || role.DesiredReplicas != 2 || role.FinalReplicas != 2 {
		t.Fatalf("unexpected fixed role result: %#v", role)
	}
}

func TestMetricSourcesForRole_AddsRoleSelectorWithoutMutatingInput(t *testing.T) {
	sources := map[string]workload.MetricSource{
		"load": {Pod: &workload.PodMetricSource{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "router"}}}},
	}

	got := metricSourcesForRole("decode", sources)
	labels := got["load"].Pod.LabelSelector.MatchLabels
	if labels[workload.RoleLabelKey] != "decode" {
		t.Fatalf("role label = %q, want decode", labels[workload.RoleLabelKey])
	}
	if labels["app"] != "router" {
		t.Fatalf("custom label app not preserved: %#v", labels)
	}
	if _, mutated := sources["load"].Pod.LabelSelector.MatchLabels[workload.RoleLabelKey]; mutated {
		t.Fatalf("input metric source was mutated: %#v", sources["load"].Pod.LabelSelector.MatchLabels)
	}
}
