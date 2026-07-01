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

	"github.com/stretchr/testify/assert"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureCertificateCreatesSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	caBundle, err := EnsureCertificate(ctx, client, "default", "webhook-certs", []string{"webhook.default.svc"})
	assert.NoError(t, err)
	assert.NotEmpty(t, caBundle)
}

func TestEnsureCertificateReusesExistingSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			CAKey: []byte("existing-ca"),
		},
	}
	_, err := client.CoreV1().Secrets("default").Create(ctx, existingSecret, metav1.CreateOptions{})
	assert.NoError(t, err)

	caBundle, err := EnsureCertificate(ctx, client, "default", "webhook-certs", []string{"webhook.default.svc"})
	assert.NoError(t, err)
	assert.Equal(t, []byte("existing-ca"), caBundle)
}

func TestEnsureCertificateRequiresDNSNames(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	_, err := EnsureCertificate(ctx, client, "default", "webhook-certs", []string{})
	assert.Error(t, err)
}

func TestLoadCertBundleFromSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			TLSCertKey: []byte("cert-data"),
			TLSKeyKey:  []byte("key-data"),
			CAKey:      []byte("ca-data"),
		},
	}
	_, err := client.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
	assert.NoError(t, err)

	bundle, err := LoadCertBundleFromSecret(ctx, client, "default", "webhook-certs")
	assert.NoError(t, err)
	assert.NotNil(t, bundle)
	assert.Equal(t, []byte("cert-data"), bundle.CertPEM)
	assert.Equal(t, []byte("key-data"), bundle.KeyPEM)
	assert.Equal(t, []byte("ca-data"), bundle.CAPEM)
}

func TestLoadCertBundleFromSecretReturnsNilForMissingSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	bundle, err := LoadCertBundleFromSecret(ctx, client, "default", "missing")
	assert.NoError(t, err)
	assert.Nil(t, bundle)
}

func TestUpdateValidatingWebhookCABundleReconcile(t *testing.T) {
	tests := []struct {
		name       string
		existingCA []byte
		desiredCA  []byte
		expectCA   []byte
	}{
		{
			name:       "empty caBundle gets filled",
			existingCA: []byte{},
			desiredCA:  []byte("new-ca-cert"),
			expectCA:   []byte("new-ca-cert"),
		},
		{
			name:       "matching caBundle stays unchanged",
			existingCA: []byte("same-ca-cert"),
			desiredCA:  []byte("same-ca-cert"),
			expectCA:   []byte("same-ca-cert"),
		},
		{
			name:       "stale non-empty caBundle gets overwritten",
			existingCA: []byte("old-ca-cert"),
			desiredCA:  []byte("new-ca-cert"),
			expectCA:   []byte("new-ca-cert"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			webhookConfig := &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-validating-webhook",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: tt.existingCA,
						},
					},
				},
			}

			client := fake.NewSimpleClientset(webhookConfig)

			err := UpdateValidatingWebhookCABundle(
				context.Background(),
				client,
				"test-validating-webhook",
				tt.desiredCA,
			)
			assert.NoError(t, err)

			result, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
				context.Background(),
				"test-validating-webhook",
				metav1.GetOptions{},
			)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectCA, result.Webhooks[0].ClientConfig.CABundle)
		})
	}
}

func TestUpdateMutatingWebhookCABundleReconcile(t *testing.T) {
	tests := []struct {
		name       string
		existingCA []byte
		desiredCA  []byte
		expectCA   []byte
	}{
		{
			name:       "empty caBundle gets filled",
			existingCA: []byte{},
			desiredCA:  []byte("new-ca-cert"),
			expectCA:   []byte("new-ca-cert"),
		},
		{
			name:       "matching caBundle stays unchanged",
			existingCA: []byte("same-ca-cert"),
			desiredCA:  []byte("same-ca-cert"),
			expectCA:   []byte("same-ca-cert"),
		},
		{
			name:       "stale non-empty caBundle gets overwritten",
			existingCA: []byte("old-ca-cert"),
			desiredCA:  []byte("new-ca-cert"),
			expectCA:   []byte("new-ca-cert"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			webhookConfig := &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-mutating-webhook",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: tt.existingCA,
						},
					},
				},
			}

			client := fake.NewSimpleClientset(webhookConfig)

			err := UpdateMutatingWebhookCABundle(
				context.Background(),
				client,
				"test-mutating-webhook",
				tt.desiredCA,
			)
			assert.NoError(t, err)

			result, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
				context.Background(),
				"test-mutating-webhook",
				metav1.GetOptions{},
			)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectCA, result.Webhooks[0].ClientConfig.CABundle)
		})
	}
}

func TestUpdateValidatingWebhookCABundleNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()

	err := UpdateValidatingWebhookCABundle(
		context.Background(),
		client,
		"nonexistent-webhook",
		[]byte("ca-cert"),
	)
	assert.NoError(t, err)
}

func TestUpdateMutatingWebhookCABundleNotFound(t *testing.T) {
	client := fake.NewSimpleClientset()

	err := UpdateMutatingWebhookCABundle(
		context.Background(),
		client,
		"nonexistent-webhook",
		[]byte("ca-cert"),
	)
	assert.NoError(t, err)
}
