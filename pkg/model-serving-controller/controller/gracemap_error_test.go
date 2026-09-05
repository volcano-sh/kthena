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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
)

// deletedGroupStore reproduces the window the bug needs: the ServingGroup still
// reads as Running, but the ModelServing has already been removed from the
// store by DeleteModelServing/DeleteServingGroup, so the status update fails.
type deletedGroupStore struct {
	datastore.Store
	updateCalls int
}

func (s *deletedGroupStore) GetServingGroupStatus(types.NamespacedName, string) datastore.ServingGroupStatus {
	return datastore.ServingGroupRunning
}

func (s *deletedGroupStore) UpdateServingGroupStatus(name types.NamespacedName, group string, _ datastore.ServingGroupStatus) error {
	s.updateCalls++
	return fmt.Errorf("failed to find modelServing %s/%s", name.Namespace, name.Name)
}

// A failed markPodUnavailable must not leave the pod stuck in graceMap.
// handlePodAfterGraceTime is the only thing that clears the key, and it is not
// started on the error path, so a leftover entry makes every later event for
// that pod return early at the LoadOrStore check and the pod is never
// recovered until the controller restarts.
func TestHandleErrorPodClearsGraceMapWhenMarkUnavailableFails(t *testing.T) {
	const (
		namespace = "default"
		podName   = "test-model-0-prefill-0-0"
		groupName = "test-model-0"
	)

	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      podName,
			UID:       types.UID("failed-pod"),
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "test-model"},
	}

	controller, _ := newGracePeriodTestController(t, failedPod)
	store := &deletedGroupStore{Store: datastore.New()}
	controller.store = store
	err := controller.handleErrorPod(ms, groupName, failedPod)
	require.Error(t, err, "handleErrorPod should surface the status update failure")

	_, stuck := controller.graceMap.Load(getPodGracePeriodKey(failedPod))
	assert.False(t, stuck,
		"graceMap still holds %s after the error, so the pod can never be recovered",
		getPodGracePeriodKey(failedPod))

	// The next event for the same pod must be able to retry rather than
	// returning early because a stale key is still present.
	err = controller.handleErrorPod(ms, groupName, failedPod)
	require.Error(t, err, "a later event should retry, not short-circuit on a stale graceMap key")
	assert.Equal(t, 2, store.updateCalls, "the retry should have reached the store again")
}
