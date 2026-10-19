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

package discovery

import (
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// SyncEndpoints reconciles one ModelServer's synthetic Pods using the same
// representation for static and discovered endpoints. Callers serialize writes
// per ModelServer and validate discovered snapshots before calling this helper.
// It updates only the in-memory Store, not ModelServer resources in Kubernetes.
func SyncEndpoints(store datastore.Store, ms *api.ModelServer, endpoints []api.Endpoint) error {
	previous := sets.New[types.NamespacedName]()
	if pods, err := store.GetPodsByModelServer(utils.GetNamespaceName(ms)); err == nil {
		for _, info := range pods {
			if pod := info.GetPod(); pod != nil {
				previous.Insert(utils.GetNamespaceName(pod))
			}
		}
	}
	current := sets.New[types.NamespacedName]()
	for _, endpoint := range endpoints {
		current.Insert(types.NamespacedName{Namespace: ms.Namespace, Name: utils.EndpointPodName(ms.Name, endpoint.Name)})
	}
	if err := store.AddOrUpdateModelServer(ms, current); err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		// Keep the resolved port on the synthetic Pod, so a later ModelServer
		// update can invalidate runtime data without deleting the instance.
		port := endpointPort(ms, endpoint)
		endpoint.Port = &port
		if err := store.AddOrUpdatePod(utils.EndpointPod(ms, endpoint), []*api.ModelServer{ms}); err != nil {
			return err
		}
	}
	for name := range previous.Difference(current) {
		info := store.GetPodInfo(name)
		if info == nil || info.GetModelServers().Len() > 1 {
			continue
		}
		if err := store.DeletePod(name); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshot(ms *api.ModelServer, endpoints, previous []api.Endpoint) error {
	old := make(map[string]api.Endpoint, len(previous))
	for _, endpoint := range previous {
		old[endpoint.Name] = endpoint
	}
	seen := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		if len(validation.IsDNS1123Subdomain(endpoint.Name)) > 0 || seen[endpoint.Name] {
			return fmt.Errorf("invalid or duplicate endpoint name %q: must be a unique DNS-1123 subdomain", endpoint.Name)
		}
		seen[endpoint.Name] = true
		if net.ParseIP(endpoint.Address) == nil && (endpoint.Address == "" || len(validation.IsDNS1123Subdomain(endpoint.Address)) > 0) {
			return fmt.Errorf("endpoint %q has invalid IP address or DNS name %q", endpoint.Name, endpoint.Address)
		}
		port := endpointPort(ms, endpoint)
		if port < 1 || port > 65535 {
			return fmt.Errorf("endpoint %q has invalid port %d", endpoint.Name, port)
		}
		for key, value := range endpoint.Labels {
			if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
				return fmt.Errorf("endpoint %q has invalid label %q", endpoint.Name, key)
			}
		}
		if last, ok := old[endpoint.Name]; ok && (last.Address != endpoint.Address || endpointPort(ms, last) != port) {
			return fmt.Errorf("endpoint %q changed address or port without changing its name", endpoint.Name)
		}
	}
	return nil
}

func endpointPort(ms *api.ModelServer, endpoint api.Endpoint) int32 {
	if endpoint.Port != nil {
		return *endpoint.Port
	}
	return ms.Spec.WorkloadPort.Port
}
