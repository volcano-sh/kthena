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
	corev1 "k8s.io/api/core/v1"
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

// surfaceModelServingBlockingFailure emits a Warning Event on the ModelBooster
// directing users to the child ModelServing for pod-level failure details.
// Pod inspection is intentionally kept in ModelServingController only; this
// function avoids a duplicate pod lookup and follows the Kubernetes convention
// that owners do not re-inspect their children's pods.
//
// The ModelServing is routinely not-yet-Available during ordinary startup, so that
// alone is not a failure signal. Instead, this only fires once ModelServingController
// has itself recorded a blocking-failure Warning Event on the child ModelServing;
// until then, an unavailable child is treated as normal progress and no event is emitted.
func (mc *ModelBoosterController) surfaceModelServingBlockingFailure(ctx context.Context, model *workloadv1alpha1.ModelBooster) {
	if mc.recorder == nil {
		return
	}
	modelServings, err := mc.listModelServingsByLabel(model)
	if err != nil || len(modelServings) != 1 {
		return
	}
	ms := modelServings[0]
	if !mc.hasBlockingFailureEvent(ctx, ms) {
		return
	}
	mc.recorder.Eventf(model, corev1.EventTypeWarning, "ModelServingNotReady",
		"child ModelServing %q is not yet available; check events on ModelServing for pod-level failure details",
		ms.Name)
}

// hasBlockingFailureEvent reports whether the child ModelServing already carries a
// Warning Event, i.e. whether ModelServingController has detected an actual blocking
// pod failure (as opposed to ordinary, still-in-progress startup).
func (mc *ModelBoosterController) hasBlockingFailureEvent(ctx context.Context, ms *workloadv1alpha1.ModelServing) bool {
	events, err := mc.kubeClient.CoreV1().Events(ms.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("failed to list events for ModelServing %s/%s: %v", ms.Namespace, ms.Name, err)
		return false
	}
	for _, event := range events.Items {
		if event.Type == corev1.EventTypeWarning &&
			event.InvolvedObject.Kind == workloadv1alpha1.ModelServingKind.Kind &&
			event.InvolvedObject.Namespace == ms.Namespace &&
			event.InvolvedObject.Name == ms.Name {
			return true
		}
	}
	return false
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
