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

	msUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// surfaceModelServingBlockingFailure emits a Kubernetes Warning Event on the
// ModelBooster when the child ModelServing's pods have an actionable blocking
// failure. It queries pods directly so that the ModelServing's own conditions
// remain coarse-grained (like Deployment/StatefulSet status).
func (mc *ModelBoosterController) surfaceModelServingBlockingFailure(ctx context.Context, model *workloadv1alpha1.ModelBooster) {
	if mc.recorder == nil {
		return
	}
	modelServings, err := mc.listModelServingsByLabel(model)
	if err != nil || len(modelServings) != 1 {
		return
	}
	ms := modelServings[0]
	podSelector := labels.SelectorFromSet(labels.Set{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
	})
	pods, err := mc.podsLister.Pods(ms.Namespace).List(podSelector)
	if err != nil || len(pods) == 0 {
		return
	}
	reason, message := msUtils.ExtractPodBlockingFailure(pods)
	if reason == "" {
		return
	}
	mc.recorder.Event(model, corev1.EventTypeWarning, reason, message)
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
