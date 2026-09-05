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
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// SyncStaticEndpoints stores a ModelServer that declares `spec.endpoints`
// together with the synthetic pods representing those endpoints, so that the
// rest of the router treats statically configured and pod-discovered serving
// instances identically. Endpoints that disappeared from the spec are removed
// from the store.
func SyncStaticEndpoints(store datastore.Store, ms *aiv1alpha1.ModelServer) error {
	msName := utils.GetNamespaceName(ms)

	previous := sets.New[types.NamespacedName]()
	if podInfos, err := store.GetPodsByModelServer(msName); err == nil {
		for _, podInfo := range podInfos {
			if pod := podInfo.GetPod(); pod != nil {
				previous.Insert(utils.GetNamespaceName(pod))
			}
		}
	}

	endpointPods := make([]*corev1.Pod, 0, len(ms.Spec.Endpoints))
	current := sets.NewWithLength[types.NamespacedName](len(ms.Spec.Endpoints))
	for _, endpoint := range ms.Spec.Endpoints {
		pod := utils.EndpointPod(ms, endpoint)
		endpointPods = append(endpointPods, pod)
		current.Insert(utils.GetNamespaceName(pod))
	}

	if err := store.AddOrUpdateModelServer(ms, current); err != nil {
		return err
	}

	for _, pod := range endpointPods {
		if err := store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms}); err != nil {
			klog.Warningf("failed to add endpoint %s of model server %s to data store: %v",
				utils.GetNamespaceName(pod), msName, err)
		}
	}

	for podName := range previous.Difference(current) {
		podInfo := store.GetPodInfo(podName)
		if podInfo == nil {
			continue
		}
		// Keep instances that are still referenced by another ModelServer; only
		// that ModelServer's own sync may remove them.
		if podInfo.GetModelServers().Len() > 1 {
			continue
		}
		if err := store.DeletePod(podName); err != nil {
			klog.Warningf("failed to delete removed endpoint %s of model server %s: %v", podName, msName, err)
		}
	}

	return nil
}
