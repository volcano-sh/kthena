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
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// TLSCertKey is the key for the TLS certificate in the secret
	TLSCertKey = "tls.crt"
	// TLSKeyKey is the key for the TLS private key in the secret
	TLSKeyKey = "tls.key"
	// CAKey is the key for the CA certificate in the secret
	CAKey = "ca.crt"
)

// EnsureCertificate ensures that a certificate exists for the webhook server.
// If the secret doesn't exist, it generates a new certificate and creates the secret.
// If the secret already exists, it returns without error (reusing existing certificate).
// Returns the CA bundle bytes that can be used to update webhook configurations.
func EnsureCertificate(ctx context.Context, kubeClient kubernetes.Interface, namespace, secretName string, dnsNames []string) ([]byte, error) {
	if len(dnsNames) == 0 {
		return nil, fmt.Errorf("dnsNames cannot be empty")
	}

	klog.Infof("Ensuring certificate exists in secret %s/%s", namespace, secretName)

	// Try to get the existing secret
	secret, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		// Secret exists, use it and return the CA bundle
		klog.Infof("Found existing secret %s/%s, using existing certificate", namespace, secretName)
		caBundle, ok := secret.Data[CAKey]
		if !ok {
			return nil, fmt.Errorf("secret %s/%s does not contain %s", namespace, secretName, CAKey)
		}
		return caBundle, nil
	}

	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", namespace, secretName, err)
	}

	// Secret doesn't exist, generate new certificate
	klog.Infof("Secret %s/%s not found, generating new certificate", namespace, secretName)
	certBundle, err := GenerateSelfSignedCertificate(dnsNames)
	if err != nil {
		return nil, fmt.Errorf("failed to generate certificate: %w", err)
	}

	// Create the secret
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			TLSCertKey: certBundle.CertPEM,
			TLSKeyKey:  certBundle.KeyPEM,
			CAKey:      certBundle.CAPEM,
		},
	}

	_, err = kubeClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another pod created the secret concurrently, fetch it to get the CA bundle
			klog.Infof("Secret %s/%s was created by another pod, fetching CA bundle", namespace, secretName)
			secret, err = kubeClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to get secret after concurrent creation: %w", err)
			}
			caBundle, ok := secret.Data[CAKey]
			if !ok {
				return nil, fmt.Errorf("secret %s/%s does not contain %s", namespace, secretName, CAKey)
			}
			return caBundle, nil
		}
		return nil, fmt.Errorf("failed to create secret %s/%s: %w", namespace, secretName, err)
	}

	klog.Infof("Successfully created secret %s/%s with generated certificate", namespace, secretName)
	return certBundle.CAPEM, nil
}

// UpdateValidatingWebhookCABundle updates the ValidatingWebhookConfiguration with the provided CA bundle
func UpdateValidatingWebhookCABundle(ctx context.Context, kubeClient kubernetes.Interface, webhookName string, caBundle []byte) error {
	// Get the ValidatingWebhookConfiguration
	webhook, err := kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, webhookName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("ValidatingWebhookConfiguration %s not found, skipping CA bundle update", webhookName)
			return nil
		}
		return fmt.Errorf("failed to get ValidatingWebhookConfiguration %s: %w", webhookName, err)
	}

	// Update all webhooks with the CA bundle
	updated := false
	for i := range webhook.Webhooks {
		// Update if caBundle differs from desired CA (reconcile)
		if !bytes.Equal(webhook.Webhooks[i].ClientConfig.CABundle, caBundle) {
			webhook.Webhooks[i].ClientConfig.CABundle = caBundle
			updated = true
		}
	}

	if !updated {
		klog.Infof("ValidatingWebhookConfiguration %s already has CA bundle, skipping update", webhookName)
		return nil
	}

	// Update the ValidatingWebhookConfiguration
	_, err = kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, webhook, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ValidatingWebhookConfiguration %s: %w", webhookName, err)
	}

	klog.Infof("Successfully updated ValidatingWebhookConfiguration %s with CA bundle", webhookName)
	return nil
}

// UpdateMutatingWebhookCABundle updates the MutatingWebhookConfiguration with the provided CA bundle
func UpdateMutatingWebhookCABundle(ctx context.Context, kubeClient kubernetes.Interface, webhookName string, caBundle []byte) error {
	// Get the MutatingWebhookConfiguration
	webhook, err := kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, webhookName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("MutatingWebhookConfiguration %s not found, skipping CA bundle update", webhookName)
			return nil
		}
		return fmt.Errorf("failed to get MutatingWebhookConfiguration %s: %w", webhookName, err)
	}

	// Update all webhooks with the CA bundle
	updated := false
	for i := range webhook.Webhooks {
		// Update if caBundle differs from desired CA (reconcile)
		if !bytes.Equal(webhook.Webhooks[i].ClientConfig.CABundle, caBundle) {
			webhook.Webhooks[i].ClientConfig.CABundle = caBundle
			updated = true
		}
	}

	if !updated {
		klog.Infof("MutatingWebhookConfiguration %s already has CA bundle, skipping update", webhookName)
		return nil
	}

	// Update the MutatingWebhookConfiguration
	_, err = kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Update(ctx, webhook, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update MutatingWebhookConfiguration %s: %w", webhookName, err)
	}

	klog.Infof("Successfully updated MutatingWebhookConfiguration %s with CA bundle", webhookName)
	return nil
}

// LoadCertBundleFromSecret tries to read key cert bundle from a Kubernetes Secret.
func LoadCertBundleFromSecret(ctx context.Context, kubeClient kubernetes.Interface, namespace, secretName string) (*CertBundle, error) {
	s, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if s.Data == nil {
		return nil, nil
	}

	var bundle CertBundle
	if b, ok := s.Data[TLSCertKey]; ok && len(b) > 0 {
		bundle.CertPEM = b
	}
	if b, ok := s.Data[TLSKeyKey]; ok && len(b) > 0 {
		bundle.KeyPEM = b
	}
	if b, ok := s.Data[CAKey]; ok && len(b) > 0 {
		bundle.CAPEM = b
	}
	return &bundle, nil
}
