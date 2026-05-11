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

package utils

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	lwsclientset "sigs.k8s.io/lws/client-go/clientset/versioned"
)

// GetKubeConfig returns a Kubernetes REST config.
// It tries in-cluster config first, then falls back to kubeconfig file.
func GetKubeConfig() (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// Fall back to kubeconfig
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

// GetKubeClient returns a Kubernetes clientset.
func GetKubeClient() (*kubernetes.Clientset, error) {
	config, err := GetKubeConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

// GetLWSClient returns an LWS clientset.
func GetLWSClient() (*lwsclientset.Clientset, error) {
	config, err := GetKubeConfig()
	if err != nil {
		return nil, err
	}
	return lwsclientset.NewForConfig(config)
}

// LoadYAMLFromFile loads a YAML file from a path relative to the project root
// and unmarshals it into the specified type.
func LoadYAMLFromFile[T any](path string) *T {
	_, filename, _, _ := runtime.Caller(0)
	// Current file is test/e2e/utils/config.go, project root is 3 levels up
	projectRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..")
	absPath := filepath.Join(projectRoot, path)

	data, err := os.ReadFile(absPath)
	if err != nil {
		panic(fmt.Sprintf("Failed to read YAML file from project root: %s (abs: %s): %v", path, absPath, err))
	}

	var obj T
	if err := yaml.Unmarshal(data, &obj); err != nil {
		panic(fmt.Sprintf("Failed to unmarshal YAML file: %s: %v", absPath, err))
	}

	return &obj
}

// LoadMultiResourceYAMLFromFile loads a multi-resource YAML file from a path relative to the project root
// and returns all resources as a slice of the specified type.
// The YAML file can contain multiple resources separated by "---".
func LoadMultiResourceYAMLFromFile[T any](path string) []*T {
	_, filename, _, _ := runtime.Caller(1)
	// Get project root (3 levels up from test/e2e/utils/config.go)
	projectRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..")
	absPath := filepath.Join(projectRoot, path)

	data, err := os.ReadFile(absPath)
	if err != nil {
		panic(fmt.Sprintf("Failed to read YAML file from project root: %s (abs: %s): %v", path, absPath, err))
	}

	// Use NewYAMLOrJSONDecoder to safely parse multi-document YAML files
	// This follows the same approach as kubectl and handles "---" delimiters correctly
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 1024)
	var results []*T
	resourceIndex := 0

	for {
		var obj T
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			panic(fmt.Sprintf("Failed to decode YAML resource %d in file: %s: %v", resourceIndex+1, absPath, err))
		}

		results = append(results, &obj)
		resourceIndex++
	}

	return results
}

// LoadUnstructuredYAMLFromFile loads a multi-resource YAML file and returns each resource as an Unstructured object.
func LoadUnstructuredYAMLFromFile(path string) []*unstructured.Unstructured {
	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..")
	absPath := filepath.Join(projectRoot, path)

	data, err := os.ReadFile(absPath)
	if err != nil {
		panic(fmt.Sprintf("Failed to read YAML file from project root: %s (abs: %s): %v", path, absPath, err))
	}

	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 1024)
	var results []*unstructured.Unstructured
	resourceIndex := 0

	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			panic(fmt.Sprintf("Failed to decode YAML resource %d in file: %s: %v", resourceIndex+1, absPath, err))
		}

		if len(obj.Object) == 0 {
			continue
		}

		results = append(results, &obj)
		resourceIndex++
	}

	return results
}

// RandomString generates a random string of length n.
func RandomString(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}
