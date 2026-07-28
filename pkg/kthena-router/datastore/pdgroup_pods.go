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

	"istio.io/istio/pkg/util/sets"
	"k8s.io/apimachinery/pkg/types"
)

// PDGroupPods holds the categorized pods for a specific PD group value
type PDGroupPods struct {
	mutex       sync.RWMutex
	decodePods  sets.Set[types.NamespacedName] // Pods that match decode labels
	prefillPods sets.Set[types.NamespacedName] // Pods that match prefill labels
	encodePods  sets.Set[types.NamespacedName] // Pods that match encode labels
}

// NewPDGroupPods creates a new PDGroupPods instance
func NewPDGroupPods() *PDGroupPods {
	return &PDGroupPods{
		decodePods:  sets.New[types.NamespacedName](),
		prefillPods: sets.New[types.NamespacedName](),
		encodePods:  sets.New[types.NamespacedName](),
	}
}

// AddDecodePod adds a pod to the decode pods set
func (p *PDGroupPods) AddDecodePod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.decodePods.Insert(podName)
}

// AddPrefillPod adds a pod to the prefill pods set
func (p *PDGroupPods) AddPrefillPod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.prefillPods.Insert(podName)
}

// AddEncodePod adds a pod to the encode pods set
func (p *PDGroupPods) AddEncodePod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.encodePods.Insert(podName)
}

// RemoveDecodePod removes a pod from the decode pods set
func (p *PDGroupPods) RemoveDecodePod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.decodePods.Delete(podName)
}

// RemovePrefillPod removes a pod from the prefill pods set
func (p *PDGroupPods) RemovePrefillPod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.prefillPods.Delete(podName)
}

// RemoveEncodePod removes a pod from the encode pods set
func (p *PDGroupPods) RemoveEncodePod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.encodePods.Delete(podName)
}

// RemovePod removes a pod from decode, prefill, and encode sets
func (p *PDGroupPods) RemovePod(podName types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.decodePods.Delete(podName)
	p.prefillPods.Delete(podName)
	p.encodePods.Delete(podName)
}

// GetDecodePods returns a copy of decode pods
func (p *PDGroupPods) GetDecodePods() []types.NamespacedName {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.decodePods.UnsortedList()
}

// GetPrefillPods returns a copy of prefill pods
func (p *PDGroupPods) GetPrefillPods() []types.NamespacedName {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.prefillPods.UnsortedList()
}

// GetEncodePods returns a copy of encode pods
func (p *PDGroupPods) GetEncodePods() []types.NamespacedName {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.encodePods.UnsortedList()
}

// IsEmpty returns true if decode, prefill, and encode pod sets are empty
func (p *PDGroupPods) IsEmpty() bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.decodePods.Len() == 0 && p.prefillPods.Len() == 0 && p.encodePods.Len() == 0
}
