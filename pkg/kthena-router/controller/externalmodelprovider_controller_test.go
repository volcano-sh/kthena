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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestExternalModelProviderSecretInformerFactoryFiltersSecrets(t *testing.T) {
	labeledSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "provider-secret",
			Labels: map[string]string{
				aiv1alpha1.ExternalModelProviderSecretLabelKey: aiv1alpha1.ExternalModelProviderSecretLabelValue,
			},
		},
		Data: map[string][]byte{"api-key": []byte("provider-key")},
	}
	unlabeledSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unrelated-secret"},
		Data:       map[string][]byte{"private-key": []byte("must-not-be-cached")},
	}
	kubeClient := kubefake.NewSimpleClientset(labeledSecret, unlabeledSecret)
	secretInformerFactory := NewExternalModelProviderSecretInformerFactory(kubeClient)
	secretInformer := secretInformerFactory.Core().V1().Secrets()
	secretInformerSynced := secretInformer.Informer().HasSynced

	stop := make(chan struct{})
	defer close(stop)
	secretInformerFactory.Start(stop)
	assert.True(t, waitForCacheSync(t, 5*time.Second, secretInformerSynced))

	got, err := secretInformer.Lister().Secrets("default").Get(labeledSecret.Name)
	assert.NoError(t, err)
	assert.Equal(t, labeledSecret.Data, got.Data)

	_, err = secretInformer.Lister().Secrets("default").Get(unlabeledSecret.Name)
	assert.True(t, apierrors.IsNotFound(err), "Unlabeled Secret must not enter the informer cache")

	var listSelector, watchSelector string
	for _, action := range kubeClient.Actions() {
		if action.GetResource().Resource != "secrets" {
			continue
		}
		switch typedAction := action.(type) {
		case k8stesting.ListAction:
			if typedAction.GetListRestrictions().Labels != nil {
				listSelector = typedAction.GetListRestrictions().Labels.String()
			}
		case k8stesting.WatchAction:
			if typedAction.GetWatchRestrictions().Labels != nil {
				watchSelector = typedAction.GetWatchRestrictions().Labels.String()
			}
		}
	}
	expectedSelector := aiv1alpha1.ExternalModelProviderSecretLabelKey + "=" + aiv1alpha1.ExternalModelProviderSecretLabelValue
	assert.Equal(t, expectedSelector, listSelector)
	assert.Equal(t, expectedSelector, watchSelector)
}

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
	if !waitForCacheSync(t, 5*time.Second, controller.externalModelProviderSynced, controller.secretSynced) {
		t.Fatal("Failed to sync caches within timeout")
	}

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

	t.Run("UnreferencedSecretIsNotStored", func(t *testing.T) {
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

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.secretLister.Secrets("default").Get("provider-secret")
			return err == nil
		})
		assert.True(t, found, "Secret should be in cache")

		err = controller.syncSecretHandler("default/provider-secret")
		assert.NoError(t, err)
		assert.Nil(t, store.GetSecret(types.NamespacedName{Namespace: "default", Name: "provider-secret"}))

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
	secretInformerFactory := NewExternalModelProviderSecretInformerFactory(kubeClient)
	store := datastore.New()

	controller, err := NewExternalModelProviderController(kthenaClient, kthenaInformerFactory, secretInformerFactory, store)
	assert.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)
	kthenaInformerFactory.Start(stop)
	secretInformerFactory.Start(stop)
	if !waitForCacheSync(t, 5*time.Second, controller.externalModelProviderSynced, controller.secretSynced) {
		t.Fatal("Failed to sync caches within timeout")
	}

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

	found := waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.externalModelProviderLister.ExternalModelProviders("default").Get("openai-provider")
		return err == nil
	})
	assert.True(t, found, "ExternalModelProvider should be in cache")

	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
	providerWithStatus, err := kthenaClient.NetworkingV1alpha1().ExternalModelProviders("default").Get(
		context.Background(), "openai-provider", metav1.GetOptions{})
	assert.NoError(t, err)
	credentialCondition := apimeta.FindStatusCondition(providerWithStatus.Status.Conditions, aiv1alpha1.ExternalModelProviderConditionCredentialsResolved)
	if assert.NotNil(t, credentialCondition) {
		assert.Contains(t, credentialCondition.Message, aiv1alpha1.ExternalModelProviderSecretLabelKey)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "provider-secret",
			Labels: map[string]string{
				aiv1alpha1.ExternalModelProviderSecretLabelKey: aiv1alpha1.ExternalModelProviderSecretLabelValue,
			},
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
	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionTrue, aiv1alpha1.ExternalModelProviderReasonReady)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionTrue, aiv1alpha1.ExternalModelProviderReasonCredentialResolved)

	secretWithWhitespaceOnlyKey := secret.DeepCopy()
	secretWithWhitespaceOnlyKey.Data = map[string][]byte{
		"api-key": []byte(" \t\r\n"),
	}
	_, err = kubeClient.CoreV1().Secrets("default").Update(context.Background(), secretWithWhitespaceOnlyKey, metav1.UpdateOptions{})
	assert.NoError(t, err)
	found = waitForObjectInCache(t, 2*time.Second, func() bool {
		got, err := controller.secretLister.Secrets("default").Get("provider-secret")
		return err == nil && string(got.Data["api-key"]) == " \t\r\n"
	})
	assert.True(t, found, "Secret update should be reflected in cache")

	err = controller.syncSecretHandler("default/provider-secret")
	assert.NoError(t, err)
	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialInvalid)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialInvalid)

	secretWithTrailingNewline := secret.DeepCopy()
	secretWithTrailingNewline.Data = map[string][]byte{
		"api-key": []byte("test-key\n"),
	}
	_, err = kubeClient.CoreV1().Secrets("default").Update(context.Background(), secretWithTrailingNewline, metav1.UpdateOptions{})
	assert.NoError(t, err)
	found = waitForObjectInCache(t, 2*time.Second, func() bool {
		got, err := controller.secretLister.Secrets("default").Get("provider-secret")
		return err == nil && string(got.Data["api-key"]) == "test-key\n"
	})
	assert.True(t, found, "Secret update should be reflected in cache")

	err = controller.syncSecretHandler("default/provider-secret")
	assert.NoError(t, err)
	err = controller.syncHandler("default/openai-provider")
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
	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialKeyNotFound)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialKeyNotFound)

	secretWithRotatedKey := secret.DeepCopy()
	secretWithRotatedKey.Data = map[string][]byte{
		"api-key":      []byte("rotated-key"),
		"unreferenced": []byte("must-not-be-cached"),
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
	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	storedSecret := store.GetSecret(types.NamespacedName{Namespace: "default", Name: "provider-secret"})
	if assert.NotNil(t, storedSecret) {
		assert.Equal(t, []byte("rotated-key"), storedSecret.Data["api-key"])
		assert.NotContains(t, storedSecret.Data, "unreferenced")
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
	err = controller.syncHandler("default/openai-provider")
	assert.NoError(t, err)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionReady, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
	assertProviderCondition(t, kthenaClient, "default", "openai-provider", aiv1alpha1.ExternalModelProviderConditionCredentialsResolved, metav1.ConditionFalse, aiv1alpha1.ExternalModelProviderReasonCredentialNotFound)
}

func TestExternalModelProviderController_StatusForInvalidConfiguration(t *testing.T) {
	provider := &aiv1alpha1.ExternalModelProvider{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "invalid-provider",
			Generation: 1,
		},
		Spec: aiv1alpha1.ExternalModelProviderSpec{
			ProviderType: aiv1alpha1.OpenAI,
			BaseURL:      "http://api.example.com",
		},
	}
	kthenaClient := kthenafake.NewSimpleClientset(provider)
	kubeClient := kubefake.NewSimpleClientset()
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0),
		informers.NewSharedInformerFactory(kubeClient, 0),
		datastore.New(),
	)
	assert.NoError(t, err)

	assert.NoError(t, controller.reconcileProviderStatus(provider))
	assertProviderCondition(
		t,
		kthenaClient,
		provider.Namespace,
		provider.Name,
		aiv1alpha1.ExternalModelProviderConditionReady,
		metav1.ConditionFalse,
		aiv1alpha1.ExternalModelProviderReasonConfigurationInvalid,
	)
	assertProviderCondition(
		t,
		kthenaClient,
		provider.Namespace,
		provider.Name,
		aiv1alpha1.ExternalModelProviderConditionCredentialsResolved,
		metav1.ConditionTrue,
		aiv1alpha1.ExternalModelProviderReasonCredentialNotRequired,
	)
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
		controller.workqueue.Add(QueueItem{})
		controller.processNextWorkItem()
		assert.True(t, controller.HasSynced())
	})

	t.Run("UnknownResourceType", func(t *testing.T) {
		controller.workqueue.Add(QueueItem{ResourceType: "Unknown", Key: "default/object"})
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
		workqueue.NewTypedItemExponentialFailureRateLimiter[QueueItem](0, 0),
	)
	defer controller.workqueue.ShutDown()

	provider := providerWithSecretRef("default", "openai-provider", "")
	provider.Spec.Auth = nil
	assert.NoError(t, controller.externalModelProviderIndexer.Add(provider))

	item := QueueItem{ResourceType: ResourceTypeExternalModelProvider, Key: "default/openai-provider"}
	controller.workqueue.AddRateLimited(item)
	assert.Equal(t, 1, controller.workqueue.NumRequeues(item))
	assert.True(t, controller.processNextWorkItem())
	assert.Zero(t, controller.workqueue.NumRequeues(item))
}

