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
	"reflect"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/providers"
)

type ExternalModelProviderController struct {
	kthenaClient                 clientset.Interface
	externalModelProviderLister  listerv1alpha1.ExternalModelProviderLister
	externalModelProviderIndexer cache.Indexer
	secretLister                 corelisters.SecretLister
	externalModelProviderSynced  cache.InformerSynced
	secretSynced                 cache.InformerSynced
	registration                 cache.ResourceEventHandlerRegistration
	secretRegistration           cache.ResourceEventHandlerRegistration

	workqueue   workqueue.TypedRateLimitingInterface[QueueItem]
	initialSync *atomic.Bool
	store       datastore.Store
}

const externalModelProviderSecretRefIndex = "externalModelProviderSecretRef"

func NewExternalModelProviderController(
	kthenaClient clientset.Interface,
	kthenaInformerFactory informersv1alpha1.SharedInformerFactory,
	secretInformerFactory informers.SharedInformerFactory,
	store datastore.Store,
) (*ExternalModelProviderController, error) {
	externalModelProviderInformer := kthenaInformerFactory.Networking().V1alpha1().ExternalModelProviders()
	secretInformer := secretInformerFactory.Core().V1().Secrets()
	if err := externalModelProviderInformer.Informer().AddIndexers(cache.Indexers{
		externalModelProviderSecretRefIndex: externalModelProviderSecretRefIndexFunc,
	}); err != nil {
		return nil, fmt.Errorf("failed to add Secret reference index for externalmodelprovider controller: %w", err)
	}

	controller := &ExternalModelProviderController{
		kthenaClient:                 kthenaClient,
		externalModelProviderLister:  externalModelProviderInformer.Lister(),
		externalModelProviderIndexer: externalModelProviderInformer.Informer().GetIndexer(),
		secretLister:                 secretInformer.Lister(),
		externalModelProviderSynced:  externalModelProviderInformer.Informer().HasSynced,
		secretSynced:                 secretInformer.Informer().HasSynced,
		workqueue:                    workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[QueueItem]()),
		initialSync:                  &atomic.Bool{},
		store:                        store,
	}

	var err error
	controller.registration, err = externalModelProviderInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueExternalModelProvider,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueExternalModelProvider(new)
			controller.enqueueProviderSecret(old)
			controller.enqueueProviderSecret(new)
		},
		DeleteFunc: func(obj interface{}) {
			controller.enqueueExternalModelProvider(obj)
			controller.enqueueProviderSecret(obj)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler for externalmodelprovider controller: %w", err)
	}

	controller.secretRegistration, err = secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueSecret,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueSecret(new)
		},
		DeleteFunc: controller.enqueueSecret,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add secret event handler for externalmodelprovider controller: %w", err)
	}

	return controller, nil
}

func (c *ExternalModelProviderController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.registration.HasSynced, c.secretRegistration.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}
	// add initialSync signal
	c.workqueue.Add(QueueItem{})

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	return nil
}

func (c *ExternalModelProviderController) HasSynced() bool {
	return c.initialSync.Load()
}

func (c *ExternalModelProviderController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ExternalModelProviderController) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(obj)

	if obj.ResourceType == "" && obj.Key == "" {
		klog.V(2).Info("initial external model providers have been synced")
		c.workqueue.Forget(obj)
		c.initialSync.Store(true)
		return true
	}

	var err error
	switch obj.ResourceType {
	case ResourceTypeExternalModelProvider:
		err = c.syncHandler(obj.Key)
	case ResourceTypeSecret:
		err = c.syncSecretHandler(obj.Key)
	default:
		c.workqueue.Forget(obj)
		utilruntime.HandleError(fmt.Errorf("unexpected resource type in workqueue: %s", obj.ResourceType))
		return true
	}

	if err != nil {
		if c.workqueue.NumRequeues(obj) < maxRetries {
			klog.V(2).Infof("error syncing %s %q: %v, requeuing", obj.ResourceType, obj.Key, err)
			c.workqueue.AddRateLimited(obj)
			return true
		}
		klog.V(2).Infof("giving up on syncing %s %q after %d retries: %v", obj.ResourceType, obj.Key, maxRetries, err)
	}
	c.workqueue.Forget(obj)
	return true
}

