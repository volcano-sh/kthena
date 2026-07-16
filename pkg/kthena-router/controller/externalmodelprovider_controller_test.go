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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestExternalModelProviderController_Lifecycle(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	controller, err := NewExternalModelProviderController(kthenaClient, kthenaInformerFactory, kubeInformerFactory, store)
	assert.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	t.Run("ExternalModelProviderCreate", func(t *testing.T) {
		provider := &aiv1alpha1.ExternalModelProvider{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "openai-provider",
			},
			Spec: aiv1alpha1.ExternalModelProviderSpec{
				ProviderType: aiv1alpha1.OpenAI,
				BaseURL:      "https://api.openai.com",
			},
		}

		_, err := kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Create(
			context.Background(), provider, metav1.CreateOptions{})
		assert.NoError(t, err)

		if !waitForCacheSync(t, 5*time.Second, controller.externalModelProviderSynced) {
			t.Fatal("Failed to sync caches within timeout")
		}

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.externalModelProviderLister.ExternalModelProviders("default").Get("openai-provider")
			return err == nil
		})
		assert.True(t, found, "ExternalModelProvider should be in cache")

		err = controller.syncHandler("default/openai-provider")
		assert.NoError(t, err)

		storedProvider := store.GetExternalModelProvider(types.NamespacedName{Namespace: "default", Name: "openai-provider"})
		assert.NotNil(t, storedProvider)
		assert.Equal(t, "https://api.openai.com", storedProvider.Spec.BaseURL)

		updatedProvider, err := kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Get(
			context.Background(), "openai-provider", metav1.GetOptions{})
		assert.NoError(t, err)
		ready := apimeta.FindStatusCondition(updatedProvider.Status.Conditions, aiv1alpha1.ExternalModelProviderConditionReady)
		assert.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
	})

	t.Run("ExternalModelProviderUpdate", func(t *testing.T) {
		existing, err := kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Get(
			context.Background(), "openai-provider", metav1.GetOptions{})
		assert.NoError(t, err)

		updated := existing.DeepCopy()
		updated.Spec.BaseURL = "https://api.anthropic.com"
		updated.Spec.ProviderType = aiv1alpha1.Anthropic

		_, err = kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Update(
			context.Background(), updated, metav1.UpdateOptions{})
		assert.NoError(t, err)

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			provider, err := controller.externalModelProviderLister.ExternalModelProviders("default").Get("openai-provider")
			return err == nil && provider.Spec.BaseURL == "https://api.anthropic.com"
		})
		assert.True(t, found, "ExternalModelProvider update should be reflected in cache")

		err = controller.syncHandler("default/openai-provider")
		assert.NoError(t, err)

		storedProvider := store.GetExternalModelProvider(types.NamespacedName{Namespace: "default", Name: "openai-provider"})
		assert.NotNil(t, storedProvider)
		assert.Equal(t, aiv1alpha1.Anthropic, storedProvider.Spec.ProviderType)
	})

	t.Run("ExternalModelProviderDelete", func(t *testing.T) {
		err := kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Delete(
			context.Background(), "openai-provider", metav1.DeleteOptions{})
		assert.NoError(t, err)

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.externalModelProviderLister.ExternalModelProviders("default").Get("openai-provider")
			return err != nil
		})
		assert.True(t, found, "ExternalModelProvider should be removed from cache")

		err = controller.syncHandler("default/openai-provider")
		assert.NoError(t, err)

		storedProvider := store.GetExternalModelProvider(types.NamespacedName{Namespace: "default", Name: "openai-provider"})
		assert.Nil(t, storedProvider)
	})

	t.Run("SecretSync", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "provider-secret",
			},
			Data: map[string][]byte{
				"api-key": []byte("test-key"),
			},
		}

		_, err := kubeClient.CoreV1().Secrets("default").Create(
			context.Background(), secret, metav1.CreateOptions{})
		assert.NoError(t, err)

		if !waitForCacheSync(t, 5*time.Second, controller.secretSynced) {
			t.Fatal("Failed to sync secret cache within timeout")
		}

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.secretLister.Secrets("default").Get("provider-secret")
			return err == nil
		})
		assert.True(t, found, "Secret should be in cache")

		err = controller.syncSecretHandler("default/provider-secret")
		assert.NoError(t, err)

		storedSecret := store.GetSecret(types.NamespacedName{Namespace: "default", Name: "provider-secret"})
		assert.NotNil(t, storedSecret)
		assert.Equal(t, []byte("test-key"), storedSecret.Data["api-key"])

		err = kubeClient.CoreV1().Secrets("default").Delete(
			context.Background(), "provider-secret", metav1.DeleteOptions{})
		assert.NoError(t, err)

		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.secretLister.Secrets("default").Get("provider-secret")
			return err != nil
		})
		assert.True(t, found, "Secret should be removed from cache")

		err = controller.syncSecretHandler("default/provider-secret")
		assert.NoError(t, err)
		assert.Nil(t, store.GetSecret(types.NamespacedName{Namespace: "default", Name: "provider-secret"}))
	})
}

