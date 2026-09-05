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

package autoscaler

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const scalarOnePayload = `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"1"]}}`

func secretGetterFor(secrets ...*corev1.Secret) SecretGetter {
	byKey := make(map[string]*corev1.Secret, len(secrets))
	for _, s := range secrets {
		byKey[s.Namespace+"/"+s.Name] = s
	}
	return func(_ context.Context, namespace, name string) (*corev1.Secret, error) {
		s, ok := byKey[namespace+"/"+name]
		if !ok {
			return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
		}
		return s, nil
	}
}

func newSecret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			Labels:    map[string]string{workload.PrometheusAuthSecretLabelKey: workload.PrometheusAuthSecretLabelValue},
		},
		Data: make(map[string][]byte, len(data)),
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func secretKeyRef(name, key string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
}

func newAuthTestCollector(getSecret SecretGetter) *MetricCollector {
	collector := newTestCollector()
	collector.policyNamespace = "default"
	collector.getSecret = getSecret
	return collector
}

func TestFetchPrometheusMetricSendsBearerToken(t *testing.T) {
	var mu sync.Mutex
	expectedToken := "token-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		want := expectedToken
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(scalarOnePayload))
	}))
	t.Cleanup(srv.Close)

	tokenSecret := newSecret("prom-token", map[string]string{"token": "token-1"})
	collector := newAuthTestCollector(secretGetterFor(tokenSecret))

	t.Run("request without auth is rejected by the server", func(t *testing.T) {
		_, err := collector.fetchPrometheusMetric(context.Background(), &workload.PrometheusMetricSource{
			ServerURL: srv.URL,
			Query:     "up",
		})
		require.Error(t, err)
	})

	src := &workload.PrometheusMetricSource{
		ServerURL: srv.URL,
		Query:     "up",
		Auth:      &workload.PrometheusAuth{BearerTokenSecret: secretKeyRef("prom-token", "token")},
	}

	t.Run("token from secret is sent", func(t *testing.T) {
		got, err := collector.fetchPrometheusMetric(context.Background(), src)
		require.NoError(t, err)
		assert.InDelta(t, 1.0, got, 1e-9)
	})

	t.Run("rotated token replaces the cached client", func(t *testing.T) {
		mu.Lock()
		expectedToken = "token-2"
		mu.Unlock()
		tokenSecret.Data["token"] = []byte("token-2")

		got, err := collector.fetchPrometheusMetric(context.Background(), src)
		require.NoError(t, err)
		assert.InDelta(t, 1.0, got, 1e-9)
		assert.Len(t, collector.promClients, 1, "only the latest client per serverURL is kept")
	})

	t.Run("surrounding whitespace in the secret value is trimmed", func(t *testing.T) {
		mu.Lock()
		expectedToken = "token-3"
		mu.Unlock()
		tokenSecret.Data["token"] = []byte("  token-3\n")

		got, err := collector.fetchPrometheusMetric(context.Background(), src)
		require.NoError(t, err)
		assert.InDelta(t, 1.0, got, 1e-9)
	})
}

func TestFetchPrometheusMetricTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(scalarOnePayload))
	}))
	t.Cleanup(srv.Close)

	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	caSecret := newSecret("prom-ca", map[string]string{"ca.crt": caPEM})

	cases := []struct {
		name    string
		auth    *workload.PrometheusAuth
		wantErr bool
	}{
		{
			name:    "unknown server certificate is rejected",
			wantErr: true,
		},
		{
			name: "ca from secret is trusted",
			auth: &workload.PrometheusAuth{TLSConfig: &workload.PrometheusTLSConfig{CASecret: secretKeyRef("prom-ca", "ca.crt")}},
		},
		{
			name: "insecureSkipVerify accepts any certificate",
			auth: &workload.PrometheusAuth{TLSConfig: &workload.PrometheusTLSConfig{InsecureSkipVerify: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collector := newAuthTestCollector(secretGetterFor(caSecret))
			got, err := collector.fetchPrometheusMetric(context.Background(), &workload.PrometheusMetricSource{
				ServerURL: srv.URL,
				Query:     "up",
				Auth:      tc.auth,
			})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, 1.0, got, 1e-9)
		})
	}
}

