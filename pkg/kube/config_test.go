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

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	kthenaclientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func writeKubeconfig(t *testing.T, name, server string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user: {}
`, server)
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}
	return path
}

// forceOutOfCluster clears the in-cluster env so BuildConfig takes the
// kubeconfig path regardless of where the test runs.
func forceOutOfCluster(t *testing.T) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func assertContentTypes(t *testing.T, config *rest.Config) {
	t.Helper()
	if config.ContentType != runtime.ContentTypeJSON {
		t.Errorf("expected ContentType %q, got %q", runtime.ContentTypeJSON, config.ContentType)
	}
	if config.AcceptContentTypes != acceptContentTypes {
		t.Errorf("expected AcceptContentTypes %q, got %q", acceptContentTypes, config.AcceptContentTypes)
	}
}

func TestBuildConfig_HonorsKubeconfigEnv(t *testing.T) {
	forceOutOfCluster(t)
	envPath := writeKubeconfig(t, "env-config", "https://from-env.example.com")
	t.Setenv("KUBECONFIG", envPath)

	config, err := BuildConfig("", "")
	if err != nil {
		t.Fatalf("BuildConfig failed: %v", err)
	}
	if config.Host != "https://from-env.example.com" {
		t.Errorf("expected host from KUBECONFIG, got %q", config.Host)
	}
	assertContentTypes(t, config)
}

func TestBuildConfig_ExplicitPathWinsOverEnv(t *testing.T) {
	forceOutOfCluster(t)
	envPath := writeKubeconfig(t, "env-config", "https://from-env.example.com")
	t.Setenv("KUBECONFIG", envPath)
	explicitPath := writeKubeconfig(t, "explicit-config", "https://from-flag.example.com")

	config, err := BuildConfig("", explicitPath)
	if err != nil {
		t.Fatalf("BuildConfig failed: %v", err)
	}
	if config.Host != "https://from-flag.example.com" {
		t.Errorf("expected host from the explicit path, got %q", config.Host)
	}
	assertContentTypes(t, config)
}

func TestBuildConfig_ContentNegotiation(t *testing.T) {
	forceOutOfCluster(t)

	var (
		podAccept       string
		crdContentType  string
		crdAccept       string
		crdRequestBody  []byte
		protoSerializer = protobuf.NewSerializer(scheme.Scheme, scheme.Scheme)
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/default/pods":
			podAccept = r.Header.Get("Accept")
			var buf bytes.Buffer
			if err := protoSerializer.Encode(&corev1.PodList{
				Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "test-pod"}}},
			}, &buf); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", runtime.ContentTypeProtobuf)
			_, _ = w.Write(buf.Bytes())
		case "/apis/workload.serving.volcano.sh/v1alpha1/namespaces/default/modelservings":
			crdContentType = r.Header.Get("Content-Type")
			crdAccept = r.Header.Get("Accept")
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			crdRequestBody = body
			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	kubeconfigPath := writeKubeconfig(t, "negotiation-config", server.URL)
	config, err := BuildConfig("", kubeconfigPath)
	if err != nil {
		t.Fatalf("BuildConfig failed: %v", err)
	}
	assertContentTypes(t, config)

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig failed: %v", err)
	}
	pods, err := kubeClient.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("built-in Pod list failed: %v", err)
	}
	if !strings.Contains(podAccept, runtime.ContentTypeProtobuf) {
		t.Errorf("expected built-in request Accept header to include %q, got %q", runtime.ContentTypeProtobuf, podAccept)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "test-pod" {
		t.Errorf("expected decoded protobuf pod list with test-pod, got %+v", pods.Items)
	}

	kthenaClient, err := kthenaclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("kthenaclientset.NewForConfig failed: %v", err)
	}
	created, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
		context.Background(),
		&workloadv1alpha1.ModelServing{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ms"},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("CRD ModelServing create failed: %v", err)
	}
	if crdContentType != runtime.ContentTypeJSON {
		t.Errorf("expected CRD request Content-Type %q, got %q", runtime.ContentTypeJSON, crdContentType)
	}
	if !strings.Contains(crdAccept, runtime.ContentTypeJSON) {
		t.Errorf("expected CRD request Accept header to include JSON fallback %q, got %q", runtime.ContentTypeJSON, crdAccept)
	}
	var decoded workloadv1alpha1.ModelServing
	if err := json.Unmarshal(crdRequestBody, &decoded); err != nil {
		t.Fatalf("expected CRD request body to be valid JSON, got error %v (body=%q)", err, string(crdRequestBody))
	}
	if decoded.Name != "test-ms" || created.Name != "test-ms" {
		t.Errorf("expected ModelServing name %q, decoded=%q created=%q", "test-ms", decoded.Name, created.Name)
	}
}
