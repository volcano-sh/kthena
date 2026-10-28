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

package plugins

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestLWSLabelsPluginRuntimeCompatibility(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "LeaderWorkerSet", Name: "sample"},
			},
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{{Name: "default", WorkerReplicas: 2}},
			},
		},
	}
	plugin := &LWSLabelsPlugin{name: LWSLabelsPluginName}
	dynamoArgs := []string{
		"--dist-init-addr=$(LWS_LEADER_ADDRESS):29500",
		"--nnodes=$(LWS_GROUP_SIZE)",
		"--node-rank=$(LWS_WORKER_INDEX)",
	}

	pods := make([]*corev1.Pod, 0, 3)
	for workerIndex := 0; workerIndex < 3; workerIndex++ {
		podName := "sample-1-default-0-0"
		isEntry := workerIndex == 0
		if !isEntry {
			podName = fmt.Sprintf("sample-1-default-0-%d", workerIndex)
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "runtime", Args: dynamoArgs,
					Env: []corev1.EnvVar{
						{Name: workloadv1alpha1.EntryAddressEnv, Value: "existing-entry"},
						{Name: lwsv1.LwsWorkerIndex, Value: "user-value"},
					},
				}},
				InitContainers: []corev1.Container{{Name: "init"}},
			},
		}
		req := &HookRequest{
			ModelServing: ms,
			ServingGroup: "sample-1",
			RoleName:     "default",
			IsEntry:      isEntry,
			Pod:          pod,
		}
		require.NoError(t, plugin.OnPodCreate(context.Background(), req))
		pods = append(pods, pod)

		expectedHostname := "sample-1"
		if workerIndex > 0 {
			expectedHostname = fmt.Sprintf("sample-1-%d", workerIndex)
		}
		assert.Equal(t, expectedHostname, pod.Spec.Hostname)
		assert.Equal(t, "sample", pod.Spec.Subdomain)
		assert.Equal(t, dynamoArgs, pod.Spec.Containers[0].Args)
		assert.Equal(t, "sample", pod.Labels[lwsv1.SetNameLabelKey])
		assert.Equal(t, fmt.Sprint(workerIndex), pod.Labels[lwsv1.WorkerIndexLabelKey])
		assert.Equal(t, "3dd92d607354e0e4a553335b6aa440af56667905", pod.Labels[lwsv1.GroupUniqueHashLabelKey])
		assert.Equal(t, "3", pod.Annotations[lwsv1.SizeAnnotationKey])
		assertEnv(t, pod.Spec.Containers[0], lwsv1.LwsLeaderAddress, "sample-1.sample.default")
		assertEnv(t, pod.Spec.Containers[0], lwsv1.LwsGroupSize, "3")
		assertEnv(t, pod.Spec.Containers[0], lwsv1.LwsWorkerIndex, fmt.Sprint(workerIndex))
		assertEnv(t, pod.Spec.Containers[0], workloadv1alpha1.EntryAddressEnv, "existing-entry")
		assertEnv(t, pod.Spec.InitContainers[0], lwsv1.LwsLeaderAddress, "sample-1.sample.default")
	}

	leaderAddress := envValue(pods[0].Spec.Containers[0], lwsv1.LwsLeaderAddress)
	for workerIndex := 1; workerIndex < len(pods); workerIndex++ {
		// Dynamo TRT-LLM derives worker hosts by inserting the worker ordinal before
		// the first dot in LWS_LEADER_ADDRESS.
		derived := strings.Replace(leaderAddress, ".", fmt.Sprintf("-%d.", workerIndex), 1)
		actual := fmt.Sprintf("%s.%s.%s", pods[workerIndex].Spec.Hostname, pods[workerIndex].Spec.Subdomain, pods[workerIndex].Namespace)
		assert.Equal(t, actual, derived)
	}
}

func TestLWSLabelsPluginSkipsNonLWSModelServing(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime"}}}}
	req := &HookRequest{
		ModelServing: &workloadv1alpha1.ModelServing{},
		Pod:          pod,
	}
	require.NoError(t, (&LWSLabelsPlugin{name: LWSLabelsPluginName}).OnPodCreate(context.Background(), req))
	assert.Empty(t, pod.Spec.Hostname)
	assert.Empty(t, pod.Spec.Containers[0].Env)
}

func assertEnv(t *testing.T, container corev1.Container, name, value string) {
	t.Helper()
	assert.Equal(t, value, envValue(container, name), "environment variable %s", name)
}

func envValue(container corev1.Container, name string) string {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}