func TestExternalModelProviderController_StatusForCredentials(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	controller, err := NewExternalModelProviderController(kthenaClient, kthenaInformerFactory, kubeInformerFactory, store)
	assert.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	provider := &aiv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "openai-provider",
			Generation: 1,
		},
		Spec: aiv1alpha1.ExternalModelProviderSpec{
			ProviderType: aiv1alpha1.OpenAI,
			BaseURL:      "https://api.openai.com",
			Auth: &aiv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "provider-secret"},
					Key:                  "api-key",
				},
			},
		},
	}

	_, err = kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Create(
		context.Background(), provider, metav1.CreateOptions{})
	assert.NoError(t, err)

	if !waitForCacheSync(t, 5*time.Second, controller.externalModelProviderSynced, controller.secretSynced) {
		t.Fatal("Failed to sync caches within timeout")
	}
	found := waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.externalModelProviderLister.ExternalModelProviders("default").Get("openai-provider")
		return err == nil
	})
	assert.True(t, found, "ExternalModelProvider should be in cache")

	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "provider-secret",
		},
		Data: map[string][]byte{
			"api-key": []byte("test-key"),
		},
	}
	_, err = kubeClient.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	assert.NoError(t, err)
	found = waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.secretLister.Secrets("default").Get("provider-secret")
		return err == nil
	})
	assert.True(t, found, "Secret should be in cache")

	err = controller.syncSecretHandler("default/provider-secret")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionTrue, aiv1alpha1.ExternalModelProviderReasonReady)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionTrue, aiv1alpha1.ExternalModelProviderReasonCredentialResolved)

	secretWithoutKey := secret.DeepCopy()
	secretWithoutKey.Data = map[string][]byte{
		"other-key": []byte("test-key"),
	}
	_, err = kubeClient.CoreV1().Secrets("default").Update(context.Background(), secretWithoutKey, metav1.UpdateOptions{})
	assert.NoError(t, err)
	found = waitForObjectInCache(t, 2*time.Second, func() bool {
		got, err := controller.secretLister.Secrets("default").Get("provider-secret")
		return err == nil && got.Data["api-key"] == nil
	})
	assert.True(t, found, "Secret update should be reflected in cache")

	err = controller.syncSecretHandler("default/provider-secret")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialKeyNotFound)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialKeyNotFound)

	secretWithRotatedKey := secret.DeepCopy()
	secretWithRotatedKey.Data = map[string][]byte{
		"api-key": []byte("rotated-key"),
	}
	_, err = kubeClient.CoreV1().Secrets("default").Update(context.Background(), secretWithRotatedKey, metav1.UpdateOptions{})
	assert.NoError(t, err)
	found = waitForObjectInCache(t, 2*time.Second, func() bool {
		got, err := controller.secretLister.Secrets("default").Get("provider-secret")
		return err == nil && string(got.Data["api-key"]) == "rotated-key"
	})
	assert.True(t, found, "Secret rotation should be reflected in cache")

	err = controller.syncSecretHandler("default/provider-secret")
	assert.NoError(t, err)
	storedSecret := store.GetSecret(types.NamespacedName{Namespace: "default", Name: "provider-secret"})
	if assert.NotNil(t, storedSecret) {
		assert.Equal(t, []byte("rotated-key"), storedSecret.Data["api-key"])
	}
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionTrue, aiv1alpha1.ExternalModelProviderReasonReady)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionTrue, aiv1alpha1.ExternalModelProviderReasonCredentialResolved)

	err = kubeClient.CoreV1().Secrets("default").Delete(context.Background(), "provider-secret", metav1.DeleteOptions{})
	assert.NoError(t, err)
	found = waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.secretLister.Secrets("default").Get("provider-secret")
		return err != nil
	})
	assert.True(t, found, "Secret should be removed from cache")

	err = controller.syncSecretHandler("default/provider-secret")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
}

func assertProviderCondition(t *testing.T, client *kthenafake.Clientset, namespace, name, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	provider, err := client.NetworkingV1alpha1().ExternalModelProviders(namespace).Get(context.Background(), name, metav1.GetOptions{})
	assert.NoError(t, err)
	condition := apimeta.FindStatusCondition(provider.Status.Conditions, conditionType)
	if assert.NotNil(t, condition) {
		assert.Equal(t, status, condition.Status)
		assert.Equal(t, reason, condition.Reason)
	}
}

