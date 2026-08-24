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

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/convert"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func (mc *ModelBoosterController) createOrUpdateModelRoute(ctx context.Context, model *workloadv1alpha1.ModelBooster) error {
	modelRoute := convert.BuildModelRoute(model)
	oldModelRoute, err := mc.modelRoutesLister.ModelRoutes(modelRoute.Namespace).Get(modelRoute.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The informer cache may lag behind a successful creation. Check the
			// live object before issuing another Create request.
			currentModelRoute, getErr := mc.client.NetworkingV1alpha1().ModelRoutes(model.Namespace).Get(ctx, modelRoute.Name, metav1.GetOptions{})
			if getErr == nil {
				if currentModelRoute.DeletionTimestamp != nil {
					return nil
				}
				if currentModelRoute.Spec.ModelName != modelRoute.Spec.ModelName {
					klog.Infof("Delete ModelBooster Route %s because spec.modelName is immutable", modelRoute.Name)
					return mc.client.NetworkingV1alpha1().ModelRoutes(model.Namespace).Delete(ctx, modelRoute.Name, metav1.DeleteOptions{})
				}
				return mc.updateModelRoute(ctx, currentModelRoute, modelRoute)
			}
			if !apierrors.IsNotFound(getErr) {
				klog.Errorf("failed to get live ModelRoute %s: %v", klog.KObj(modelRoute), getErr)
				return getErr
			}
			klog.V(4).Infof("Create ModelBooster Route %s", modelRoute.Name)
			if _, err := mc.client.NetworkingV1alpha1().ModelRoutes(model.Namespace).Create(ctx, modelRoute, metav1.CreateOptions{}); err != nil {
				klog.Errorf("failed to create ModelRoute %s: %v", klog.KObj(modelRoute), err)
				return err
			}
			return nil
		}
		klog.Errorf("failed to get ModelRoute %s: %v", klog.KObj(modelRoute), err)
		return err
	}
	if oldModelRoute.Spec.ModelName != modelRoute.Spec.ModelName {
		currentModelRoute, err := mc.client.NetworkingV1alpha1().ModelRoutes(model.Namespace).Get(ctx, modelRoute.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// The informer cache is stale after the previous delete. The delete
			// event will enqueue a reconcile that creates the desired route.
			return nil
		}
		if err != nil {
			klog.Errorf("failed to get ModelRoute %s before recreation: %v", klog.KObj(modelRoute), err)
			return err
		}
		if currentModelRoute.DeletionTimestamp != nil {
			return nil
		}
		if currentModelRoute.Spec.ModelName == modelRoute.Spec.ModelName {
			// The informer can still hold the pre-recreation route. Reconcile the
			// live route so concurrent rule changes are not lost.
			return mc.updateModelRoute(ctx, currentModelRoute, modelRoute)
		}

		klog.Infof("Delete ModelBooster Route %s because spec.modelName is immutable", modelRoute.Name)
		if err := mc.client.NetworkingV1alpha1().ModelRoutes(model.Namespace).Delete(ctx, modelRoute.Name, metav1.DeleteOptions{}); err != nil {
			klog.Errorf("failed to delete ModelRoute %s for recreation: %v", klog.KObj(oldModelRoute), err)
			return err
		}
		// The delete event enqueues the owning ModelBooster. Its next reconcile
		// observes the missing route and creates it with the desired model name.
		return nil
	}
	return mc.updateModelRoute(ctx, oldModelRoute, modelRoute)
}

func (mc *ModelBoosterController) updateModelRoute(ctx context.Context, currentModelRoute, modelRoute *networkingv1alpha1.ModelRoute) error {
	if currentModelRoute.Labels[utils.RevisionLabelKey] == modelRoute.Labels[utils.RevisionLabelKey] {
		klog.Infof("ModelBooster Route %s does not need to update", modelRoute.Name)
		return nil
	}
	modelRoute.ResourceVersion = currentModelRoute.ResourceVersion
	if _, err := mc.client.NetworkingV1alpha1().ModelRoutes(modelRoute.Namespace).Update(ctx, modelRoute, metav1.UpdateOptions{}); err != nil {
		klog.Errorf("failed to update ModelRoute %s: %v", klog.KObj(modelRoute), err)
		return err
	}
	return nil
}