func TestExternalModelProviderController_SecretSyncEnqueuesAffectedProviders(t *testing.T) {
	secretName := types.NamespacedName{Namespace: "default", Name: "provider-secret"}
	providers := []*aiv1alpha1.ExternalModelProvider{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "first-provider", Namespace: secretName.Namespace},
			Spec: aiv1alpha1.ExternalModelProviderSpec{Auth: &aiv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName.Name},
					Key:                  "api-key",
				},
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "second-provider", Namespace: secretName.Namespace},
			Spec: aiv1alpha1.ExternalModelProviderSpec{Auth: &aiv1alpha1.ProviderAuth{
				SecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName.Name},
					Key:                  "api-key",
				},
			}},
		},
	}
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	secretInformer := kubeInformerFactory.Core().V1().Secrets()
	controller, err := NewExternalModelProviderController(kthenaClient, kthenaInformerFactory, kubeInformerFactory, datastore.New())
	assert.NoError(t, err)
	for _, provider := range providers {
		assert.NoError(t, controller.externalModelProviderIndexer.Add(provider))
	}
	assert.NoError(t, secretInformer.Informer().GetIndexer().Add(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName.Name, Namespace: secretName.Namespace},
		Data:       map[string][]byte{"api-key": []byte("key")},
	}))

	err = controller.syncSecretHandler(secretName.String())
	assert.NoError(t, err)
	var got []QueueItem
	for controller.workqueue.Len() > 0 {
		item, shutdown := controller.workqueue.Get()
		assert.False(t, shutdown)
		got = append(got, item)
		controller.workqueue.Done(item)
		controller.workqueue.Forget(item)
	}
	assert.ElementsMatch(t, []QueueItem{
		{ResourceType: ResourceTypeExternalModelProvider, Key: "default/first-provider"},
		{ResourceType: ResourceTypeExternalModelProvider, Key: "default/second-provider"},
	}, got)
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
		want         QueueItem
	}{
		{
			name:         "ExternalModelProvider",
			tombstoneKey: "default/openai-provider",
			object: &aiv1alpha1.ExternalModelProvider{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "openai-provider",
			}},
			enqueue: controller.enqueueExternalModelProvider,
			want: QueueItem{
				ResourceType: ResourceTypeExternalModelProvider,
				Key:          "default/openai-provider",
			},
		},
		{
			name:         "Secret",
			tombstoneKey: "default/provider-secret",
			object: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "provider-secret",
			}},
			enqueue: controller.enqueueSecret,
			want: QueueItem{
				ResourceType: ResourceTypeSecret,
				Key:          "default/provider-secret",
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

func TestExternalModelProviderController_EnqueueProviderSecretFromTombstone(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0),
		informers.NewSharedInformerFactory(kubeClient, 0),
		datastore.New(),
	)
	assert.NoError(t, err)

	controller.enqueueProviderSecret(cache.DeletedFinalStateUnknown{
		Key: "default/openai-provider",
		Obj: providerWithSecretRef("default", "openai-provider", "provider-secret"),
	})

	if assert.Equal(t, 1, controller.workqueue.Len()) {
		item, shutdown := controller.workqueue.Get()
		assert.False(t, shutdown)
		assert.Equal(t, QueueItem{
			ResourceType: ResourceTypeSecret,
			Key:          "default/provider-secret",
		}, item)
		controller.workqueue.Done(item)
		controller.workqueue.Forget(item)
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

func TestExternalModelProviderController_ProviderSyncManagesReferencedSecrets(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	store := datastore.New()
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)
	assert.NoError(t, err)
	controller.kthenaClient = nil

	providerName := types.NamespacedName{Namespace: "default", Name: "provider"}
	oldProvider := providerWithSecretRef(providerName.Namespace, providerName.Name, "old-secret")
	newProvider := providerWithSecretRef(providerName.Namespace, providerName.Name, "new-secret")
	assert.NoError(t, store.AddOrUpdateExternalModelProvider(oldProvider))
	assert.NoError(t, store.AddOrUpdateSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "old-secret"},
		Data:       map[string][]byte{"api-key": []byte("old")},
	}))
	assert.NoError(t, controller.externalModelProviderIndexer.Add(newProvider))

	secretInformer := kubeInformerFactory.Core().V1().Secrets()
	assert.NoError(t, secretInformer.Informer().GetIndexer().Add(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "new-secret"},
		Data: map[string][]byte{
			"api-key":      []byte("new"),
			"unreferenced": []byte("must-not-be-cached"),
		},
	}))

	assert.NoError(t, controller.syncHandler(providerName.String()))
	assert.Nil(t, store.GetSecret(types.NamespacedName{Namespace: "default", Name: "old-secret"}))
	stored := store.GetSecret(types.NamespacedName{Namespace: "default", Name: "new-secret"})
	if assert.NotNil(t, stored) {
		assert.Equal(t, map[string][]byte{"api-key": []byte("new")}, stored.Data)
	}

	assert.NoError(t, controller.externalModelProviderIndexer.Delete(newProvider))
	assert.NoError(t, controller.syncHandler(providerName.String()))
	assert.Nil(t, store.GetExternalModelProvider(providerName))
	assert.Nil(t, store.GetSecret(types.NamespacedName{Namespace: "default", Name: "new-secret"}))
}

