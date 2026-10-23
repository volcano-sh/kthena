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

package inplace

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

type PatchOptions struct {
	Images map[string]string
	Labels map[string]string
	State  *State
}

type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// BuildPatch builds one JSON patch that changes images, labels, and the state together.
// Its test operations make the patch fail if the Pod changed after it was read.
func BuildPatch(pod *corev1.Pod, opts PatchOptions) ([]byte, error) {
	if opts.State == nil {
		return nil, fmt.Errorf("in-place update state is required")
	}
	stateValue, err := opts.State.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode in-place update state: %w", err)
	}

	var ops []jsonPatchOp
	if pod.UID != "" {
		ops = append(ops, jsonPatchOp{Op: "test", Path: "/metadata/uid", Value: string(pod.UID)})
	}
	if pod.ResourceVersion != "" {
		ops = append(ops, jsonPatchOp{Op: "test", Path: "/metadata/resourceVersion", Value: pod.ResourceVersion})
	}

	imageOps, err := imagePatchOps(pod, opts.Images)
	if err != nil {
		return nil, err
	}
	ops = append(ops, imageOps...)
	ops = appendMapEntries(ops, "/metadata/labels", pod.Labels, opts.Labels)
	ops = appendMapEntries(ops, "/metadata/annotations", pod.Annotations,
		map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: stateValue})
	return json.Marshal(ops)
}

func imagePatchOps(pod *corev1.Pod, images map[string]string) ([]jsonPatchOp, error) {
	changed := false
	found := make(map[string]bool, len(images))
	var ops []jsonPatchOp
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		image, ok := images[container.Name]
		if !ok {
			continue
		}
		found[container.Name] = true
		if container.Image == image {
			continue
		}
		changed = true
		path := fmt.Sprintf("/spec/containers/%d", i)
		ops = append(ops,
			jsonPatchOp{Op: "test", Path: path + "/name", Value: container.Name},
			jsonPatchOp{Op: "test", Path: path + "/image", Value: container.Image},
		)
		if container.ImagePullPolicy != "" {
			ops = append(ops, jsonPatchOp{Op: "test", Path: path + "/imagePullPolicy", Value: string(container.ImagePullPolicy)})
		}
		ops = append(ops, jsonPatchOp{Op: "replace", Path: path + "/image", Value: image})
	}
	for _, name := range sortedKeys(images) {
		if !found[name] {
			return nil, fmt.Errorf("pod %s/%s has no regular container %q", pod.Namespace, pod.Name, name)
		}
	}
	if !changed {
		return nil, nil
	}
	// Kubelet restarts a container on the patched image only under restartPolicy Always.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		return nil, fmt.Errorf("pod %s/%s has restartPolicy %q, in-place image update requires %q",
			pod.Namespace, pod.Name, pod.Spec.RestartPolicy, corev1.RestartPolicyAlways)
	}
	guard := jsonPatchOp{Op: "test", Path: "/spec/restartPolicy", Value: string(corev1.RestartPolicyAlways)}
	return append([]jsonPatchOp{guard}, ops...), nil
}

// appendMapEntries adds a missing map as a whole, because JSON patch cannot add a key under a missing parent.
func appendMapEntries(ops []jsonPatchOp, path string, current, entries map[string]string) []jsonPatchOp {
	if len(entries) == 0 {
		return ops
	}
	if current == nil {
		return append(ops, jsonPatchOp{Op: "add", Path: path, Value: entries})
	}
	for _, key := range sortedKeys(entries) {
		ops = append(ops, jsonPatchOp{Op: "add", Path: path + "/" + escapeJSONPointer(key), Value: entries[key]})
	}
	return ops
}

func escapeJSONPointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
