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
	"sort"
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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type AutoscaleController struct {
	// Client for k8s. Use it to call K8S API
	kubeClient kubernetes.Interface
	// client for custom resource
	client                      clientset.Interface
	autoscalingPoliciesLister   workloadLister.AutoscalingPolicyLister
	autoscalingPoliciesInformer cache.Controller
	modelServingLister          workloadLister.ModelServingLister
	modelServingInformer        cache.Controller
	podsLister                  listerv1.PodLister
	podsInformer                cache.Controller
	scalerMap                   map[string]*autoscaler.Autoscaler
	optimizerMap                map[string]*autoscaler.Optimizer
}

func NewAutoscaleController(kubeClient kubernetes.Interface, client clientset.Interface) *AutoscaleController {
	informerFactory := informersv1alpha1.NewSharedInformerFactory(client, 0)
	modelInferInformer := informerFactory.Workload().V1alpha1().ModelServings()
	autoscalingPoliciesInformer := informerFactory.Workload().V1alpha1().AutoscalingPolicies()

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
		kubeClient:                  kubeClient,
		client:                      client,
		autoscalingPoliciesLister:   autoscalingPoliciesInformer.Lister(),
		autoscalingPoliciesInformer: autoscalingPoliciesInformer.Informer(),
		modelServingLister:          modelInferInformer.Lister(),
		modelServingInformer:        modelInferInformer.Informer(),
		podsLister:                  podsInformer.Lister(),
		podsInformer:                podsInformer.Informer(),
		scalerMap:                   make(map[string]*autoscaler.Autoscaler),
		optimizerMap:                make(map[string]*autoscaler.Optimizer),
	}
	return ac
}

