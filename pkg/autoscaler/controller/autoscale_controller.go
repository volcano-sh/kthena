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
	"fmt"
	"time"

	"github.com/volcano-sh/kthena/pkg/autoscaler/autoscaler"
	corev1 "k8s.io/api/core/v1"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	workloadLister "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	"istio.io/istio/pkg/util/sets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type AutoscaleController struct {
	// Client for k8s. Use it to call K8S API
	kubeClient kubernetes.Interface
	// client for custom resource
	client                             clientset.Interface
	namespace                          string
	autoscalingPoliciesLister          workloadLister.AutoscalingPolicyLister
	autoscalingPoliciesInformer        cache.Controller
	autoscalingPoliciesBindingLister   workloadLister.AutoscalingPolicyBindingLister
	autoscalingPoliciesBindingInformer cache.Controller
	modelServingLister                 workloadLister.ModelServingLister
	modelServingInformer               cache.Controller
	podsLister                         listerv1.PodLister
	podsInformer                       cache.Controller
	scalerMap                          map[string]*autoscaler.Autoscaler
	optimizerMap                       map[string]*autoscaler.Optimizer
}

func NewAutoscaleController(kubeClient kubernetes.Interface, client clientset.Interface, namespace string) *AutoscaleController {
	informerFactory := informersv1alpha1.NewSharedInformerFactory(client, 0)
	modelInferInformer := informerFactory.Workload().V1alpha1().ModelServings()
	autoscalingPoliciesInformer := informerFactory.Workload().V1alpha1().AutoscalingPolicies()
	autoscalingPoliciesBindingInformer := informerFactory.Workload().V1alpha1().AutoscalingPolicyBindings()

	selector, err := labels.NewRequirement(workload.GroupNameLabelKey, selection.Exists, nil)
	if err != nil {
		klog.Errorf("can not create label selector,err:%v", err)
		return nil
	}
	kubeInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		kubeClient, 0, informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = selector.String()
		}),
	)
	podsInformer := kubeInformerFactory.Core().V1().Pods()
	ac := &AutoscaleController{
		kubeClient:                         kubeClient,
		client:                             client,
		namespace:                          namespace,
		autoscalingPoliciesLister:          autoscalingPoliciesInformer.Lister(),
		autoscalingPoliciesInformer:        autoscalingPoliciesInformer.Informer(),
		autoscalingPoliciesBindingLister:   autoscalingPoliciesBindingInformer.Lister(),
		autoscalingPoliciesBindingInformer: autoscalingPoliciesBindingInformer.Informer(),
		modelServingLister:                 modelInferInformer.Lister(),
		modelServingInformer:               modelInferInformer.Informer(),
		podsLister:                         podsInformer.Lister(),
		podsInformer:                       podsInformer.Informer(),
		scalerMap:                          make(map[string]*autoscaler.Autoscaler),
		optimizerMap:                       make(map[string]*autoscaler.Optimizer),
	}
	return ac
}

func (ac *AutoscaleController) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	// start informers
	go ac.autoscalingPoliciesInformer.RunWithContext(ctx)
	go ac.autoscalingPoliciesBindingInformer.RunWithContext(ctx)
	go ac.modelServingInformer.RunWithContext(ctx)
	go ac.podsInformer.RunWithContext(ctx)
	cache.WaitForCacheSync(ctx.Done(),
		ac.autoscalingPoliciesInformer.HasSynced,
		ac.autoscalingPoliciesBindingInformer.HasSynced,
		ac.modelServingInformer.HasSynced,
		ac.podsInformer.HasSynced,
	)

	klog.Info("start autoscale controller")
	go wait.Until(func() {
		ac.Reconcile(ctx)
	}, util.AutoscalingSyncPeriodSeconds*time.Second, nil)

	<-ctx.Done()
	klog.Info("shut down autoscale controller")
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (ac *AutoscaleController) Reconcile(ctx context.Context) {
	klog.V(4).Info("start to reconcile")
	ctx, cancel := context.WithTimeout(ctx, util.AutoscaleCtxTimeoutSeconds*time.Second)
	defer cancel()
	bindingList, err := ac.client.WorkloadV1alpha1().AutoscalingPolicyBindings(ac.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to list autoscaling policy bindings, err: %v", err)
		return
	}

	scalerSet := sets.New[string]()
	optimizerSet := sets.New[string]()

	for _, binding := range bindingList.Items {
		policyName := binding.Spec.PolicyRef.Name
		if policyName == "" {
			klog.Warningf("invalid autoscaling policy name, binding name: %s", binding.Name)
			continue
		}
		if binding.Spec.HomogeneousTarget != nil {
			scalerSet.Insert(formatAutoscalerMapKey(binding.Name, &binding.Spec.HomogeneousTarget.Target.TargetRef))
		} else if binding.Spec.HeterogeneousTarget != nil {
			optimizerSet.Insert(formatAutoscalerMapKey(binding.Name, nil))
		} else {
			klog.Warningf("Either homogeneous or heterogeneous not set, binding name: %s", binding.Name)
		}
	}

	for key := range ac.scalerMap {
		if !scalerSet.Contains(key) {
			delete(ac.scalerMap, key)
		}
	}

	for key := range ac.optimizerMap {
		if !optimizerSet.Contains(key) {
			delete(ac.optimizerMap, key)
		}
	}

	for _, binding := range bindingList.Items {
		err := ac.schedule(ctx, &binding)
		if err != nil {
			klog.Errorf("failed to process autoscale,err: %v", err)
			continue
		}
	}
}