func (c *ExternalModelProviderController) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	providerName := types.NamespacedName{Namespace: namespace, Name: name}
	previousProvider := c.store.GetExternalModelProvider(providerName)
	provider, err := c.externalModelProviderLister.ExternalModelProviders(namespace).Get(name)
	if errors.IsNotFound(err) {
		if previousSecretName, ok := providerSecretName(previousProvider); ok {
			if err := c.syncSecret(previousSecretName); err != nil {
				return err
			}
		}
		return c.store.DeleteExternalModelProvider(providerName)
	}
	if err != nil {
		return err
	}

	if secretName, ok := providerSecretName(provider); ok {
		if err := c.syncSecret(secretName); err != nil {
			return err
		}
	}
	if previousSecretName, ok := providerSecretName(previousProvider); ok {
		currentSecretName, hasCurrentSecret := providerSecretName(provider)
		if !hasCurrentSecret || previousSecretName != currentSecretName {
			if err := c.syncSecret(previousSecretName); err != nil {
				return err
			}
		}
	}
	if err := c.store.AddOrUpdateExternalModelProvider(provider); err != nil {
		return err
	}

	return c.reconcileProviderStatus(provider)
}

func (c *ExternalModelProviderController) syncSecretHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	secretName := types.NamespacedName{Namespace: namespace, Name: name}
	providers, err := c.externalModelProvidersForSecret(secretName)
	if err != nil {
		return err
	}
	if err := c.syncSecretForProviders(secretName, providers); err != nil {
		return err
	}

	for _, provider := range providers {
		c.workqueue.Add(QueueItem{
			ResourceType: ResourceTypeExternalModelProvider,
			Key:          types.NamespacedName{Namespace: provider.Namespace, Name: provider.Name}.String(),
		})
	}
	return nil
}

func (c *ExternalModelProviderController) syncSecret(secretName types.NamespacedName) error {
	providers, err := c.externalModelProvidersForSecret(secretName)
	if err != nil {
		return err
	}
	return c.syncSecretForProviders(secretName, providers)
}

func (c *ExternalModelProviderController) syncSecretForProviders(secretName types.NamespacedName, providers []*networkingv1alpha1.ExternalModelProvider) error {
	if len(providers) == 0 {
		return c.store.DeleteSecret(secretName)
	}

	secret, err := c.secretLister.Secrets(secretName.Namespace).Get(secretName.Name)
	if errors.IsNotFound(err) {
		return c.store.DeleteSecret(secretName)
	}
	if err != nil {
		return err
	}

	projected := secret.DeepCopy()
	projected.Data = make(map[string][]byte, len(providers))
	for _, provider := range providers {
		if provider.Spec.Auth == nil {
			continue
		}
		key := provider.Spec.Auth.SecretRef.Key
		if value, ok := secret.Data[key]; ok {
			projected.Data[key] = append([]byte(nil), value...)
		}
	}
	projected.StringData = nil
	return c.store.AddOrUpdateSecret(projected)
}

