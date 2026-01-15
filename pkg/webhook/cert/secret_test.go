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
	"errors"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestEnsureCertificate(t *testing.T) {
	tests := []struct {
		name           string
		namespace      string
		secretName     string
		dnsNames       []string
		existingSecret *corev1.Secret
		reactorError   error
		wantError      bool
		errorContains  string
		validateResult func(t *testing.T, caBundle []byte, err error)
	}{
		{
			name:          "empty dnsNames returns error",
			namespace:     "test-ns",
			secretName:    "test-secret",
			dnsNames:      []string{},
			wantError:     true,
			errorContains: "dnsNames cannot be empty",
		},
		{
			name:          "nil dnsNames returns error",
			namespace:     "test-ns",
			secretName:    "test-secret",
			dnsNames:      nil,
			wantError:     true,
			errorContains: "dnsNames cannot be empty",
		},
		{
			name:       "existing secret returns CA bundle",
			namespace:  "test-ns",
			secretName: "test-secret",
			dnsNames:   []string{"example.com"},
			existingSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-ns",
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					TLSCertKey: []byte("cert-data"),
					TLSKeyKey:  []byte("key-data"),
					CAKey:      []byte("ca-bundle-data"),
				},
			},
			wantError: false,
			validateResult: func(t *testing.T, caBundle []byte, err error) {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				if string(caBundle) != "ca-bundle-data" {
					t.Errorf("expected CA bundle 'ca-bundle-data', got: %s", string(caBundle))
				}
			},
		},
		{
			name:       "existing secret without CA key returns error",
			namespace:  "test-ns",
			secretName: "test-secret",
			dnsNames:   []string{"example.com"},
			existingSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-ns",
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					TLSCertKey: []byte("cert-data"),
					TLSKeyKey:  []byte("key-data"),
				},
			},
			wantError:     true,
			errorContains: "does not contain ca.crt",
		},
		{
			name:       "creates new secret when not found",
			namespace:  "test-ns",
			secretName: "test-secret",
			dnsNames:   []string{"example.com", "*.example.com"},
			wantError:  false,
			validateResult: func(t *testing.T, caBundle []byte, err error) {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				if len(caBundle) == 0 {
					t.Error("expected non-empty CA bundle")
				}
			},
		},
		{
			name:          "handles generic get error",
			namespace:     "test-ns",
			secretName:    "test-secret",
			dnsNames:      []string{"example.com"},
			reactorError:  errors.New("internal server error"),
			wantError:     true,
			errorContains: "failed to get secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.existingSecret != nil {
				objects = append(objects, tt.existingSecret)
			}

			client := fake.NewSimpleClientset(objects...)

			if tt.reactorError != nil {
				client.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.reactorError
				})
			}

			ctx := context.Background()
			caBundle, err := EnsureCertificate(ctx, client, tt.namespace, tt.secretName, tt.dnsNames)

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q, got: %v", tt.errorContains, err)
				}
			} else if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}

			if tt.validateResult != nil {
				tt.validateResult(t, caBundle, err)
			}
		})
	}
}

func TestEnsureCertificate_ConcurrentCreation(t *testing.T) {
	// Test the race condition scenario where another pod creates the secret
	client := fake.NewSimpleClientset()

	createCallCount := 0
	client.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		createCallCount++
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), "test-secret")
	})

	client.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if createCallCount > 0 {
			// After create attempt, return the secret
			return true, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-ns",
				},
				Data: map[string][]byte{
					CAKey: []byte("concurrent-ca-bundle"),
				},
			}, nil
		}
		return true, nil, apierrors.NewNotFound(corev1.Resource("secrets"), "test-secret")
	})

	ctx := context.Background()
	caBundle, err := EnsureCertificate(ctx, client, "test-ns", "test-secret", []string{"example.com"})

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if string(caBundle) != "concurrent-ca-bundle" {
		t.Errorf("expected CA bundle 'concurrent-ca-bundle', got: %s", string(caBundle))
	}
}