func TestResolvePrometheusAuthErrors(t *testing.T) {
	tokenRef := secretKeyRef("prom-token", "token")
	cases := []struct {
		name         string
		getSecret    SecretGetter
		auth         *workload.PrometheusAuth
		errSubstring string
	}{
		{
			name:         "secret missing",
			getSecret:    secretGetterFor(),
			auth:         &workload.PrometheusAuth{BearerTokenSecret: tokenRef},
			errSubstring: "not found",
		},
		{
			name:         "secret without the opt-in label",
			getSecret:    secretGetterFor(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "prom-token"}, Data: map[string][]byte{"token": []byte("t")}}),
			auth:         &workload.PrometheusAuth{BearerTokenSecret: tokenRef},
			errSubstring: "does not have label " + workload.PrometheusAuthSecretLabelKey,
		},
		{
			name:         "key missing",
			getSecret:    secretGetterFor(newSecret("prom-token", map[string]string{"other": "x"})),
			auth:         &workload.PrometheusAuth{BearerTokenSecret: tokenRef},
			errSubstring: `no key "token"`,
		},
		{
			name:         "empty value",
			getSecret:    secretGetterFor(newSecret("prom-token", map[string]string{"token": "  "})),
			auth:         &workload.PrometheusAuth{BearerTokenSecret: tokenRef},
			errSubstring: `empty value for key "token"`,
		},
		{
			name:         "ca secret missing",
			getSecret:    secretGetterFor(),
			auth:         &workload.PrometheusAuth{TLSConfig: &workload.PrometheusTLSConfig{CASecret: secretKeyRef("prom-ca", "ca.crt")}},
			errSubstring: "caSecret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePrometheusAuth(context.Background(), tc.getSecret, "default", tc.auth)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstring)
		})
	}

	optionalRef := secretKeyRef("prom-token", "token")
	optionalRef.Optional = ptr(true)

	t.Run("optional reference tolerates a missing secret", func(t *testing.T) {
		material, err := resolvePrometheusAuth(context.Background(), secretGetterFor(), "default", &workload.PrometheusAuth{BearerTokenSecret: optionalRef})
		require.NoError(t, err)
		assert.Empty(t, material.bearerToken)
	})

	t.Run("optional reference tolerates an unlabeled secret", func(t *testing.T) {
		unlabeled := secretGetterFor(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "prom-token"}, Data: map[string][]byte{"token": []byte("t")}})
		material, err := resolvePrometheusAuth(context.Background(), unlabeled, "default", &workload.PrometheusAuth{BearerTokenSecret: optionalRef})
		require.NoError(t, err)
		assert.Empty(t, material.bearerToken)
	})

	t.Run("optional reference still reports other read errors", func(t *testing.T) {
		forbidden := func(context.Context, string, string) (*corev1.Secret, error) {
			return nil, apierrors.NewForbidden(corev1.Resource("secrets"), "prom-token", fmt.Errorf("denied"))
		}
		_, err := resolvePrometheusAuth(context.Background(), forbidden, "default", &workload.PrometheusAuth{BearerTokenSecret: optionalRef})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "denied")
	})

	t.Run("nil auth resolves to empty material", func(t *testing.T) {
		material, err := resolvePrometheusAuth(context.Background(), nil, "default", nil)
		require.NoError(t, err)
		assert.Equal(t, prometheusAuthMaterial{}, material)
	})
}

func TestSecretsAreReadFromThePolicyNamespace(t *testing.T) {
	var seen []string
	getter := func(_ context.Context, namespace, name string) (*corev1.Secret, error) {
		seen = append(seen, namespace+"/"+name)
		return nil, apierrors.NewNotFound(corev1.Resource("secrets"), name)
	}
	policy := &workload.AutoscalingPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "policy-ns"}}
	target := &workload.Target{TargetRef: corev1.ObjectReference{Namespace: "target-ns", Name: "model"}}
	collector := NewMetricCollector(target, policy, nil, getter)

	_, err := collector.getPrometheusAPI(context.Background(), &workload.PrometheusMetricSource{
		ServerURL: "http://prometheus.example:9090",
		Query:     "up",
		Auth:      &workload.PrometheusAuth{BearerTokenSecret: secretKeyRef("prom-token", "token")},
	}, time.Second)
	require.Error(t, err)
	assert.Equal(t, []string{"policy-ns/prom-token"}, seen)
}

func TestPrometheusAuthMaterialCacheKey(t *testing.T) {
	base := prometheusAuthMaterial{bearerToken: "a", caPEM: "ca"}
	assert.Equal(t, base.cacheKey(), prometheusAuthMaterial{bearerToken: "a", caPEM: "ca"}.cacheKey())
	assert.NotEqual(t, base.cacheKey(), prometheusAuthMaterial{bearerToken: "b", caPEM: "ca"}.cacheKey())
	assert.NotEqual(t, base.cacheKey(), prometheusAuthMaterial{bearerToken: "a", caPEM: "other"}.cacheKey())
	assert.NotEqual(t, base.cacheKey(), prometheusAuthMaterial{bearerToken: "a", caPEM: "ca", insecureSkipVerify: true}.cacheKey())
}

func TestPrometheusAuthMaterialRoundTripperRejectsBadCA(t *testing.T) {
	_, _, err := prometheusAuthMaterial{caPEM: "not a certificate"}.roundTripper(time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PEM certificate")
}