func TestExternalModelProviderController_ErrorHandling(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	controller, err := NewExternalModelProviderController(kthenaClient, kthenaInformerFactory, kubeInformerFactory, store)
	assert.NoError(t, err)

	t.Run("InvalidKey", func(t *testing.T) {
		err := controller.syncHandler("invalid/key/format")
		assert.NoError(t, err)
	})

	t.Run("NonExistentExternalModelProvider", func(t *testing.T) {
		err := controller.syncHandler("default/non-existent")
		assert.NoError(t, err)
	})
}

func TestExternalModelProviderController_WorkQueueProcessing(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()

	controller, err := NewExternalModelProviderController(kthenaClient, kthenaInformerFactory, kubeInformerFactory, store)
	assert.NoError(t, err)

	t.Run("InitialSyncSignal", func(t *testing.T) {
		assert.False(t, controller.HasSynced())
		controller.workqueue.Add(initialSyncSignal)
		controller.processNextWorkItem()
		assert.True(t, controller.HasSynced())
	})

	t.Run("UnknownResourceType", func(t *testing.T) {
		controller.workqueue.Add(12345)
		result := controller.processNextWorkItem()
		assert.True(t, result)
	})
}

func TestExternalModelProviderController_SuccessForgetsRetryHistory(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		datastore.New(),
	)
	assert.NoError(t, err)

	controller.workqueue.ShutDown()
	controller.workqueue = workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[any](0, 0),
	)
	defer controller.workqueue.ShutDown()

	provider := providerWithSecretRef("default", "openai-provider", "")
	provider.Spec.Auth = nil
	assert.NoError(t, controller.externalModelProviderIndexer.Add(provider))

	key := "default/openai-provider"
	controller.workqueue.AddRateLimited(key)
	assert.Equal(t, 1, controller.workqueue.NumRequeues(key))
	assert.True(t, controller.processNextWorkItem())
	assert.Zero(t, controller.workqueue.NumRequeues(key))
}

func TestExternalModelProviderController_EnqueueDeletedFinalStateUnknown(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		datastore.New(),
	)
	assert.NoError(t, err)

	tests := []struct {
		name         string
		tombstoneKey string
		object       interface{}
		enqueue      func(interface{})
		want         interface{}
	}{
		{
			name:         "ExternalModelProvider",
			tombstoneKey: "default/openai-provider",
			object: &aiv1alpha1.ExternalModelProvider{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "openai-provider",
			}},
			enqueue: controller.enqueueExternalModelProvider,
			want:    "default/openai-provider",
		},
		{
			name:         "Secret",
			tombstoneKey: "default/provider-secret",
			object: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "provider-secret",
			}},
			enqueue: controller.enqueueSecret,
			want: externalModelProviderQueueItem{
				resourceType: ResourceTypeSecret,
				key:          "default/provider-secret",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.enqueue(cache.DeletedFinalStateUnknown{
				Key: tt.tombstoneKey,
				Obj: tt.object,
			})

			if assert.Equal(t, 1, controller.workqueue.Len()) {
				item, shutdown := controller.workqueue.Get()
				assert.False(t, shutdown)
				assert.Equal(t, tt.want, item)
				controller.workqueue.Done(item)
				controller.workqueue.Forget(item)
			}
		})
	}
}

func TestExternalModelProviderController_ProvidersForSecretUsesReferenceIndex(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		datastore.New(),
	)
	assert.NoError(t, err)

	providers := []*aiv1alpha1.ExternalModelProvider{
		providerWithSecretRef("default", "provider-a", "secret-a"),
		providerWithSecretRef("default", "provider-b", "secret-b"),
		providerWithSecretRef("other", "provider-c", "secret-a"),
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "provider-without-auth"},
		},
	}
	for _, provider := range providers {
		assert.NoError(t, controller.externalModelProviderIndexer.Add(provider))
	}

	got, err := controller.externalModelProvidersForSecret(types.NamespacedName{Namespace: "default", Name: "secret-a"})
	assert.NoError(t, err)
	if assert.Len(t, got, 1) {
		assert.Equal(t, "provider-a", got[0].Name)
	}
}

func providerWithSecretRef(namespace, name, secretName string) *aiv1alpha1.ExternalModelProvider {
	return &aiv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: aiv1alpha1.ExternalModelProviderSpec{
			Auth: &aiv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "api-key",
				},
			},
		},
	}
}
