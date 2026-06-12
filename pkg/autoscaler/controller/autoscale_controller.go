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
	"math"
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
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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

func NewAutoscaleController(kubeClient kubernetes.Interface, client clientset.Interface) *AutoscaleController {
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
	nextInterval := ac.Reconcile(ctx)
	timer := time.NewTimer(nextInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			klog.Info("shut down autoscale controller")
			return
		case <-timer.C:
			nextInterval = ac.Reconcile(ctx)
			klog.V(2).InfoS("adaptive reconcile interval", "interval", nextInterval)
			timer.Reset(nextInterval)
		}
	}
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Returns the minimum reconcile interval across all bindings so that the most
// urgent scaling action drives the next wake-up.
func (ac *AutoscaleController) Reconcile(ctx context.Context) time.Duration {
	klog.V(4).Info("start to reconcile")
	ctx, cancel := context.WithTimeout(ctx, util.AutoscaleCtxTimeoutSeconds*time.Second)
	defer cancel()
	// Check if the parent context (controller shutdown) is already cancelled
	// before doing work, so we don't start a reconcile during shutdown.
	if ctx.Err() != nil {
		return util.DefaultSyncPeriodSeconds * time.Second
	}
	bindings, err := ac.autoscalingPoliciesBindingLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list autoscaling policy bindings, err: %v", err)
		return util.DefaultSyncPeriodSeconds * time.Second
	}

	scalerSet := sets.New[string]()
	optimizerSet := sets.New[string]()

	for _, binding := range bindings {
		policyName := binding.Spec.PolicyRef.Name
		if policyName == "" {
			klog.Warningf("invalid autoscaling policy name, binding name: %s", binding.Name)
			continue
		}
		if binding.Spec.HomogeneousTarget != nil {
			scalerSet.Insert(formatAutoscalerMapKey(binding.Namespace, binding.Name, &binding.Spec.HomogeneousTarget.Target.TargetRef))
		} else if binding.Spec.HeterogeneousTarget != nil {
			optimizerSet.Insert(formatAutoscalerMapKey(binding.Namespace, binding.Name, nil))
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

	defaultInterval := util.DefaultSyncPeriodSeconds * time.Second
	minInterval := time.Duration(math.MaxInt64)
	hasValidInterval := false
	for _, binding := range bindings {
		dir, periods, err := ac.schedule(ctx, binding)
		if err != nil {
			klog.Errorf("failed to process autoscale, err: %v", err)
			continue
		}
		interval := nextInterval(dir, periods)
		if interval < minInterval {
			minInterval = interval
		}
		hasValidInterval = true
	}
	if !hasValidInterval {
		return defaultInterval
	}
	return minInterval
}

func (ac *AutoscaleController) updateTargetReplicas(ctx context.Context, target *workload.Target, defaultNamespace string, replicas int32) error {
	targetRef := target.TargetRef
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = defaultNamespace
	}

	if target.TargetRef.Kind != "" && target.TargetRef.Kind != workload.ModelServingKind.Kind {
		return fmt.Errorf("target ref kind %s, name: %s not supported", targetRef.Kind, targetRef.Name)
	}

	instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name)
	if err != nil {
		return err
	}

	if target.SubTarget == nil {
		if instance.Spec.Replicas != nil && *instance.Spec.Replicas == replicas {
			return nil
		}
		patchBytes := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
		_, err = ac.client.WorkloadV1alpha1().ModelServings(namespaceScope).Patch(
			ctx, targetRef.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		return err
	}

	if target.SubTarget.Kind == util.ModelServingRoleKind && target.SubTarget.Name != "" {
		roleIndex := -1
		for idx, role := range instance.Spec.Template.Roles {
			if role.Name == target.SubTarget.Name {
				if role.Replicas != nil && *role.Replicas == replicas {
					return nil
				}
				roleIndex = idx
				break
			}
		}
		if roleIndex < 0 {
			return fmt.Errorf("role %s not found in ModelServing %s", target.SubTarget.Name, targetRef.Name)
		}
		patchBytes := []byte(fmt.Sprintf(
			`[{"op":"test","path":"/spec/template/roles/%d/name","value":"%s"},{"op":"add","path":"/spec/template/roles/%d/replicas","value":%d}]`,
			roleIndex, target.SubTarget.Name, roleIndex, replicas))
		_, err = ac.client.WorkloadV1alpha1().ModelServings(namespaceScope).Patch(
			ctx, targetRef.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
		return err
	}

	return fmt.Errorf("target ref kind %s, name: %s not supported", targetRef.Kind, targetRef.Name)
}

func (ac *AutoscaleController) getTargetReplicas(target *workload.Target, defaultNamespace string) (int32, error) {
	targetRef := target.TargetRef
	namespaceScope := targetRef.Namespace
	if namespaceScope == "" {
		namespaceScope = defaultNamespace
	}

	if targetRef.Kind == workload.ModelServingKind.Kind || targetRef.Kind == "" {
		instance, err := ac.modelServingLister.ModelServings(namespaceScope).Get(targetRef.Name)
		if err != nil {
			return 0, err
		}
		if target.SubTarget == nil {
			if instance.Spec.Replicas != nil {
				return *instance.Spec.Replicas, nil
			}
		} else if target.SubTarget.Kind == util.ModelServingRoleKind && target.SubTarget.Name != "" {
			for _, role := range instance.Spec.Template.Roles {
				if role.Name == target.SubTarget.Name && role.Replicas != nil {
					return *role.Replicas, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("target ref kind %s, name: %s not supported", targetRef.Kind, targetRef.Name)
}

func (ac *AutoscaleController) schedule(ctx context.Context, binding *workload.AutoscalingPolicyBinding) (int, syncPeriods, error) {
	klog.V(2).Infof("start to process asp binding %s", klog.KObj(binding))
	autoscalePolicy, err := ac.getAutoscalePolicy(binding.Spec.PolicyRef.Name, binding.Namespace)
	if err != nil {
		klog.Errorf("get autoscale policy error: %v", err)
		return 0, syncPeriods{}, err
	}
	periods := resolveSyncPolicy(autoscalePolicy)

	if binding.Spec.HeterogeneousTarget != nil {
		dir, err := ac.doOptimize(ctx, binding, autoscalePolicy)
		if err != nil {
			klog.Errorf("failed to do optimize, err: %v", err)
			return 0, syncPeriods{}, err
		}
		return dir, periods, nil
	} else if binding.Spec.HomogeneousTarget != nil {
		dir, err := ac.doScale(ctx, binding, autoscalePolicy)
		if err != nil {
			klog.Errorf("failed to do scale, err: %v", err)
			return 0, syncPeriods{}, err
		}
		return dir, periods, nil
	} else {
		klog.Warningf("binding %s has no scalingConfiguration and optimizerConfiguration", binding.Name)
	}

	return 0, periods, nil
}

func (ac *AutoscaleController) doOptimize(ctx context.Context, binding *workload.AutoscalingPolicyBinding, autoscalePolicy *workload.AutoscalingPolicy) (int, error) {
	key := formatAutoscalerMapKey(binding.Namespace, binding.Name, nil)
	optimizer, ok := ac.optimizerMap[key]
	if !ok || optimizer.NeedUpdate(autoscalePolicy, binding) {
		optimizer = autoscaler.NewOptimizer(autoscalePolicy, binding)
		ac.optimizerMap[key] = optimizer
		klog.Infof("asp: %s or binding: %s changed, create new optimizer", autoscalePolicy.Name, binding.Name)
	}
	// Fetch current replicas
	replicasMap := make(map[string]int32, len(optimizer.Meta.Config.Params))
	for _, param := range optimizer.Meta.Config.Params {
		currentInstancesCount, err := ac.getTargetReplicas(&param.Target, binding.Namespace)
		if err != nil {
			klog.Errorf("failed to get current replicas, err: %v", err)
			return 0, err
		}
		replicasMap[param.Target.TargetRef.Name] = currentInstancesCount
	}

	// Get recommended replicas
	recommendedInstances, err := optimizer.Optimize(ctx, ac.podsLister, autoscalePolicy, replicasMap)
	if err != nil {
		klog.Errorf("failed to do optimize, err: %v", err)
		return 0, err
	}
	// Compute direction — compare only targets that exist in both
	// replicasMap and recommendedInstances, matching the update loop below.
	var currentSum, recommendedSum int32
	for name, current := range replicasMap {
		if recommended, ok := recommendedInstances[name]; ok {
			currentSum += current
			recommendedSum += recommended
		}
	}
	direction := int(int64(recommendedSum) - int64(currentSum))

	// Do update replicas
	for _, param := range optimizer.Meta.Config.Params {
		instancesCount, exists := recommendedInstances[param.Target.TargetRef.Name]
		if !exists {
			klog.Warningf("recommended instances not exists, target ref name: %s", param.Target.TargetRef.Name)
			continue
		}
		if err := ac.updateTargetReplicas(ctx, &param.Target, binding.Namespace, instancesCount); err != nil {
			klog.Errorf("failed to update target kind:%s name: %s replicas:%d, err: %v", param.Target.TargetRef.Kind, param.Target.TargetRef.Name, instancesCount, err)
			return 0, err
		}
	}

	return direction, nil
}

func (ac *AutoscaleController) doScale(ctx context.Context, binding *workload.AutoscalingPolicyBinding, autoscalePolicy *workload.AutoscalingPolicy) (int, error) {
	target := binding.Spec.HomogeneousTarget.Target
	key := formatAutoscalerMapKey(binding.Namespace, binding.Name, &target.TargetRef)
	scaler, ok := ac.scalerMap[key]
	if !ok || scaler.NeedUpdate(autoscalePolicy, binding) {
		scaler = autoscaler.NewAutoscaler(autoscalePolicy, binding)
		ac.scalerMap[key] = scaler
		klog.Infof("asp: %s or binding: %s changed, create new scaler", autoscalePolicy.Name, binding.Name)
	}
	// Fetch current replicas
	currentInstancesCount, err := ac.getTargetReplicas(&target, binding.Namespace)
	if err != nil {
		klog.Errorf("failed to get current replicas, err: %v", err)
		return 0, err
	}
	// Get recommended replicas
	klog.InfoS("do homogeneous scaling for target", "targetRef", target.TargetRef, "currentInstancesCount", currentInstancesCount)
	recommendedInstances, err := scaler.Scale(ctx, ac.podsLister, autoscalePolicy, currentInstancesCount)
	if err != nil {
		klog.Errorf("failed to do homogeneous scaling for target %s, err: %v", target.TargetRef.Name, err)
		return 0, err
	}
	if recommendedInstances < 0 {
		return 0, nil
	}
	direction := int(int64(recommendedInstances) - int64(currentInstancesCount))
	// Do update replicas
	if err := ac.updateTargetReplicas(ctx, &target, binding.Namespace, recommendedInstances); err != nil {
		klog.Errorf("failed to update target replicas %s, err: %v", target.TargetRef.Name, err)
		return 0, err
	}
	klog.InfoS("successfully update target replicas", "targetRef", target.TargetRef, "recommendedInstances", recommendedInstances)
	return direction, nil
}

func (ac *AutoscaleController) getAutoscalePolicy(autoscalingPolicyName string, namespace string) (*workload.AutoscalingPolicy, error) {
	autoscalingPolicy, err := ac.autoscalingPoliciesLister.AutoscalingPolicies(namespace).Get(autoscalingPolicyName)
	if err != nil {
		klog.Errorf("can not get autoscaling policyname: %s, error: %v", autoscalingPolicyName, err)
		return nil, client.IgnoreNotFound(err)
	}
	return autoscalingPolicy, nil
}

func formatAutoscalerMapKey(bindingNamespace, bindingName string, targetRef *corev1.ObjectReference) string {
	key := bindingNamespace + "/" + bindingName
	if targetRef != nil {
		targetKind := targetRef.Kind
		if targetKind == "" {
			targetKind = workload.ModelServingKind.Kind
		}
		targetNamespace := targetRef.Namespace
		if targetNamespace == "" {
			targetNamespace = bindingNamespace
		}
		key += "/" + targetNamespace + "/" + targetKind + "/" + targetRef.Name
	}
	return key
}

// minReconcileInterval is the floor for any user-configured sync period.
// Values below this would cause a tight reconcile loop, overwhelming the
// API Server and burning CPU.
const minReconcileInterval = 1 * time.Second

// syncPeriods holds the per-binding reconcile intervals resolved from a single Policy.
type syncPeriods struct {
	syncPeriod      time.Duration
	scaleUpPeriod   time.Duration
	scaleDownPeriod time.Duration
}

// resolveSyncPolicy derives reconcile intervals from the Policy's syncPolicy.
// Fields left unset in the CR use compiled-in defaults. Durations below
// minReconcileInterval are clamped to minReconcileInterval and a warning is logged.
func resolveSyncPolicy(policy *workload.AutoscalingPolicy) syncPeriods {
	sp := policy.Spec.Behavior.SyncPolicy
	if sp == nil {
		return syncPeriods{
			syncPeriod:      util.DefaultSyncPeriodSeconds * time.Second,
			scaleUpPeriod:   util.ScaleUpSyncPeriodSeconds * time.Second,
			scaleDownPeriod: util.ScaleDownSyncPeriodSeconds * time.Second,
		}
	}
	return syncPeriods{
		syncPeriod:      applyOrDefault(sp.DefaultPeriod, util.DefaultSyncPeriodSeconds),
		scaleUpPeriod:   applyOrDefault(sp.ScaleUpPeriod, util.ScaleUpSyncPeriodSeconds),
		scaleDownPeriod: applyOrDefault(sp.ScaleDownPeriod, util.ScaleDownSyncPeriodSeconds),
	}
}

// applyOrDefault returns the duration from d if set and >= minReconcileInterval,
// clamps to minReconcileInterval if set but below the floor, or falls back to
// defaultSeconds when nil.
func applyOrDefault(d *metav1.Duration, defaultSeconds int) time.Duration {
	def := time.Duration(defaultSeconds) * time.Second
	if d == nil {
		return def
	}
	if d.Duration < minReconcileInterval {
		klog.V(2).Infof("syncPolicy duration %v is below minimum %v, clamping to %v", d.Duration, minReconcileInterval, minReconcileInterval)
		return minReconcileInterval
	}
	return d.Duration
}

// nextInterval returns the reconcile interval for a given scaling direction
// and set of per-binding periods.
func nextInterval(direction int, p syncPeriods) time.Duration {
	switch {
	case direction > 0:
		return p.scaleUpPeriod
	case direction < 0:
		return p.scaleDownPeriod
	default:
		return p.syncPeriod
	}
}