func (ac *AutoscaleController) updateTargetReplicas(ctx context.Context, targetRef *corev1.ObjectReference, replicas int32) error {
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = ac.namespace
	}
	switch targetRef.Kind {
	case workload.ModelServingKind.Kind:
		instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name)
		if err != nil {
			return err
		}
		// need not update replicas
		if instance.Spec.Replicas != nil && *instance.Spec.Replicas == replicas {
			return nil
		}
		instance_copy := instance.DeepCopy()
		instance_copy.Spec.Replicas = &replicas
		if _, err = ac.client.WorkloadV1alpha1().ModelServings(namespaceScope).Update(ctx, instance_copy, metav1.UpdateOptions{}); err != nil {
			return err
		}
	case workload.ModelServingKind.Kind + util.ModelServingRoleKindSuffix:
		servingName, roleName, err := util.GetRoleName(targetRef)
		if err != nil {
			return err
		}
		instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(servingName)
		if err != nil {
			return err
		}
		instance_copy := instance.DeepCopy()
		for idx := range instance_copy.Spec.Template.Roles {
			role := &instance_copy.Spec.Template.Roles[idx]
			if role.Name == roleName {
				if role.Replicas != nil && *role.Replicas == replicas {
					return nil
				}
				role.Replicas = &replicas
				break
			}
		}
		if _, err = ac.client.WorkloadV1alpha1().ModelServings(namespaceScope).Update(ctx, instance_copy, metav1.UpdateOptions{}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("target ref kind %s not supported", targetRef.Kind)
	}
	return nil
}

func (ac *AutoscaleController) getTargetReplicas(targetRef *corev1.ObjectReference) (int32, error) {
	if targetRef == nil {
		return 0, fmt.Errorf("target ref is nil")
	}
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = ac.namespace
	}
	switch targetRef.Kind {
	case workload.ModelServingKind.Kind:
		if instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name); err != nil {
			return 0, err
		} else if instance.Spec.Replicas != nil {
			return *instance.Spec.Replicas, nil
		}
	case workload.ModelServingKind.Kind + util.ModelServingRoleKindSuffix:
		servingName, roleName, err := util.GetRoleName(targetRef)
		if err != nil {
			return 0, err
		}
		instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(servingName)
		if err != nil {
			return 0, err
		}
		for _, role := range instance.Spec.Template.Roles {
			if role.Name == roleName && role.Replicas != nil {
				return *role.Replicas, nil
			}
		}
		return 0, fmt.Errorf("role %s not found in model serving %s", roleName, servingName)
	}
	return 0, fmt.Errorf("target ref kind %s not supported", targetRef.Kind)
}

func (ac *AutoscaleController) schedule(ctx context.Context, binding *workload.AutoscalingPolicyBinding) error {
	klog.V(2).Infof("start to process asp binding %s", klog.KObj(binding))
	autoscalePolicy, err := ac.getAutoscalePolicy(binding.Spec.PolicyRef.Name, binding.Namespace)
	if err != nil {
		klog.Errorf("get autoscale policy error: %v", err)
		return err
	}
	if binding.Spec.HeterogeneousTarget != nil {
		if err := ac.doOptimize(ctx, binding, autoscalePolicy); err != nil {
			klog.Errorf("failed to do optimize, err: %v", err)
			return err
		}
	} else if binding.Spec.HomogeneousTarget != nil {
		if err := ac.doScale(ctx, binding, autoscalePolicy); err != nil {
			klog.Errorf("failed to do scale, err: %v", err)
			return err
		}
	} else {
		klog.Warningf("binding %s has no scalingConfiguration and optimizerConfiguration", binding.Name)
	}

	return nil
}

