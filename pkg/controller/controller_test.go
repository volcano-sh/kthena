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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func kubeClientWithNetworking(kinds ...string) *kubefake.Clientset {
	client := kubefake.NewSimpleClientset()
	if len(kinds) > 0 {
		resources := make([]metav1.APIResource, 0, len(kinds))
		for _, kind := range kinds {
			resources = append(resources, metav1.APIResource{Kind: kind})
		}
		client.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{
			GroupVersion: networkingv1alpha1.SchemeGroupVersion.String(),
			APIResources: resources,
		}}
	}
	return client
}

func TestNewModelBoosterController(t *testing.T) {
	discoveryDown := kubefake.NewSimpleClientset()
	discoveryDown.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("discovery unavailable")
	})

	tests := []struct {
		name    string
		client  *kubefake.Clientset
		started bool
	}{
		{name: "networking CRDs installed", client: kubeClientWithNetworking("ModelServer", "ModelRoute"), started: true},
		{name: "workload only install", client: kubeClientWithNetworking(), started: false},
		{name: "only ModelServer installed", client: kubeClientWithNetworking("ModelServer"), started: false},
		// A failed check keeps the controller on, so an API blip can't hide every ModelBooster.
		{name: "discovery fails", client: discoveryDown, started: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := newModelBoosterController(tt.client, kthenafake.NewSimpleClientset())
			assert.Equal(t, tt.started, mc != nil)
		})
	}
}
