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

package cert

import (
	"context"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureCertificateCreatesSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	caBundle, err := EnsureCertificate(ctx, client, "default", "webhook-certs", []string{"webhook.default.svc"})
	require.NoError(t, err)
	require.NotEmpty(t, caBundle)

	secret, err := client.CoreV1().Secrets("default").Get(ctx, "webhook-certs", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, corev1.SecretTypeTLS, secret.Type)
	assert.NotEmpty(t, secret.Data[TLSCertKey])
	assert.NotEmpty(t, secret.Data[TLSKeyKey])
	assert.Equal(t, caBundle, secret.Data[CAKey])
}

func TestEnsureCertificateReusesExistingSecret(t *testing.T) {
	ctx := context.Background()
	existingCA := []byte("existing-ca")
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			TLSCertKey: []byte("existing-cert"),
			TLSKeyKey:  []byte("existing-key"),
			CAKey:      existingCA,
		},
	})

	caBundle, err := EnsureCertificate(ctx, client, "default", "webhook-certs", []string{"webhook.default.svc"})
	require.NoError(t, err)
	assert.Equal(t, existingCA, caBundle)

	secret, err := client.CoreV1().Secrets("default").Get(ctx, "webhook-certs", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("existing-cert"), secret.Data[TLSCertKey])
	assert.Equal(t, []byte("existing-key"), secret.Data[TLSKeyKey])
}

func TestEnsureCertificateRequiresDNSNames(t *testing.T) {
	caBundle, err := EnsureCertificate(context.Background(), fake.NewSimpleClientset(), "default", "webhook-certs", nil)
	require.Error(t, err)
	assert.Nil(t, caBundle)
	assert.Contains(t, err.Error(), "dnsNames cannot be empty")
}

func TestLoadCertBundleFromSecret(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			TLSCertKey: []byte("cert"),
			TLSKeyKey:  []byte("key"),
			CAKey:      []byte("ca"),
		},
	})

	bundle, err := LoadCertBundleFromSecret(ctx, client, "default", "webhook-certs")
	require.NoError(t, err)
	require.NotNil(t, bundle)
	assert.Equal(t, []byte("cert"), bundle.CertPEM)
	assert.Equal(t, []byte("key"), bundle.KeyPEM)
	assert.Equal(t, []byte("ca"), bundle.CAPEM)
}

func TestLoadCertBundleFromSecretReturnsNilForMissingSecret(t *testing.T) {
	bundle, err := LoadCertBundleFromSecret(context.Background(), fake.NewSimpleClientset(), "default", "missing")
	require.NoError(t, err)
	assert.Nil(t, bundle)
}

func TestUpdateValidatingWebhookCABundleOnlyFillsEmptyBundles(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "validating-webhook"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name: "empty.example.com",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: nil,
				},
			},
			{
				Name: "manual.example.com",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: []byte("manual-ca"),
				},
			},
		},
	})

	err := UpdateValidatingWebhookCABundle(ctx, client, "validating-webhook", []byte("generated-ca"))
	require.NoError(t, err)

	webhook, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "validating-webhook", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("generated-ca"), webhook.Webhooks[0].ClientConfig.CABundle)
	assert.Equal(t, []byte("manual-ca"), webhook.Webhooks[1].ClientConfig.CABundle)
}

func TestUpdateMutatingWebhookCABundleOnlyFillsEmptyBundles(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mutating-webhook"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: "empty.example.com",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: nil,
				},
			},
			{
				Name: "manual.example.com",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					CABundle: []byte("manual-ca"),
				},
			},
		},
	})

	err := UpdateMutatingWebhookCABundle(ctx, client, "mutating-webhook", []byte("generated-ca"))
	require.NoError(t, err)

	webhook, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, "mutating-webhook", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("generated-ca"), webhook.Webhooks[0].ClientConfig.CABundle)
	assert.Equal(t, []byte("manual-ca"), webhook.Webhooks[1].ClientConfig.CABundle)
}

func TestUpdateWebhookCABundleIgnoresMissingConfigurations(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	require.NoError(t, UpdateValidatingWebhookCABundle(ctx, client, "missing-validating", []byte("ca")))
	require.NoError(t, UpdateMutatingWebhookCABundle(ctx, client, "missing-mutating", []byte("ca")))
}
