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

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/convert"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	msplugins "github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

// createOrUpdateModelServing attempts to create model serving if model serving does not exist, or update it if it is different from model.
func (mc *ModelBoosterController) createOrUpdateModelServing(ctx context.Context, model *workload.ModelBooster) error {
	existingModelServings, err := mc.listModelServingsByLabel(model)
	if err != nil {
		return err
	}

	modelServing, err := convert.BuildModelServing(model)
	if err != nil {
		klog.Errorf("failed to build model serving for model %s: %v", model.Name, err)
		return err
	}

	oldModelServing, err := mc.modelServingLister.ModelServings(modelServing.Namespace).Get(modelServing.Name)
	if err != nil {
		// check if not found error directly return
		if !apierrors.IsNotFound(err) {
			klog.Errorf("failed to get ModelServing %s: %v", klog.KObj(modelServing), err)
			return err
		}
		// resource not found need create
		klog.V(4).Infof("Create ModelServing %s", modelServing.Name)
		if _, err := mc.client.WorkloadV1alpha1().ModelServings(model.Namespace).Create(ctx, modelServing, metav1.CreateOptions{}); err != nil {
			klog.Errorf("failed to create ModelServing %s: %v", klog.KObj(modelServing), err)
			return err
		}
	} else {
		preserveLegacyTemplateRevision(oldModelServing, modelServing)
		if err := msplugins.PreserveChildPluginConfiguration(oldModelServing, modelServing); err != nil {
			return err
		}
		if oldModelServing.Labels[utils.RevisionLabelKey] != modelServing.Labels[utils.RevisionLabelKey] {
			updated := oldModelServing.DeepCopy()
			updated.Spec = modelServing.Spec
			if updated.Labels == nil {
				updated.Labels = make(map[string]string)
			}
			for key, value := range modelServing.Labels {
				updated.Labels[key] = value
			}
			if _, err := mc.client.WorkloadV1alpha1().ModelServings(model.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("failed to update ModelServing %s: %v", klog.KObj(modelServing), err)
				return err
			}
			klog.V(4).Infof("Updated ModelServing %s for model %s", modelServing.Name, model.Name)
		} else {
			klog.Infof("ModelServing %s of model %s does not need to update", modelServing.Name, model.Name)
		}
	}

	// Delete old ModelServings that are not the current one
	for _, existingModelServing := range existingModelServings {
		if existingModelServing.Name == modelServing.Name {
			continue
		}
		if err := mc.client.WorkloadV1alpha1().ModelServings(model.Namespace).Delete(ctx, existingModelServing.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		klog.V(4).Infof("Delete ModelServing %s", existingModelServing.Name)
	}
	return nil
}

// A backend count change also changes the converter's revision label. Retain
// that generated Pod-template label for legacy children so safe count reductions
// cannot trigger a template rollout. All actual user template changes remain
// subject to the legacy-child migration check.
func preserveLegacyTemplateRevision(old, desired *workload.ModelServing) {
	for _, plugin := range old.Spec.Plugins {
		if plugin.Name == msplugins.HeadlessServicePluginName {
			return
		}
	}
	preserve := func(previous, next *workload.PodTemplateSpec) {
		if previous == nil || next == nil || next.Metadata == nil {
			return
		}
		delete(next.Metadata.Labels, utils.RevisionLabelKey)
		if previous.Metadata != nil {
			if value, ok := previous.Metadata.Labels[utils.RevisionLabelKey]; ok {
				if next.Metadata.Labels == nil {
					next.Metadata.Labels = make(map[string]string)
				}
				next.Metadata.Labels[utils.RevisionLabelKey] = value
			}
		}
	}
	for i := range desired.Spec.Template.Roles {
		for _, role := range old.Spec.Template.Roles {
			if role.Name == desired.Spec.Template.Roles[i].Name {
				// Older converters persisted unused worker templates. Preserve that
				// historical representation instead of rolling a legacy child just
				// because the current converter omits zero-worker templates.
				if role.WorkerReplicas == 0 && desired.Spec.Template.Roles[i].WorkerReplicas == 0 && desired.Spec.Template.Roles[i].WorkerTemplate == nil {
					desired.Spec.Template.Roles[i].WorkerTemplate = role.WorkerTemplate.DeepCopy()
				}
				preserve(&role.EntryTemplate, &desired.Spec.Template.Roles[i].EntryTemplate)
				preserve(role.WorkerTemplate, desired.Spec.Template.Roles[i].WorkerTemplate)
				break
			}
		}
	}
}

// listModelServingsByLabel list all model serving which label key is "owner" and label value is model uid
func (mc *ModelBoosterController) listModelServingsByLabel(model *workload.ModelBooster) ([]*workload.ModelServing, error) {
	if modelServings, err := mc.modelServingLister.ModelServings(model.Namespace).List(labels.SelectorFromSet(map[string]string{
		utils.OwnerUIDKey: string(model.UID),
	})); err != nil {
		return nil, err
	} else {
		return modelServings, nil
	}
}
