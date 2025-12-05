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
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("Create ModelServing %s", modelServing.Name)
			if _, err := mc.client.WorkloadV1alpha1().ModelServings(model.Namespace).Create(ctx, modelServing, metav1.CreateOptions{}); err != nil {
				klog.Errorf("failed to create ModelServing %s: %v", klog.KObj(modelServing), err)
				return err
			}
		} else {
			klog.Errorf("failed to get ModelServing %s: %v", klog.KObj(modelServing), err)
			return err
		}
	} else {
		if oldModelServing.Labels[utils.RevisionLabelKey] != modelServing.Labels[utils.RevisionLabelKey] {
			modelServing.ResourceVersion = oldModelServing.ResourceVersion
			if _, err := mc.client.WorkloadV1alpha1().ModelServings(model.Namespace).Update(ctx, modelServing, metav1.UpdateOptions{}); err != nil {
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
		if existingModelServing.Name != modelServing.Name {
			if err := mc.client.WorkloadV1alpha1().ModelServings(model.Namespace).Delete(ctx, existingModelServing.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			klog.V(4).Infof("Delete ModelServing %s", existingModelServing.Name)
		}
	}
	return nil
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