func (c *ExternalModelProviderController) externalModelProvidersForSecret(secretName types.NamespacedName) ([]*networkingv1alpha1.ExternalModelProvider, error) {
	objects, err := c.externalModelProviderIndexer.ByIndex(externalModelProviderSecretRefIndex, secretName.String())
	if err != nil {
		return nil, fmt.Errorf("failed to look up ExternalModelProviders for Secret %s: %w", secretName, err)
	}

	providers := make([]*networkingv1alpha1.ExternalModelProvider, 0, len(objects))
	for _, object := range objects {
		provider, ok := object.(*networkingv1alpha1.ExternalModelProvider)
		if !ok {
			return nil, fmt.Errorf("unexpected object in ExternalModelProvider Secret reference index: %T", object)
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

func externalModelProviderSecretRefIndexFunc(obj interface{}) ([]string, error) {
	provider, ok := obj.(*networkingv1alpha1.ExternalModelProvider)
	if !ok {
		return nil, fmt.Errorf("expected ExternalModelProvider in Secret reference index, got %T", obj)
	}
	if provider.Spec.Auth == nil || provider.Spec.Auth.SecretRef.Name == "" {
		return nil, nil
	}
	return []string{types.NamespacedName{
		Namespace: provider.Namespace,
		Name:      provider.Spec.Auth.SecretRef.Name,
	}.String()}, nil
}

func providerSecretName(provider *networkingv1alpha1.ExternalModelProvider) (types.NamespacedName, bool) {
	if provider == nil || provider.Spec.Auth == nil || provider.Spec.Auth.SecretRef.Name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{
		Namespace: provider.Namespace,
		Name:      provider.Spec.Auth.SecretRef.Name,
	}, true
}

func (c *ExternalModelProviderController) reconcileProviderStatus(provider *networkingv1alpha1.ExternalModelProvider) error {
	if c.kthenaClient == nil {
		return nil
	}

	status := provider.Status
	status.ObservedGeneration = provider.Generation
	status.Conditions = append([]metav1.Condition(nil), provider.Status.Conditions...)
	readyCondition := metav1.Condition{
		Type:               networkingv1alpha1.ExternalModelProviderConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             networkingv1alpha1.ExternalModelProviderReasonReady,
		Message:            "External provider configuration is ready",
		ObservedGeneration: provider.Generation,
	}

	credentialCondition := metav1.Condition{
		Type:               networkingv1alpha1.ExternalModelProviderConditionCredentialsResolved,
		Status:             metav1.ConditionTrue,
		Reason:             networkingv1alpha1.ExternalModelProviderReasonCredentialNotRequired,
		Message:            "No credential is configured",
		ObservedGeneration: provider.Generation,
	}

	if provider.Spec.Auth != nil {
		credentialCondition.Reason = networkingv1alpha1.ExternalModelProviderReasonCredentialResolved
		credentialCondition.Message = "Credential Secret and key are available"

		secret, err := c.secretLister.Secrets(provider.Namespace).Get(provider.Spec.Auth.SecretRef.Name)
		if errors.IsNotFound(err) {
			credentialCondition.Status = metav1.ConditionFalse
			credentialCondition.Reason = networkingv1alpha1.ExternalModelProviderReasonCredentialNotFound
			credentialCondition.Message = fmt.Sprintf(
				"Credential Secret is not found or does not have label %s=%s",
				networkingv1alpha1.ExternalModelProviderSecretLabelKey,
				networkingv1alpha1.ExternalModelProviderSecretLabelValue,
			)
		} else if err != nil {
			return err
		} else if value := secret.Data[provider.Spec.Auth.SecretRef.Key]; len(value) == 0 {
			credentialCondition.Status = metav1.ConditionFalse
			credentialCondition.Reason = networkingv1alpha1.ExternalModelProviderReasonCredentialKeyNotFound
			credentialCondition.Message = "Credential Secret key is not found"
		} else if _, err := providers.NormalizeCredential(value); err != nil {
			credentialCondition.Status = metav1.ConditionFalse
			credentialCondition.Reason = networkingv1alpha1.ExternalModelProviderReasonCredentialInvalid
			credentialCondition.Message = err.Error()
		}
	}

	if credentialCondition.Status != metav1.ConditionTrue {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = credentialCondition.Reason
		readyCondition.Message = credentialCondition.Message
	}
	if err := providers.ValidateConfiguration(provider); err != nil {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = networkingv1alpha1.ExternalModelProviderReasonConfigurationInvalid
		readyCondition.Message = "External provider configuration is invalid"
	}

	apimeta.SetStatusCondition(&status.Conditions, credentialCondition)
	apimeta.SetStatusCondition(&status.Conditions, readyCondition)

	if reflect.DeepEqual(provider.Status, status) {
		return nil
	}

	updated := provider.DeepCopy()
	updated.Status = status
	_, err := c.kthenaClient.NetworkingV1alpha1().ExternalModelProviders(provider.Namespace).UpdateStatus(context.TODO(), updated, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *ExternalModelProviderController) enqueueExternalModelProvider(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(QueueItem{ResourceType: ResourceTypeExternalModelProvider, Key: key})
}

func (c *ExternalModelProviderController) enqueueSecret(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.workqueue.Add(QueueItem{ResourceType: ResourceTypeSecret, Key: key})
}

func (c *ExternalModelProviderController) enqueueProviderSecret(obj interface{}) {
	provider, err := externalModelProviderFromEvent(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	secretName, ok := providerSecretName(provider)
	if !ok {
		return
	}
	c.workqueue.Add(QueueItem{ResourceType: ResourceTypeSecret, Key: secretName.String()})
}

func externalModelProviderFromEvent(obj interface{}) (*networkingv1alpha1.ExternalModelProvider, error) {
	if provider, ok := obj.(*networkingv1alpha1.ExternalModelProvider); ok {
		return provider, nil
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, fmt.Errorf("expected ExternalModelProvider event, got %T", obj)
	}
	provider, ok := tombstone.Obj.(*networkingv1alpha1.ExternalModelProvider)
	if !ok {
		return nil, fmt.Errorf("expected ExternalModelProvider in tombstone, got %T", tombstone.Obj)
	}
	return provider, nil
}
