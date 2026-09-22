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
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const legacyRevisionSource = "modelserving.volcano.sh/legacy-revision-source"
const legacyRevisionUID = "modelserving.volcano.sh/legacy-revision-uid"
const legacyRevisionBaseline = "modelserving.volcano.sh/legacy-revision-baseline"

// EnsureRevisionBaseline gives a legacy Roles-only revision a durable snapshot
// of the scheduler/plugins seen on first migration, without modifying its data
// or changing the identity carried by running Pods. Later reconciles must use
// this snapshot, not silently adopt subsequent plugin changes as a new baseline.
// A plugin edit simultaneous with the first migration cannot be distinguished
// from the initial configuration because legacy history never recorded it.
func EnsureRevisionBaseline(ctx context.Context, client kubernetes.Interface, ms *workloadv1alpha1.ModelServing, source *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	if source == nil || !metav1.IsControlledBy(source, ms) {
		return nil, fmt.Errorf("revision is missing or not controlled by the current ModelServing")
	}
	version := source.Annotations[ControllerRevisionDataVersionAnnotation]
	if version == ControllerRevisionDataVersionV1 {
		return source, nil
	}
	if version != "" {
		return nil, fmt.Errorf("unsupported revision data version %q", version)
	}
	name := source.Name + "-baseline"
	validate := func(baseline *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
		if !metav1.IsControlledBy(baseline, ms) ||
			baseline.Annotations[legacyRevisionSource] != source.Name ||
			baseline.Annotations[legacyRevisionUID] != string(source.UID) ||
			baseline.Annotations[ControllerRevisionDataVersionAnnotation] != ControllerRevisionDataVersionV1 ||
			baseline.Labels[ControllerRevisionRevisionLabelKey] != source.Labels[ControllerRevisionRevisionLabelKey] {
			return nil, fmt.Errorf("legacy baseline %s has conflicting identity", name)
		}
		if _, err := decodeRevisionPatch(baseline.Data.Raw); err != nil {
			return nil, err
		}
		return baseline, nil
	}
	baselines := client.AppsV1().ControllerRevisions(ms.Namespace)
	// Record migration in source metadata as well. If a baseline is lost later,
	// do not silently rebuild it from changed plugins and mask a real update.
	pin := func(baseline *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
		if _, err := validate(baseline); err != nil {
			return nil, err
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			live, err := baselines.Get(ctx, source.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if live.UID != source.UID || !metav1.IsControlledBy(live, ms) {
				return fmt.Errorf("legacy source %s has conflicting identity", source.Name)
			}
			if pinned := live.Annotations[legacyRevisionBaseline]; pinned != "" {
				if pinned != name {
					return fmt.Errorf("legacy source %s pins a conflicting baseline %s", source.Name, pinned)
				}
				return nil
			}
			if live.Annotations == nil {
				live.Annotations = map[string]string{}
			}
			live.Annotations[legacyRevisionBaseline] = name
			_, err = baselines.Update(ctx, live, metav1.UpdateOptions{})
			return err
		})
		return baseline, err
	}
	existing, err := baselines.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return pin(existing)
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	liveSource, err := baselines.Get(ctx, source.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if liveSource.UID != source.UID || !metav1.IsControlledBy(liveSource, ms) {
		return nil, fmt.Errorf("legacy source %s has conflicting identity", source.Name)
	}
	if liveSource.Annotations[legacyRevisionBaseline] != "" {
		return nil, fmt.Errorf("previously established legacy baseline %s is missing", name)
	}
	workload, err := ModelServingForControllerRevision(ms, source)
	if err != nil {
		return nil, err
	}
	data, err := BuildRevisionData(workload)
	if err != nil {
		return nil, err
	}
	if _, err := decodeRevisionPatch(data); err != nil {
		return nil, err
	}
	baseline := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ms.Namespace,
			Labels: map[string]string{
				ControllerRevisionLabelKey:         ms.Name,
				ControllerRevisionRevisionLabelKey: source.Labels[ControllerRevisionRevisionLabelKey],
			},
			Annotations: map[string]string{
				ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1,
				legacyRevisionSource:                    source.Name,
				legacyRevisionUID:                       string(source.UID),
			},
			OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(ms)},
		},
		Revision: source.Revision,
		Data:     runtime.RawExtension{Raw: data},
	}
	created, err := baselines.Create(ctx, baseline, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, err = baselines.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return pin(existing)
		}
	}
	if err != nil {
		return nil, err
	}
	return pin(created)
}
