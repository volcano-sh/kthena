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

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	ModelInitsReason      = "ModelCreating"
	ModelActiveReason     = "ModelAvailable"
	ModelProcessingReason = "ModelProcessing"
	ModelFailedReason     = "ModelAbnormal"
)

// setModelInitCondition sets model condition to initialized
func (mc *ModelBoosterController) setModelInitCondition(ctx context.Context, model *workloadv1alpha1.ModelBooster) error {
	meta.SetStatusCondition(&model.Status.Conditions, newCondition(string(workloadv1alpha1.ModelStatusConditionTypeInitialized),
		metav1.ConditionTrue, ModelInitsReason, "ModelBooster initialized"))
	if err := mc.updateModelBoosterStatus(ctx, model); err != nil {
		klog.Errorf("update ModelBooster status failed: %v", err)
		return err
	}
	return nil
}

// setModelProcessingCondition sets model condition to processing
func (mc *ModelBoosterController) setModelProcessingCondition(ctx context.Context, model *workloadv1alpha1.ModelBooster) error {
	meta.SetStatusCondition(&model.Status.Conditions, newCondition(string(workloadv1alpha1.ModelStatusConditionTypeActive),
		metav1.ConditionFalse, ModelProcessingReason, "ModelBooster not ready yet"))
	if err := mc.updateModelBoosterStatus(ctx, model); err != nil {
		klog.Errorf("update ModelBooster status failed: %v", err)
		return err
	}
	return nil
}

// setModelFailedCondition sets model condition to failed
func (mc *ModelBoosterController) setModelFailedCondition(ctx context.Context, model *workloadv1alpha1.ModelBooster, err error) {
	meta.SetStatusCondition(&model.Status.Conditions, newCondition(string(workloadv1alpha1.ModelStatusConditionTypeFailed),
		metav1.ConditionTrue, ModelFailedReason, err.Error()))
	if err := mc.updateModelBoosterStatus(ctx, model); err != nil {
		klog.Errorf("update ModelBooster status failed: %v", err)
	}
}

// setModelActiveCondition sets ModelBooster conditions to active
func (mc *ModelBoosterController) setModelActiveCondition(ctx context.Context, model *workloadv1alpha1.ModelBooster) error {
	meta.SetStatusCondition(&model.Status.Conditions, newCondition(string(workloadv1alpha1.ModelStatusConditionTypeActive),
		metav1.ConditionTrue, ModelActiveReason, "ModelBooster is ready"))
	if err := mc.updateModelBoosterStatus(ctx, model); err != nil {
		klog.Errorf("update ModelBooster status failed: %v", err)
		return err
	}
	return nil
}

// newCondition returns a condition
func newCondition(conditionType string, status metav1.ConditionStatus, reason string, message string) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}