func (ac *AutoscaleController) doOptimize(ctx context.Context, binding *workload.AutoscalingPolicyBinding, autoscalePolicy *workload.AutoscalingPolicy) error {
	key := formatAutoscalerMapKey(binding.Name, nil)
	optimizer, ok := ac.optimizerMap[key]
	if !ok || optimizer.NeedUpdate(autoscalePolicy, binding) {
		optimizer = autoscaler.NewOptimizer(autoscalePolicy, binding)
		ac.optimizerMap[key] = optimizer
		klog.Infof("asp: %s or binding: %s changed, create new optimizer", autoscalePolicy.Name, binding.Name)
	}
	// Fetch current replicas
	replicasMap := make(map[string]int32, len(optimizer.Meta.Config.Params))
	for _, param := range optimizer.Meta.Config.Params {
		currentInstancesCount, err := ac.getTargetReplicas(&param.Target.TargetRef)
		if err != nil {
			klog.Errorf("failed to get current replicas, err: %v", err)
			return err
		}
		replicasMap[param.Target.TargetRef.Name] = currentInstancesCount
	}

	// Get recommended replicas
	recommendedInstances, err := optimizer.Optimize(ctx, ac.podsLister, autoscalePolicy, replicasMap)
	if err != nil {
		klog.Errorf("failed to do optimize, err: %v", err)
		return err
	}
	// Do update replicas
	for _, param := range optimizer.Meta.Config.Params {
		instancesCount, exists := recommendedInstances[param.Target.TargetRef.Name]
		if !exists {
			klog.Warningf("recommended instances not exists, target ref name: %s", param.Target.TargetRef.Name)
			continue
		}
		if err := ac.updateTargetReplicas(ctx, &param.Target.TargetRef, instancesCount); err != nil {
			klog.Errorf("failed to update target kind:%s name: %s replicas:%d, err: %v", param.Target.TargetRef.Kind, param.Target.TargetRef.Name, instancesCount, err)
			return err
		}
	}

	return nil
}

func (ac *AutoscaleController) doScale(ctx context.Context, binding *workload.AutoscalingPolicyBinding, autoscalePolicy *workload.AutoscalingPolicy) error {
	target := binding.Spec.HomogeneousTarget.Target
	key := formatAutoscalerMapKey(binding.Name, &target.TargetRef)
	scaler, ok := ac.scalerMap[key]
	if !ok || scaler.NeedUpdate(autoscalePolicy, binding) {
		scaler = autoscaler.NewAutoscaler(autoscalePolicy, binding)
		ac.scalerMap[key] = scaler
		klog.Infof("asp: %s or binding: %s changed, create new scaler", autoscalePolicy.Name, binding.Name)
	}
	// Fetch current replicas
	currentInstancesCount, err := ac.getTargetReplicas(&target.TargetRef)
	if err != nil {
		klog.Errorf("failed to get current replicas, err: %v", err)
		return err
	}
	// Get recommended replicas
	klog.InfoS("do homogeneous scaling for target", "targetRef", target.TargetRef, "currentInstancesCount", currentInstancesCount)
	recommendedInstances, err := scaler.Scale(ctx, ac.podsLister, autoscalePolicy, currentInstancesCount)
	if err != nil {
		klog.Errorf("failed to do homogeneous scaling for target %s, err: %v", target.TargetRef.Name, err)
		return err
	}
	if recommendedInstances < 0 {
		return nil
	}
	// Do update replicas
	if err := ac.updateTargetReplicas(ctx, &target.TargetRef, recommendedInstances); err != nil {
		klog.Errorf("failed to update target replicas %s, err: %v", target.TargetRef.Name, err)
		return err
	}
	klog.InfoS("successfully update target replicas", "targetRef", target.TargetRef, "recommendedInstances", recommendedInstances)
	return nil
}

func (ac *AutoscaleController) getAutoscalePolicy(autoscalingPolicyName string, namespace string) (*workload.AutoscalingPolicy, error) {
	autoscalingPolicy, err := ac.autoscalingPoliciesLister.AutoscalingPolicies(namespace).Get(autoscalingPolicyName)
	if err != nil {
		klog.Errorf("can not get autoscaling policyname: %s, error: %v", autoscalingPolicyName, err)
		return nil, client.IgnoreNotFound(err)
	}
	return autoscalingPolicy, nil
}

func formatAutoscalerMapKey(bindingName string, targetRef *corev1.ObjectReference) string {
	if targetRef == nil {
		return bindingName
	}
	if targetRef.Kind == "" {
		targetRef.Kind = workload.ModelServingKind.Kind
	}
	return bindingName + "#" + targetRef.Kind + "#" + targetRef.Name
}
