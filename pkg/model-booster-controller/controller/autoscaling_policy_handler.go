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

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/convert"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func (mc *ModelBoosterController) createOrUpdateAutoscalingPolicyAndBinding(ctx context.Context, model *v1alpha1.ModelBooster) error {
	if model.Spec.AutoscalingPolicy != nil {
		// Create autoscaling policy and optimize policy binding
		asp := convert.BuildAutoscalingPolicy(model.Spec.AutoscalingPolicy, model, "")
		aspBinding := convert.BuildOptimizePolicyBinding(model, utils.GetBackendResourceName(model.Name, ""))
		if err := mc.createOrUpdateAsp(ctx, asp); err != nil {
			return err
		}
		if err := mc.createOrUpdateAspBinding(ctx, aspBinding); err != nil {
			return err
		}
	} else {
		// Create autoscaling policy and scaling policy binding for single backend
		backend := model.Spec.Backend
		if backend.AutoscalingPolicy != nil {
			asp := convert.BuildAutoscalingPolicy(backend.AutoscalingPolicy, model, backend.Name)
			aspBinding := convert.BuildScalingPolicyBinding(model, &backend, utils.GetBackendResourceName(model.Name, backend.Name))
			if err := mc.createOrUpdateAsp(ctx, asp); err != nil {
				return err
			}
			if err := mc.createOrUpdateAspBinding(ctx, aspBinding); err != nil {
				return err
			}
		}
	}
	return nil
}

func (mc *ModelBoosterController) createOrUpdateAsp(ctx context.Context, policy *v1alpha1.AutoscalingPolicy) error {
	oldPolicy, err := mc.autoscalingPoliciesLister.AutoscalingPolicies(policy.Namespace).Get(policy.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if _, err := mc.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Create(ctx, policy, metav1.CreateOptions{}); err != nil {
				klog.Errorf("failed to create ASP %s,%v", klog.KObj(policy), err)
				return err
			}
			return nil
		}
		return err
	}
	if oldPolicy.Labels[utils.RevisionLabelKey] == policy.Labels[utils.RevisionLabelKey] {
		klog.Infof("Autoscaling policy %s does not need to update", policy.Name)
		return nil
	}
	if _, err := mc.client.WorkloadV1alpha1().AutoscalingPolicies(policy.Namespace).Update(ctx, policy, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (mc *ModelBoosterController) createOrUpdateAspBinding(ctx context.Context, binding *v1alpha1.AutoscalingPolicyBinding) error {
	oldPolicy, err := mc.autoscalingPolicyBindingsLister.AutoscalingPolicyBindings(binding.Namespace).Get(binding.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if _, err := mc.client.WorkloadV1alpha1().AutoscalingPolicyBindings(binding.Namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
				klog.Errorf("failed to create bindings %s,%v", klog.KObj(binding), err)
				return err
			}
			return nil
		}
		return err
	}
	if oldPolicy.Labels[utils.RevisionLabelKey] == binding.Labels[utils.RevisionLabelKey] {
		klog.Infof("Bindings %s does not need to update", binding.Name)
		return nil
	}
	if _, err := mc.client.WorkloadV1alpha1().AutoscalingPolicyBindings(binding.Namespace).Update(ctx, binding, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}
