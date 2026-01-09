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
	"fmt"
	"strconv"
	"testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeModelServingController struct {
	pods map[string][]*corev1.Pod
	err  error
}

func (f *fakeModelServingController) getPodsByIndex(key, value string) ([]*corev1.Pod, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.pods[value], nil
}

func (f *fakeModelServingController) getPodDeletionCost(pod *corev1.Pod) int {
	if costStr, ok := pod.Annotations[PodDeletionCostAnnotation]; ok {
		cost, _ := strconv.Atoi(costStr)
		return cost
	}
	return 0
}

func (f *fakeModelServingController) calculateRoleScore(
	mi *workloadv1alpha1.ModelServing,
	groupName, roleName, roleID string,
) (int, error) {
	roleIDValue := fmt.Sprintf("%s/%s/%s/%s", mi.Namespace, groupName, roleName, roleID)

	pods, err := f.getPodsByIndex(RoleIDKey, roleIDValue)
	if err != nil {
		return 0, err
	}

	score := 0
	for _, pod := range pods {
		score += f.getPodDeletionCost(pod)
	}
	return score, nil
}

func (f *fakeModelServingController) calculateServingGroupScore(
	mi *workloadv1alpha1.ModelServing,
	groupName string,
) (int, error) {
	groupNameValue := fmt.Sprintf("%s/%s", mi.Namespace, groupName)

	pods, err := f.getPodsByIndex(GroupNameKey, groupNameValue)
	if err != nil {
		return 0, err
	}

	score := 0
	for _, pod := range pods {
		score += f.getPodDeletionCost(pod)
	}
	return score, nil
}

func TestCalculateRoleScoreSuccess(t *testing.T) {
	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
		},
	}

	key := "default/groupA/worker/1"

	fake := &fakeModelServingController{
		pods: map[string][]*corev1.Pod{
			key: {
				{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							PodDeletionCostAnnotation: "5",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							PodDeletionCostAnnotation: "3",
						},
					},
				},
			},
		},
	}

	score, err := fake.calculateRoleScore(mi, "groupA", "worker", "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 8 {
		t.Fatalf("expected 8, got %d", score)
	}
}

func TestCalculateRoleScoreError(t *testing.T) {
	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
		},
	}

	fake := &fakeModelServingController{
		err: errors.New("index error"),
	}

	_, err := fake.calculateRoleScore(mi, "groupA", "worker", "1")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestCalculateServingGroupScoreSuccess(t *testing.T) {
	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
		},
	}

	key := "default/groupA"

	fake := &fakeModelServingController{
		pods: map[string][]*corev1.Pod{
			key: {
				{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							PodDeletionCostAnnotation: "4",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{
							PodDeletionCostAnnotation: "6",
						},
					},
				},
			},
		},
	}

	score, err := fake.calculateServingGroupScore(mi, "groupA")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score != 10 {
		t.Fatalf("expected 10, got %d", score)
	}
}
