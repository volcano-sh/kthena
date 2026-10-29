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

package datastore

import (
	"sync"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

type modelServer struct {
	mutex sync.RWMutex

	modelServer *aiv1alpha1.ModelServer
	pods        sets.Set[types.NamespacedName]

	// PDGroup categorization for efficient PD scheduling.
	// Key: PD group value; Value: PDGroupPods with categorized decode/prefill pods.
	pdGroups map[string]*PDGroupPods
	// Reverse indexes updated with pdGroups under mutex, independent of PodInfo labels.
	decodePodGroups  map[types.NamespacedName]string
	prefillPodGroups map[types.NamespacedName]string
}

func newModelServer(ms *aiv1alpha1.ModelServer) *modelServer {
	return &modelServer{
		modelServer:      ms,
		pods:             sets.New[types.NamespacedName](),
		pdGroups:         make(map[string]*PDGroupPods),
		decodePodGroups:  make(map[types.NamespacedName]string),
		prefillPodGroups: make(map[types.NamespacedName]string),
	}
}

func (m *modelServer) getModelServer() *aiv1alpha1.ModelServer {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.modelServer
}

func (m *modelServer) getPods() []types.NamespacedName {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	podNames := make([]types.NamespacedName, 0, m.pods.Len())

	for podName := range m.pods {
		podNames = append(podNames, podName)
	}
	return podNames
}

func (m *modelServer) addPod(podName types.NamespacedName) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.pods.Insert(podName)
}

func (m *modelServer) deletePod(podName types.NamespacedName) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.pods.Delete(podName)
}

// categorizePodForPDGroup replaces classification without exposing an intermediate absence to readers.
func (m *modelServer) categorizePodForPDGroup(podName types.NamespacedName, podLabels map[string]string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.removePodFromPDGroupsLocked(podName)
	m.categorizePodForPDGroupLocked(podName, podLabels)
}

// categorizePodForPDGroupLocked requires m.mutex to be held for writing.
func (m *modelServer) categorizePodForPDGroupLocked(podName types.NamespacedName, podLabels map[string]string) {
	pdGroupValue := m.getPDGroupName(podLabels)
	if pdGroupValue == "" {
		return
	}
	pdGroup := m.modelServer.Spec.WorkloadSelector.PDGroup
	isDecodePod := matchesLabels(podLabels, pdGroup.DecodeLabels)
	if !isDecodePod && !matchesLabels(podLabels, pdGroup.PrefillLabels) {
		return
	}
	// Get or create PDGroupPods for this group value
	if _, exists := m.pdGroups[pdGroupValue]; !exists {
		m.pdGroups[pdGroupValue] = NewPDGroupPods()
	}
	pdGroupPods := m.pdGroups[pdGroupValue]
	// Check if pod matches decode labels
	if isDecodePod {
		pdGroupPods.AddDecodePod(podName)
		m.decodePodGroups[podName] = pdGroupValue
		return
	}

	pdGroupPods.AddPrefillPod(podName)
	m.prefillPodGroups[podName] = pdGroupValue
}

// getPDGroupName returns the PD group name for a given pod
func (m *modelServer) getPDGroupName(podLabels map[string]string) string {
	if m.modelServer.Spec.WorkloadSelector == nil || m.modelServer.Spec.WorkloadSelector.PDGroup == nil {
		return ""
	}

	pdGroup := m.modelServer.Spec.WorkloadSelector.PDGroup
	return podLabels[pdGroup.GroupKey]
}

// removePodFromPDGroups removes a pod from all PDGroup categorizations
func (m *modelServer) removePodFromPDGroups(podName types.NamespacedName) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.removePodFromPDGroupsLocked(podName)
}

// removePodFromPDGroupsLocked requires m.mutex to be held for writing.
func (m *modelServer) removePodFromPDGroupsLocked(podName types.NamespacedName) {
	pdGroupName, ok := m.decodePodGroups[podName]
	if !ok {
		pdGroupName, ok = m.prefillPodGroups[podName]
	}
	if !ok {
		return
	}
	delete(m.decodePodGroups, podName)
	delete(m.prefillPodGroups, podName)
	if pdGroup, ok := m.pdGroups[pdGroupName]; ok {
		pdGroup.RemovePod(podName)
		// Clean up empty PDGroupPods
		if pdGroup.IsEmpty() {
			delete(m.pdGroups, pdGroupName)
		}
	}
}

// getAllDecodePods returns all decode pods across all PD groups
func (m *modelServer) getAllDecodePods() []types.NamespacedName {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var result []types.NamespacedName
	for _, pdGroupPods := range m.pdGroups {
		result = append(result, pdGroupPods.GetDecodePods()...)
	}
	return result
}

// getAllPrefillPods returns all prefill pods across all PD groups
func (m *modelServer) getAllPrefillPods() []types.NamespacedName {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var result []types.NamespacedName
	for _, pdGroupPods := range m.pdGroups {
		result = append(result, pdGroupPods.GetPrefillPods()...)
	}
	return result
}

// getPrefillPodsForDecodeGroup returns prefill pods that match the same PD group as a decode pod
func (m *modelServer) getPrefillPodsForDecodeGroup(pod *PodInfo) []types.NamespacedName {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// Check if this modelServer has PDGroup configuration
	if m.modelServer.Spec.WorkloadSelector == nil || m.modelServer.Spec.WorkloadSelector.PDGroup == nil {
		return nil
	}

	// Use the same classification snapshot as pdGroups; PodInfo labels may lag behind it.
	pdGroupValue, ok := m.decodePodGroups[pod.GetPodNamespacedName()]
	if !ok {
		return nil
	}

	// Return prefill pods for the same PD group value
	if pdGroupPods, exists := m.pdGroups[pdGroupValue]; exists {
		return pdGroupPods.GetPrefillPods()
	}

	return nil
}

// getDecodePodsForPrefillPod returns decode pods paired with the given prefill
// pod (same PD group). Nil if no PDGroup config or the pod isn't a prefill.
func (m *modelServer) getDecodePodsForPrefillPod(pod *PodInfo) []types.NamespacedName {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if m.modelServer.Spec.WorkloadSelector == nil || m.modelServer.Spec.WorkloadSelector.PDGroup == nil {
		return nil
	}
	// Use the same classification snapshot as pdGroups; PodInfo labels may lag behind it.
	pdGroupValue, ok := m.prefillPodGroups[pod.GetPodNamespacedName()]
	if !ok {
		return nil
	}

	// Return decode pods for the same PD group value
	if pdGroupPods, exists := m.pdGroups[pdGroupValue]; exists {
		return pdGroupPods.GetDecodePods()
	}

	return nil
}

// matchesLabels checks if pod labels match the required labels
func matchesLabels(podLabels map[string]string, requiredLabels map[string]string) bool {
	if len(requiredLabels) == 0 {
		return false // No required labels means no match
	}

	for key, value := range requiredLabels {
		if podLabels[key] != value {
			return false
		}
	}
	return true
}
