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

package kthena

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

const testRouterPort int32 = 18080

type renderedChart struct {
	deployments map[string]appsv1.Deployment
	services    map[string]corev1.Service
}

func TestRouterServicePorts(t *testing.T) {
	rendered := renderChart(t, routerValues(false))

	deployment, ok := rendered.deployments["kthena-router"]
	require.True(t, ok, "kthena-router Deployment was not rendered")
	router := findContainer(t, deployment, "kthena-router")
	httpPort := findContainerPort(t, router, "http")
	require.Equal(t, testRouterPort, httpPort.ContainerPort)
	require.Equal(t, corev1.ProtocolTCP, effectiveProtocol(httpPort.Protocol))

	service, ok := rendered.services["kthena-router"]
	require.True(t, ok, "kthena-router Service was not rendered")
	require.Equal(t, corev1.ServiceTypeLoadBalancer, service.Spec.Type)
	require.Len(t, service.Spec.Ports, 1, "the public inference Service must expose only its HTTP port")
	require.Equal(t, "http", service.Spec.Ports[0].Name)
	require.EqualValues(t, 80, service.Spec.Ports[0].Port)
	require.Equal(t, intstr.FromInt32(testRouterPort), service.Spec.Ports[0].TargetPort)

	webhookService, ok := rendered.services["kthena-router-webhook"]
	require.True(t, ok, "kthena-router-webhook Service was not rendered")
	require.Equal(t, corev1.ServiceTypeClusterIP, webhookService.Spec.Type)
	require.Len(t, webhookService.Spec.Ports, 1)
	require.Equal(t, "webhook", webhookService.Spec.Ports[0].Name)
	require.EqualValues(t, 443, webhookService.Spec.Ports[0].Port)
	require.Equal(t, intstr.FromInt32(8443), webhookService.Spec.Ports[0].TargetPort)
}

func TestRouterHealthAndReadinessProbes(t *testing.T) {
	tests := []struct {
		name       string
		tlsEnabled bool
		wantScheme corev1.URIScheme
	}{
		{
			name:       "HTTP",
			wantScheme: corev1.URISchemeHTTP,
		},
		{
			name:       "HTTPS",
			tlsEnabled: true,
			wantScheme: corev1.URISchemeHTTPS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered := renderChart(t, routerValues(tt.tlsEnabled))
			deployment, ok := rendered.deployments["kthena-router"]
			require.True(t, ok, "kthena-router Deployment was not rendered")
			router := findContainer(t, deployment, "kthena-router")

			require.NotNil(t, router.LivenessProbe)
			require.NotNil(t, router.LivenessProbe.HTTPGet)
			require.Equal(t, "/healthz", router.LivenessProbe.HTTPGet.Path)
			require.Equal(t, intstr.FromInt32(testRouterPort), router.LivenessProbe.HTTPGet.Port)
			require.Equal(t, tt.wantScheme, effectiveScheme(router.LivenessProbe.HTTPGet.Scheme))

			require.NotNil(t, router.ReadinessProbe)
			require.NotNil(t, router.ReadinessProbe.HTTPGet)
			require.Contains(t, []string{"/healthz", "/readyz"}, router.ReadinessProbe.HTTPGet.Path)
			require.Equal(t, intstr.FromInt32(testRouterPort), router.ReadinessProbe.HTTPGet.Port)
			require.Equal(t, tt.wantScheme, effectiveScheme(router.ReadinessProbe.HTTPGet.Scheme))
		})
	}
}

func routerValues(tlsEnabled bool) map[string]interface{} {
	values := map[string]interface{}{
		"workload": map[string]interface{}{
			"enabled": false,
		},
		"networking": map[string]interface{}{
			"enabled": true,
			"kthenaRouter": map[string]interface{}{
				"port": testRouterPort,
			},
		},
	}

	if tlsEnabled {
		values["global"] = map[string]interface{}{
			"certManagementMode": "cert-manager",
		}
		values["networking"].(map[string]interface{})["kthenaRouter"].(map[string]interface{})["tls"] = map[string]interface{}{
			"enabled": true,
		}
	}

	return values
}

func renderChart(t *testing.T, values map[string]interface{}) renderedChart {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve chart directory")
	chartDir := filepath.Dir(filename)

	chart, err := loader.Load(chartDir)
	require.NoError(t, err)
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{
		Name:      "observability-contract",
		Namespace: "kthena-system",
		Revision:  1,
		IsInstall: true,
	}, chartutil.DefaultCapabilities)
	require.NoError(t, err)

	files, err := engine.Render(chart, renderValues)
	require.NoError(t, err)

	result := renderedChart{
		deployments: make(map[string]appsv1.Deployment),
		services:    make(map[string]corev1.Service),
	}
	for _, file := range files {
		for _, manifest := range releaseutil.SplitManifests(file) {
			var header struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(manifest), &header))

			switch header.Kind {
			case "Deployment":
				var deployment appsv1.Deployment
				require.NoError(t, yaml.Unmarshal([]byte(manifest), &deployment))
				result.deployments[header.Metadata.Name] = deployment
			case "Service":
				var service corev1.Service
				require.NoError(t, yaml.Unmarshal([]byte(manifest), &service))
				result.services[header.Metadata.Name] = service
			}
		}
	}

	return result
}

func findContainer(t *testing.T, deployment appsv1.Deployment, name string) corev1.Container {
	t.Helper()
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("container %q was not rendered", name)
	return corev1.Container{}
}

func findContainerPort(t *testing.T, container corev1.Container, name string) corev1.ContainerPort {
	t.Helper()
	for _, port := range container.Ports {
		if port.Name == name {
			return port
		}
	}
	t.Fatalf("container port %q was not rendered", name)
	return corev1.ContainerPort{}
}

func effectiveProtocol(protocol corev1.Protocol) corev1.Protocol {
	if protocol == "" {
		return corev1.ProtocolTCP
	}
	return protocol
}

func effectiveScheme(scheme corev1.URIScheme) corev1.URIScheme {
	if scheme == "" {
		return corev1.URISchemeHTTP
	}
	return scheme
}
