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

package vllm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
)

type Model struct {
	ID string `json:"id"`
}

type ModelList struct {
	Data []Model `json:"data"`
}

func (engine *vllmEngine) GetPodModels(pod *corev1.Pod) ([]string, error) {
	url := fmt.Sprintf("http://%s:%d/v1/models", pod.Status.PodIP, engine.MetricPort)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get models from pod %s/%s: HTTP %d", pod.GetNamespace(), pod.GetName(), resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var modelList ModelList
	err = json.Unmarshal(body, &modelList)
	if err != nil {
		return nil, err
	}

	models := make([]string, 0, len(modelList.Data))
	for _, model := range modelList.Data {
		models = append(models, model.ID)
	}
	return models, nil
}