func TestUpdateValidatingWebhookCABundle(t *testing.T) {
	tests := []struct {
		name            string
		webhookName     string
		caBundle        []byte
		existingWebhook *admissionregistrationv1.ValidatingWebhookConfiguration
		reactorError    error
		wantError       bool
		errorContains   string
		expectUpdate    bool
	}{
		{
			name:        "webhook not found returns nil",
			webhookName: "test-webhook",
			caBundle:    []byte("ca-bundle"),
			wantError:   false,
		},
		{
			name:        "updates webhook with empty CA bundle",
			webhookName: "test-webhook",
			caBundle:    []byte("new-ca-bundle"),
			existingWebhook: &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-webhook",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name: "webhook1.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte{},
						},
					},
				},
			},
			expectUpdate: true,
			wantError:    false,
		},
		{
			name:        "skips update when CA bundle already present",
			webhookName: "test-webhook",
			caBundle:    []byte("new-ca-bundle"),
			existingWebhook: &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-webhook",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name: "webhook1.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte("existing-ca-bundle"),
						},
					},
				},
			},
			expectUpdate: false,
			wantError:    false,
		},
		{
			name:        "updates only webhooks with empty CA bundle",
			webhookName: "test-webhook",
			caBundle:    []byte("new-ca-bundle"),
			existingWebhook: &admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-webhook",
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name: "webhook1.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte{},
						},
					},
					{
						Name: "webhook2.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte("existing-ca"),
						},
					},
				},
			},
			expectUpdate: true,
			wantError:    false,
		},
		{
			name:          "handles get error",
			webhookName:   "test-webhook",
			caBundle:      []byte("ca-bundle"),
			reactorError:  errors.New("api error"),
			wantError:     true,
			errorContains: "failed to get ValidatingWebhookConfiguration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.existingWebhook != nil {
				objects = append(objects, tt.existingWebhook)
			}

			client := fake.NewSimpleClientset(objects...)

			if tt.reactorError != nil {
				client.PrependReactor("get", "validatingwebhookconfigurations", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.reactorError
				})
			}

			ctx := context.Background()
			err := UpdateValidatingWebhookCABundle(ctx, client, tt.webhookName, tt.caBundle)

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q, got: %v", tt.errorContains, err)
				}
			} else if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}

			if tt.expectUpdate && tt.existingWebhook != nil {
				// Verify the webhook was updated
				updated, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, tt.webhookName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get updated webhook: %v", err)
				}
				for i := range updated.Webhooks {
					if len(tt.existingWebhook.Webhooks[i].ClientConfig.CABundle) == 0 {
						if string(updated.Webhooks[i].ClientConfig.CABundle) != string(tt.caBundle) {
							t.Errorf("webhook %d: expected CA bundle %q, got %q",
								i, string(tt.caBundle), string(updated.Webhooks[i].ClientConfig.CABundle))
						}
					}
				}
			}
		})
	}
}

func TestUpdateMutatingWebhookCABundle(t *testing.T) {
	tests := []struct {
		name            string
		webhookName     string
		caBundle        []byte
		existingWebhook *admissionregistrationv1.MutatingWebhookConfiguration
		reactorError    error
		wantError       bool
		errorContains   string
		expectUpdate    bool
	}{
		{
			name:        "webhook not found returns nil",
			webhookName: "test-webhook",
			caBundle:    []byte("ca-bundle"),
			wantError:   false,
		},
		{
			name:        "updates webhook with empty CA bundle",
			webhookName: "test-webhook",
			caBundle:    []byte("new-ca-bundle"),
			existingWebhook: &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-webhook",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name: "webhook1.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte{},
						},
					},
				},
			},
			expectUpdate: true,
			wantError:    false,
		},
		{
			name:        "skips update when CA bundle already present",
			webhookName: "test-webhook",
			caBundle:    []byte("new-ca-bundle"),
			existingWebhook: &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-webhook",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name: "webhook1.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte("existing-ca-bundle"),
						},
					},
				},
			},
			expectUpdate: false,
			wantError:    false,
		},
		{
			name:        "updates only webhooks with empty CA bundle",
			webhookName: "test-webhook",
			caBundle:    []byte("new-ca-bundle"),
			existingWebhook: &admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-webhook",
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name: "webhook1.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte{},
						},
					},
					{
						Name: "webhook2.example.com",
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							CABundle: []byte("existing-ca"),
						},
					},
				},
			},
			expectUpdate: true,
			wantError:    false,
		},
		{
			name:          "handles get error",
			webhookName:   "test-webhook",
			caBundle:      []byte("ca-bundle"),
			reactorError:  errors.New("api error"),
			wantError:     true,
			errorContains: "failed to get MutatingWebhookConfiguration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.existingWebhook != nil {
				objects = append(objects, tt.existingWebhook)
			}

			client := fake.NewSimpleClientset(objects...)

			if tt.reactorError != nil {
				client.PrependReactor("get", "mutatingwebhookconfigurations", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.reactorError
				})
			}

			ctx := context.Background()
			err := UpdateMutatingWebhookCABundle(ctx, client, tt.webhookName, tt.caBundle)

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error containing %q, got: %v", tt.errorContains, err)
				}
			} else if err != nil {
				t.Errorf("expected no error, got: %v", err)
			}

			if tt.expectUpdate && tt.existingWebhook != nil {
				// Verify the webhook was updated
				updated, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, tt.webhookName, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get updated webhook: %v", err)
				}
				if len(updated.Webhooks) != len(tt.existingWebhook.Webhooks) {
					t.Fatalf("expected %d webhooks after update, got %d", len(tt.existingWebhook.Webhooks), len(updated.Webhooks))
				}
				for i, updatedWebhook := range updated.Webhooks {
					if len(tt.existingWebhook.Webhooks[i].ClientConfig.CABundle) == 0 {
						if string(updatedWebhook.ClientConfig.CABundle) != string(tt.caBundle) {
							t.Errorf("webhook %d: expected CA bundle %q, got %q",
								i, string(tt.caBundle), string(updatedWebhook.ClientConfig.CABundle))
						}
					}
				}
			}
		})
	}
}