func (ac *AutoscaleController) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	// start informers
	go ac.autoscalingPoliciesInformer.RunWithContext(ctx)
	go ac.modelServingInformer.RunWithContext(ctx)
	go ac.podsInformer.RunWithContext(ctx)
	cache.WaitForCacheSync(ctx.Done(),
		ac.autoscalingPoliciesInformer.HasSynced,
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
	policies, err := ac.autoscalingPoliciesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list autoscaling policies, err: %v", err)
		return
	}

	scalerSet := sets.New[string]()
	optimizerSet := sets.New[string]()

	for _, policy := range policies {
		if policy.Spec.HomogeneousTarget != nil {
			scalerSet.Insert(formatAutoscalerMapKey(policy.Namespace, policy.Name, &policy.Spec.HomogeneousTarget.Target.TargetRef))
		} else if policy.Spec.HeterogeneousTarget != nil {
			optimizerSet.Insert(formatAutoscalerMapKey(policy.Namespace, policy.Name, nil))
		} else if policy.Spec.DisaggregatedTarget != nil {
			klog.V(2).Infof("disaggregated target scaling is not yet implemented, skip policy: %s", policy.Name)
		} else {
			klog.Warningf("no target set, policy name: %s", policy.Name)
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

	// Decode-first: process decode policies before prefill so decode pods enter the scheduler queue first
	sortPoliciesByRolePriority(policies)

	for _, policy := range policies {
		err := ac.schedule(ctx, policy)
		if err != nil {
			klog.Errorf("failed to process autoscale,err: %v", err)
			continue
		}
	}
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

	if instance.Spec.Replicas != nil && *instance.Spec.Replicas == replicas {
		return nil
	}
	patchBytes := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err = ac.client.WorkloadV1alpha1().ModelServings(namespaceScope).Patch(
		ctx, targetRef.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
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
		if instance.Spec.Replicas != nil {
			return *instance.Spec.Replicas, nil
		}
	}
	return 0, fmt.Errorf("target ref kind %s, name: %s not supported", targetRef.Kind, targetRef.Name)
}

func (ac *AutoscaleController) schedule(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	klog.V(2).Infof("start to process autoscaling policy %s", klog.KObj(autoscalePolicy))
	if autoscalePolicy.Spec.HeterogeneousTarget != nil {
		if err := ac.doOptimize(ctx, autoscalePolicy); err != nil {
			klog.Errorf("failed to do optimize, err: %v", err)
			return err
		}
	} else if autoscalePolicy.Spec.HomogeneousTarget != nil {
		if err := ac.doScale(ctx, autoscalePolicy); err != nil {
			klog.Errorf("failed to do scale, err: %v", err)
			return err
		}
	} else if autoscalePolicy.Spec.DisaggregatedTarget != nil {
		klog.V(2).Infof("disaggregated target scaling is not yet implemented, skip policy: %s", autoscalePolicy.Name)
	} else {
		klog.Warningf("policy %s has no target configuration", autoscalePolicy.Name)
	}

	return nil
}

func (ac *AutoscaleController) doOptimize(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	key := formatAutoscalerMapKey(autoscalePolicy.Namespace, autoscalePolicy.Name, nil)
	optimizer, ok := ac.optimizerMap[key]
	if !ok || optimizer.NeedUpdate(autoscalePolicy) {
		optimizer = autoscaler.NewOptimizer(autoscalePolicy)
		ac.optimizerMap[key] = optimizer
		klog.Infof("asp: %s changed, create new optimizer", autoscalePolicy.Name)
	}
	// Fetch current replicas
	replicasMap := make(map[string]int32, len(optimizer.Meta.Config.Params))
	for _, param := range optimizer.Meta.Config.Params {
		currentInstancesCount, err := ac.getTargetReplicas(&param.Target, autoscalePolicy.Namespace)
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
		if err := ac.updateTargetReplicas(ctx, &param.Target, autoscalePolicy.Namespace, instancesCount); err != nil {
			klog.Errorf("failed to update target kind:%s name: %s replicas:%d, err: %v", param.Target.TargetRef.Kind, param.Target.TargetRef.Name, instancesCount, err)
			return err
		}
	}

	return nil
}

func (ac *AutoscaleController) doScale(ctx context.Context, autoscalePolicy *workload.AutoscalingPolicy) error {
	target := autoscalePolicy.Spec.HomogeneousTarget.Target
	key := formatAutoscalerMapKey(autoscalePolicy.Namespace, autoscalePolicy.Name, &target.TargetRef)
	scaler, ok := ac.scalerMap[key]
	if !ok || scaler.NeedUpdate(autoscalePolicy) {
		scaler = autoscaler.NewAutoscaler(autoscalePolicy)
		ac.scalerMap[key] = scaler
		klog.Infof("asp: %s changed, create new scaler", autoscalePolicy.Name)
	}
	// Fetch current replicas
	currentInstancesCount, err := ac.getTargetReplicas(&target, autoscalePolicy.Namespace)
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
	if err := ac.updateTargetReplicas(ctx, &target, autoscalePolicy.Namespace, recommendedInstances); err != nil {
		klog.Errorf("failed to update target replicas %s, err: %v", target.TargetRef.Name, err)
		return err
	}
	klog.InfoS("successfully update target replicas", "targetRef", target.TargetRef, "recommendedInstances", recommendedInstances)
	return nil
}

func formatAutoscalerMapKey(policyNamespace, policyName string, targetRef *corev1.ObjectReference) string {
	key := policyNamespace + "/" + policyName
	if targetRef != nil {
		targetKind := targetRef.Kind
		if targetKind == "" {
			targetKind = workload.ModelServingKind.Kind
		}
		targetNamespace := targetRef.Namespace
		if targetNamespace == "" {
			targetNamespace = policyNamespace
		}
		key += "/" + targetNamespace + "/" + targetKind + "/" + targetRef.Name
	}
	return key
}

// sortPoliciesByRolePriority sorts policies so that decode is processed
// before prefill. Policies without a recognized role sort last.
func sortPoliciesByRolePriority(policies []*workload.AutoscalingPolicy) {
	sort.Slice(policies, func(i, j int) bool {
		ri := roleOfPolicy(policies[i])
		rj := roleOfPolicy(policies[j])
		oi := roleOrder(ri)
		oj := roleOrder(rj)
		if oi != oj {
			return oi < oj
		}
		if ri != rj {
			// Within the same order bucket, empty role sorts last
			if ri == "" {
				return false
			}
			if rj == "" {
				return true
			}
			return ri < rj
		}
		if policies[i].Namespace != policies[j].Namespace {
			return policies[i].Namespace < policies[j].Namespace
		}
		return policies[i].Name < policies[j].Name
	})
}

// roleOfPolicy extracts the role name from a policy's metric source label
// selector. It scans all PodMetricSources and deterministically picks the
// role with the highest priority (lowest roleOrder); lexicographic order
// breaks ties. This avoids non-deterministic map iteration causing
// inconsistent sort comparisons.
func roleOfPolicy(policy *workload.AutoscalingPolicy) string {
	if policy.Spec.HomogeneousTarget == nil {
		return ""
	}
	bestRole := ""
	bestOrder := 3 // higher than any valid roleOrder (max=2)
	for _, ms := range policy.Spec.HomogeneousTarget.Target.MetricSources {
		if ms.Pod != nil && ms.Pod.LabelSelector != nil {
			if role, ok := ms.Pod.LabelSelector.MatchLabels[workload.RoleLabelKey]; ok {
				o := roleOrder(role)
				if o < bestOrder || (o == bestOrder && role < bestRole) {
					bestOrder = o
					bestRole = role
				}
			}
		}
	}
	return bestRole
}

// roleOrder returns a sort key for a role name. "decode" has the highest
// priority (0), "prefill" second (1), everything else last (2).
func roleOrder(role string) int {
	switch role {
	case workload.RoleNameDecode:
		return 0
	case workload.RoleNamePrefill:
		return 1
	default:
		return 2
	}
}