func TestExternalModelProviderController_ProviderSyncRetainsOldReferenceForRetry(t *testing.T) {
	kthenaClient := kthenafake.NewSimpleClientset()
	kubeClient := kubefake.NewSimpleClientset()
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	baseStore := datastore.New()
	store := &failSecretDeleteStore{Store: baseStore, fail: true}
	controller, err := NewExternalModelProviderController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)
	assert.NoError(t, err)
	controller.kthenaClient = nil

	providerName := types.NamespacedName{Namespace: "default", Name: "provider"}
	oldProvider := providerWithSecretRef(providerName.Namespace, providerName.Name, "old-secret")
	newProvider := providerWithSecretRef(providerName.Namespace, providerName.Name, "new-secret")
	assert.NoError(t, baseStore.AddOrUpdateExternalModelProvider(oldProvider))
	assert.NoError(t, baseStore.AddOrUpdateSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "old-secret"},
		Data:       map[string][]byte{"api-key": []byte("old")},
	}))
	assert.NoError(t, controller.externalModelProviderIndexer.Add(newProvider))
	assert.NoError(t, kubeInformerFactory.Core().V1().Secrets().Informer().GetIndexer().Add(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "new-secret"},
		Data:       map[string][]byte{"api-key": []byte("new")},
	}))

	assert.Error(t, controller.syncHandler(providerName.String()))
	assert.Equal(t, "old-secret", baseStore.GetExternalModelProvider(providerName).Spec.Auth.SecretRef.Name)

	store.fail = false
	assert.NoError(t, controller.syncHandler(providerName.String()))
	assert.Equal(t, "new-secret", baseStore.GetExternalModelProvider(providerName).Spec.Auth.SecretRef.Name)
	assert.Nil(t, baseStore.GetSecret(types.NamespacedName{Namespace: "default", Name: "old-secret"}))
}

type failSecretDeleteStore struct {
	datastore.Store
	fail bool
}

func (s *failSecretDeleteStore) DeleteSecret(name types.NamespacedName) error {
	if s.fail {
		return errors.New("injected Secret deletion failure")
	}
	return s.Store.DeleteSecret(name)
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
