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
	"fmt"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pendingSince returns the time since which the pods of one ServingGroup have
// been waiting for a node, or nil if they are not. A gang that cannot be
// satisfied has members that would fit on their own, so one unschedulable Pod
// is enough.
func pendingSince(pods []*corev1.Pod) *metav1.Time {
	var since *metav1.Time
	for _, pod := range pods {
		if utils.IsPodTerminating(pod) {
			continue
		}
		if t := utils.PodUnschedulableSince(pod); t != nil && (since == nil || t.Before(since)) {
			since = t
		}
	}
	return since
}

// servingGroupPendingSince returns the time since which the scheduler has been
// unable to place the ServingGroup, or nil if it can.
func (c *ModelServingController) servingGroupPendingSince(ms *workloadv1alpha1.ModelServing, groupName string) (*metav1.Time, error) {
	pods, err := c.ownedPodsByIndex(ms, GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName))
	if err != nil {
		return nil, err
	}
	return pendingSince(pods), nil
}