func TestLoadCertBundleFromSecret(t *testing.T) {
	tests := []struct {
		name           string
		namespace      string
		secretName     string
		existingSecret *corev1.Secret
		reactorError   error
		wantError      bool
		wantNil        bool
		expectedBundle *CertBundle
	}{
		{
			name:       "secret not found returns nil",
			namespace:  "test-ns",
			secretName: "test-secret",
			wantError:  false,
			wantNil:    true,
		},
		{
			name:       "loads complete cert bundle",
			namespace:  "test-ns",
			secretName: "test-secret",
			existingSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-ns",
				},
				Data: map[string][]byte{
					TLSCertKey: []byte("cert-data"),
					TLSKeyKey:  []byte("key-data"),
					CAKey:      []byte("ca-data"),
				},
			},
			wantError: false,
			expectedBundle: &CertBundle{
				CertPEM: []byte("cert-data"),
				KeyPEM:  []byte("key-data"),
				CAPEM:   []byte("ca-data"),
			},
		},
		{
			name:       "loads partial cert bundle",
			namespace:  "test-ns",
			secretName: "test-secret",
			existingSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-ns",
				},
				Data: map[string][]byte{
					TLSCertKey: []byte("cert-data"),
				},
			},
			wantError: false,
			expectedBundle: &CertBundle{
				CertPEM: []byte("cert-data"),
			},
		},
		{
			name:       "secret with nil data returns nil bundle",
			namespace:  "test-ns",
			secretName: "test-secret",
			existingSecret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "test-ns",
				},
				Data: nil,
			},
			wantError: false,
			wantNil:   true,
		},
		{
			name:         "handles get error",
			namespace:    "test-ns",
			secretName:   "test-secret",
			reactorError: errors.New("api error"),
			wantError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.existingSecret != nil {
				objects = append(objects, tt.existingSecret)
			}

			client := fake.NewSimpleClientset(objects...)

			if tt.reactorError != nil {
				client.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.reactorError
				})
			}

			ctx := context.Background()
			bundle, err := LoadCertBundleFromSecret(ctx, client, tt.namespace, tt.secretName)

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("expected no error, got: %v", err)
				return
			}

			if tt.wantNil {
				if bundle != nil {
					t.Errorf("expected nil bundle, got: %+v", bundle)
				}
				return
			}

			if tt.expectedBundle != nil {
				if bundle == nil {
					t.Fatal("expected non-nil bundle, got nil")
				}
				if string(bundle.CertPEM) != string(tt.expectedBundle.CertPEM) {
					t.Errorf("CertPEM mismatch: expected %q, got %q",
						string(tt.expectedBundle.CertPEM), string(bundle.CertPEM))
				}
				if string(bundle.KeyPEM) != string(tt.expectedBundle.KeyPEM) {
					t.Errorf("KeyPEM mismatch: expected %q, got %q",
						string(tt.expectedBundle.KeyPEM), string(bundle.KeyPEM))
				}
				if string(bundle.CAPEM) != string(tt.expectedBundle.CAPEM) {
					t.Errorf("CAPEM mismatch: expected %q, got %q",
						string(tt.expectedBundle.CAPEM), string(bundle.CAPEM))
				}
			}
		})
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
