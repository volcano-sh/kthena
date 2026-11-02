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
	"context"
	"fmt"
	"slices"
	"sync"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// podDeletionTargets retains one in-process deletion decision's original Pod
// identities across API failures. It is not a membership store or restart log.
// A retry never takes another snapshot or includes replacement Pods.
type podDeletionTargets struct {
	mu        sync.Mutex
	targets   map[string]types.UID
	done      map[string]bool
	namespace string
	ownerName string
	ownerUID  types.UID
}

// Delete snapshots current owned Pods once, then submits named, UID-fenced
// deletions. API acceptance completes each target here; callers separately
// observe Pod disappearance before releasing workload capacity.
func (d *podDeletionTargets) Delete(ctx context.Context, client kubernetes.Interface, ms *workload.ModelServing, selector labels.Selector) error {
	if client == nil || ms == nil || selector == nil {
		return fmt.Errorf("Pod deletion requires Kubernetes client, ModelServing and selector")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	pods := client.CoreV1().Pods(ms.Namespace)
	if d.targets == nil {
		existing, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return fmt.Errorf("snapshot Pods for ModelServing %s/%s deletion: %w", ms.Namespace, ms.Name, err)
		}
		d.targets, d.done = make(map[string]types.UID), make(map[string]bool)
		d.namespace, d.ownerName, d.ownerUID = ms.Namespace, ms.Name, ms.UID
		for i := range existing.Items {
			pod := &existing.Items[i]
			if podOwnedForDeletion(pod, ms) {
				d.targets[pod.Name] = pod.UID
			}
		}
	} else if d.namespace != ms.Namespace || d.ownerName != ms.Name || d.ownerUID != ms.UID {
		return fmt.Errorf("Pod deletion targets belong to a different ModelServing identity")
	}
	names := make([]string, 0, len(d.targets))
	for name := range d.targets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if d.done[name] {
			continue
		}
		uid := d.targets[name]
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			d.done[name] = true
			continue
		}
		if err != nil {
			return fmt.Errorf("get original Pod %s/%s for deletion: %w", ms.Namespace, name, err)
		}
		if pod.UID != uid || !podOwnedForDeletion(pod, ms) {
			d.done[name] = true
			continue
		}
		err = pods.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete original Pod %s/%s (UID %s): %w", ms.Namespace, name, uid, err)
		}
		d.done[name] = true
	}
	return nil
}

func podOwnedForDeletion(pod *corev1.Pod, ms *workload.ModelServing) bool {
	owner := metav1.GetControllerOf(pod)
	return pod.Namespace == ms.Namespace && owner != nil && owner.UID == ms.UID && owner.Name == ms.Name &&
		owner.Kind == workload.ModelServingKind.Kind && owner.APIVersion == workload.SchemeGroupVersion.String()
}

// replacements reports live Pods excluded from this deletion decision. The
// caller must wait/reconcile their identity, never expand the original targets.
func (d *podDeletionTargets) replacements(ctx context.Context, client kubernetes.Interface, namespace string, selector labels.Selector) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, pod := range pods.Items {
		if uid, known := d.targets[pod.Name]; !known || uid != pod.UID {
			names = append(names, pod.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}
