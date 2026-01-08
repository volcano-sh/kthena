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

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureCertificate_CreateSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	ca, err := EnsureCertificate(
		ctx,
		client,
		"default",
		"test-secret",
		[]string{"webhook.default.svc"},
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ca) == 0 {
		t.Fatalf("expected CA bundle, got empty")
	}
}

func TestEnsureCertificate_ReuseExistingSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			CAKey: []byte("test-ca"),
		},
	}

	_, _ = client.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})

	ca, err := EnsureCertificate(
		ctx,
		client,
		"default",
		"existing-secret",
		[]string{"webhook.default.svc"},
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(ca) != "test-ca" {
		t.Fatalf("expected existing CA bundle")
	}
}

func TestUpdateValidatingWebhookCABundle(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-validating",
		},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name: "vhook.test",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: nil,
				},
			},
		},
	}

	_, _ = client.AdmissionregistrationV1().
		ValidatingWebhookConfigurations().
		Create(ctx, vwc, metav1.CreateOptions{})

	err := UpdateValidatingWebhookCABundle(ctx, client, "test-validating", []byte("ca"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateMutatingWebhookCABundle(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-mutating",
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name: "mhook.test",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: nil,
				},
			},
		},
	}

	_, _ = client.AdmissionregistrationV1().
		MutatingWebhookConfigurations().
		Create(ctx, mwc, metav1.CreateOptions{})

	err := UpdateMutatingWebhookCABundle(ctx, client, "test-mutating", []byte("ca"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadCertBundleFromSecret(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bundle-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			TLSCertKey: []byte("cert"),
			TLSKeyKey:  []byte("key"),
			CAKey:      []byte("ca"),
		},
	}

	_, _ = client.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})

	bundle, err := LoadCertBundleFromSecret(ctx, client, "default", "bundle-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if string(bundle.CertPEM) != "cert" {
		t.Fatalf("cert mismatch")
	}
	if string(bundle.KeyPEM) != "key" {
		t.Fatalf("key mismatch")
	}
	if string(bundle.CAPEM) != "ca" {
		t.Fatalf("ca mismatch")
	}
}
