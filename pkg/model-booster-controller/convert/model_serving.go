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

package convert

import (
	"bytes"
	"crypto/md5"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/volcano-sh/kthena/pkg/model-booster-controller/env"
	icUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"k8s.io/utils/ptr"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/config"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	CacheURIPrefixPVC              = "pvc://"
	CacheURIPrefixHostPath         = "hostpath://"
	URIPrefixSeparator             = "://"
	VllmTemplatePath               = "templates/vllm.yaml"
	VllmDisaggregatedTemplatePath  = "templates/vllm-pd.yaml"
	VllmMultiNodeServingScriptPath = "/vllm-workspace/vllm/examples/online_serving/multi-node-serving.sh"
	modelRouteRuleName             = "default"
)

//go:embed templates/*
var templateFS embed.FS

// BuildModelServing creates a ModelServing object based on the model's backend.
func BuildModelServing(model *workload.ModelBooster) (*workload.ModelServing, error) {
	backend := &model.Spec.Backend
	var serving *workload.ModelServing
	var err error
	switch backend.Type {
	case workload.ModelBackendTypeVLLM:
		serving, err = buildVllmModelServing(model)
	case workload.ModelBackendTypeVLLMDisaggregated:
		serving, err = buildVllmDisaggregatedModelServing(model)
	default:
		return nil, fmt.Errorf("not support model backend type: %s", backend.Type)
	}
	if err != nil {
		return nil, err
	}
	return serving, nil
}

// buildVllmDisaggregatedModelServing handles VLLM disaggregated backend creation.
func buildVllmDisaggregatedModelServing(model *workload.ModelBooster) (*workload.ModelServing, error) {
	backend := &model.Spec.Backend
	workersMap := mapWorkers(backend.Workers)
	if workersMap[workload.ModelWorkerTypePrefill] == nil {
		return nil, fmt.Errorf("prefill worker not found in backend")
	}
	if workersMap[workload.ModelWorkerTypeDecode] == nil {
		return nil, fmt.Errorf("decode worker not found in backend")
	}
	cacheVolume, err := buildCacheVolume(backend)
	if err != nil {
		return nil, err
	}
	modelDownloadPath := GetCachePath(backend.CacheURI) + GetMountPath(backend.ModelURI)

	// Build an initial container list including model downloader container
	var envVars []corev1.EnvVar
	endpointEnvVars := env.GetEnvValueOrDefault[[]corev1.EnvVar](backend, env.Endpoint, []corev1.EnvVar{
		{Name: env.Endpoint},
	})
	if len(endpointEnvVars) > 0 && endpointEnvVars[0].Value != "" {
		envVars = append(envVars, endpointEnvVars[0])
	}
	hfEndpointEnvVars := env.GetEnvValueOrDefault[[]corev1.EnvVar](backend, env.HfEndpoint, []corev1.EnvVar{
		{Name: env.HfEndpoint},
	})
	if len(hfEndpointEnvVars) > 0 && hfEndpointEnvVars[0].Value != "" {
		envVars = append(envVars, hfEndpointEnvVars[0])
	}
	initContainers := []corev1.Container{
		{
			Name:  model.Name + "-model-downloader",
			Image: config.Config.DownloaderImage(),
			Args: []string{
				"--source", backend.ModelURI,
				"--output-dir", modelDownloadPath,
			},
			Env:     envVars,
			EnvFrom: backend.EnvFrom,
			VolumeMounts: []corev1.VolumeMount{{
				Name:      cacheVolume.Name,
				MountPath: GetCachePath(backend.CacheURI),
			}},
		},
	}

	var preFillCommand []string
	var decodeCommand []string
	for _, worker := range backend.Workers {
		if worker.Type == workload.ModelWorkerTypePrefill {
			preFillCommand, err = buildCommands(&worker.Config, modelDownloadPath, workersMap)
			if err != nil {
				return nil, err
			}
		} else if worker.Type == workload.ModelWorkerTypeDecode {
			decodeCommand, err = buildCommands(&worker.Config, modelDownloadPath, workersMap)
			if err != nil {
				return nil, err
			}
		}
	}

	prefillEngineEnv := buildEngineEnvVars(backend,
		corev1.EnvVar{Name: "HF_HUB_OFFLINE", Value: "1"},
		corev1.EnvVar{Name: "HCCL_IF_IP", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		}},
	)

	decodeEngineEnv := buildEngineEnvVars(backend,
		corev1.EnvVar{Name: "HF_HUB_OFFLINE", Value: "1"},
		corev1.EnvVar{Name: "GLOO_SOCKET_IFNAME", Value: "eth0"},
		corev1.EnvVar{Name: "TP_SOCKET_IFNAME", Value: "eth0"},
		corev1.EnvVar{Name: "HCCL_SOCKET_IFNAME", Value: "eth0"},
	)

	data := map[string]interface{}{
		"MODEL_SERVING_TEMPLATE_METADATA": &metav1.ObjectMeta{
			Name:      utils.GetBackendResourceName(model.Name, backend.Name),
			Namespace: model.Namespace,
			Labels:    utils.GetModelControllerLabels(model, backend.Name, icUtils.Revision(backend)),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workload.GroupVersion.String(),
					Kind:       workload.ModelKind.Kind,
					Name:       model.Name,
					UID:        model.UID,
				},
			},
		},
		"VOLUME_MOUNTS": []corev1.VolumeMount{{
			Name:      cacheVolume.Name,
			MountPath: GetCachePath(backend.CacheURI),
		}},
		"VOLUMES": []*corev1.Volume{
			cacheVolume,
		},
		"MODEL_NAME":             model.Name,
		"BACKEND_REPLICAS":       backend.MinReplicas, // todo: backend replicas
		"INIT_CONTAINERS":        initContainers,
		"MODEL_DOWNLOAD_ENVFROM": backend.EnvFrom,
		"ENGINE_PREFILL_COMMAND": preFillCommand,
		"ENGINE_DECODE_COMMAND":  decodeCommand,
		"SERVER_ENTRY_TEMPLATE_METADATA": &metav1.ObjectMeta{
			Labels: utils.GetModelControllerLabels(model, backend.Name, icUtils.Revision(backend)),
		},
		"MODEL_SERVING_RUNTIME_IMAGE":        config.Config.RuntimeImage(),
		"MODEL_SERVING_RUNTIME_PORT":         env.GetEnvValueOrDefault[int32](backend, env.RuntimePort, 8100),
		"MODEL_SERVING_RUNTIME_URL":          env.GetEnvValueOrDefault[string](backend, env.RuntimeUrl, "http://localhost:8000"),
		"MODEL_SERVING_RUNTIME_METRICS_PATH": env.GetEnvValueOrDefault[string](backend, env.RuntimeMetricsPath, "/metrics"),
		"ENGINE_PREFILL_ENV":                 prefillEngineEnv,
		"ENGINE_DECODE_ENV":                  decodeEngineEnv,
		"MODEL_SERVING_RUNTIME_ENGINE":       strings.ToLower(string(backend.Type)),
		"MODEL_SERVING_RUNTIME_POD":          "$(POD_NAME).$(NAMESPACE)",
		"PREFILL_REPLICAS":                   workersMap[workload.ModelWorkerTypePrefill].Replicas,
		"DECODE_REPLICAS":                    workersMap[workload.ModelWorkerTypeDecode].Replicas,
		"ENGINE_DECODE_RESOURCES":            workersMap[workload.ModelWorkerTypeDecode].Resources,
		"ENGINE_DECODE_IMAGE":                workersMap[workload.ModelWorkerTypeDecode].Image,
		"ENGINE_PREFILL_RESOURCES":           workersMap[workload.ModelWorkerTypePrefill].Resources,
		"ENGINE_PREFILL_IMAGE":               workersMap[workload.ModelWorkerTypePrefill].Image,
		"SCHEDULER_NAME":                     backend.SchedulerName,
	}
	return loadModelServingTemplate(VllmDisaggregatedTemplatePath, &data)
}

// buildVllmModelServing handles VLLM backend creation.
func buildVllmModelServing(model *workload.ModelBooster) (*workload.ModelServing, error) {
	backend := &model.Spec.Backend
	workersMap := mapWorkers(backend.Workers)
	if workersMap[workload.ModelWorkerTypeServer] == nil {
		return nil, fmt.Errorf("server worker not found in backend: %s", backend.Name)
	}
	cacheVolume, err := buildCacheVolume(backend)
	if err != nil {
		return nil, err
	}
	modelDownloadPath := GetCachePath(backend.CacheURI) + GetMountPath(backend.ModelURI)
	// only one worker in such circumstance so get the first worker's config as commands
	commands, err := buildCommands(&backend.Workers[0].Config, modelDownloadPath, workersMap)
	if err != nil {
		return nil, err
	}

	// Build an initial container list including model downloader container
	var envVars []corev1.EnvVar
	endpointEnvVars := env.GetEnvValueOrDefault[[]corev1.EnvVar](backend, env.Endpoint, []corev1.EnvVar{
		{Name: env.Endpoint},
	})
	if len(endpointEnvVars) > 0 && endpointEnvVars[0].Value != "" {
		envVars = append(envVars, endpointEnvVars[0])
	}
	hfEndpointEnvVars := env.GetEnvValueOrDefault[[]corev1.EnvVar](backend, env.HfEndpoint, []corev1.EnvVar{
		{Name: env.HfEndpoint},
	})
	if len(hfEndpointEnvVars) > 0 && hfEndpointEnvVars[0].Value != "" {
		envVars = append(envVars, hfEndpointEnvVars[0])
	}
	initContainers := []corev1.Container{
		{
			Name:  model.Name + "-model-downloader",
			Image: config.Config.DownloaderImage(),
			Args: []string{
				"--source", backend.ModelURI,
				"--output-dir", modelDownloadPath,
			},
			Env:     envVars,
			EnvFrom: backend.EnvFrom,
			VolumeMounts: []corev1.VolumeMount{{
				Name:      cacheVolume.Name,
				MountPath: GetCachePath(backend.CacheURI),
			}},
		},
	}

	engineEnv := buildEngineEnvVars(backend)
	data := map[string]interface{}{
		"MODEL_SERVING_TEMPLATE_METADATA": &metav1.ObjectMeta{
			Name:      utils.GetBackendResourceName(model.Name, backend.Name),
			Namespace: model.Namespace,
			Labels:    utils.GetModelControllerLabels(model, backend.Name, icUtils.Revision(backend)),
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workload.GroupVersion.String(),
					Kind:       workload.ModelKind.Kind,
					Name:       model.Name,
					UID:        model.UID,
				},
			},
		},
		"MODEL_NAME":       model.Name,
		"BACKEND_NAME":     strings.ToLower(backend.Name),
		"BACKEND_REPLICAS": backend.MinReplicas, // todo: backend replicas
		"BACKEND_TYPE":     strings.ToLower(string(backend.Type)),
		"ENGINE_ENV":       engineEnv,
		"WORKER_ENV":       backend.Env,
		"SERVER_REPLICAS":  workersMap[workload.ModelWorkerTypeServer].Replicas,
		"SERVER_ENTRY_TEMPLATE_METADATA": &metav1.ObjectMeta{
			Labels: utils.GetModelControllerLabels(model, backend.Name, icUtils.Revision(backend)),
		},
		"SERVER_WORKER_TEMPLATE_METADATA": nil,
		"VOLUMES": []*corev1.Volume{
			cacheVolume,
		},
		"VOLUME_MOUNTS": []corev1.VolumeMount{{
			Name:      cacheVolume.Name,
			MountPath: GetCachePath(backend.CacheURI),
		}},
		"INIT_CONTAINERS":                    initContainers,
		"MODEL_DOWNLOAD_ENVFROM":             backend.EnvFrom,
		"MODEL_SERVING_RUNTIME_IMAGE":        config.Config.RuntimeImage(),
		"MODEL_SERVING_RUNTIME_PORT":         env.GetEnvValueOrDefault[int32](backend, env.RuntimePort, 8100),
		"MODEL_SERVING_RUNTIME_URL":          env.GetEnvValueOrDefault[string](backend, env.RuntimeUrl, "http://localhost:8000"),
		"MODEL_SERVING_RUNTIME_METRICS_PATH": env.GetEnvValueOrDefault[string](backend, env.RuntimeMetricsPath, "/metrics"),
		"MODEL_SERVING_RUNTIME_ENGINE":       strings.ToLower(string(backend.Type)),
		"MODEL_SERVING_RUNTIME_POD":          "$(POD_NAME).$(NAMESPACE)",
		"ENGINE_SERVER_RESOURCES":            workersMap[workload.ModelWorkerTypeServer].Resources,
		"ENGINE_SERVER_IMAGE":                workersMap[workload.ModelWorkerTypeServer].Image,
		"ENGINE_SERVER_COMMAND":              commands,
		"WORKER_REPLICAS":                    workersMap[workload.ModelWorkerTypeServer].Pods - 1,
		"SCHEDULER_NAME":                     backend.SchedulerName,
	}
	return loadModelServingTemplate(VllmTemplatePath, &data)
}

// mapWorkers creates a map of workers by type.
func mapWorkers(workers []workload.ModelWorker) map[workload.ModelWorkerType]*workload.ModelWorker {
	workersMap := make(map[workload.ModelWorkerType]*workload.ModelWorker, len(workers))
	for _, worker := range workers {
		workersMap[worker.Type] = &worker
	}
	return workersMap
}

// buildCommands constructs the command list for the backend.
func buildCommands(workerConfig *apiextensionsv1.JSON, modelDownloadPath string,
	workersMap map[workload.ModelWorkerType]*workload.ModelWorker) ([]string, error) {
	commands := []string{"python3", "-m", "vllm.entrypoints.openai.api_server", "--model", modelDownloadPath}
	args, err := utils.ConvertVLLMArgsFromJson(workerConfig)
	commands = append(commands, args...)
	if workersMap[workload.ModelWorkerTypeServer] != nil && workersMap[workload.ModelWorkerTypeServer].Pods > 1 {
		commands = append(commands, "--distributed_executor_backend", "ray")
		commands = []string{"bash", "-c", fmt.Sprintf("chmod u+x %s && %s leader --ray_cluster_size=%d --num-gpus=%d && %s", VllmMultiNodeServingScriptPath, VllmMultiNodeServingScriptPath, workersMap[workload.ModelWorkerTypeServer].Pods, utils.GetDeviceNum(workersMap[workload.ModelWorkerTypeServer]), strings.Join(commands, " "))}
	}
	return commands, err
}

// GetMountPath returns the mount path for the given ModelBackend in the format "/<backend.Name>".
func GetMountPath(modelURI string) string {
	h := md5.New()
	h.Write([]byte(modelURI))
	hashBytes := h.Sum(nil)
	hashHex := hex.EncodeToString(hashBytes)
	return "/" + hashHex
}

func buildCacheVolume(backend *workload.ModelBackend) (*corev1.Volume, error) {
	volumeName := getVolumeName(backend.Name)
	switch {
	case backend.CacheURI == "":
		return &corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}, nil
	case strings.HasPrefix(backend.CacheURI, CacheURIPrefixPVC):
		return &corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: GetCachePath(backend.CacheURI),
				},
			},
		}, nil
	case strings.HasPrefix(backend.CacheURI, CacheURIPrefixHostPath):
		return &corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: GetCachePath(backend.CacheURI),
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		}, nil
	}
	return nil, fmt.Errorf("not support prefix in CacheURI: %s", backend.CacheURI)
}

// GetCachePath gets the path from string after "://". For example, for "pvc://my-pvc", it returns "/my-pvc".
func GetCachePath(path string) string {
	if path == "" || !strings.Contains(path, URIPrefixSeparator) {
		return ""
	}
	s := strings.Split(path, URIPrefixSeparator)[1]
	s = strings.Trim(s, "/")
	builder := strings.Builder{}
	builder.WriteString("/")
	builder.WriteString(s)
	return builder.String()
}

func getVolumeName(backendName string) string {
	return backendName + "-weights"
}

// loadModelServingTemplate loads and processes the template file.
func loadModelServingTemplate(templatePath string, data *map[string]interface{}) (*workload.ModelServing, error) {
	templateBytes, err := templateFS.ReadFile(templatePath)
	if err != nil {
		return nil, err
	}

	var jsonObj interface{}
	if err = yaml.Unmarshal(templateBytes, &jsonObj); err != nil {
		return nil, fmt.Errorf("YAML template parse failed: %w", err)
	}
	if err = utils.ReplacePlaceholders(&jsonObj, data); err != nil {
		return nil, fmt.Errorf("replace placeholders failed: %v", err)
	}

	replacedJsonBytes, err := json.Marshal(jsonObj)
	if err != nil {
		return nil, fmt.Errorf("JSON parse failed with replaced json bytes: %w", err)
	}

	modelServing := &workload.ModelServing{}
	reader := bytes.NewReader(replacedJsonBytes)
	decoder := yaml.NewYAMLOrJSONDecoder(reader, 1024)
	if err := decoder.Decode(modelServing); err != nil {
		return nil, fmt.Errorf("model serving parse json failed : %w", err)
	}

	return modelServing, nil
}

func buildEngineEnvVars(backend *workload.ModelBackend, additionalEnvs ...corev1.EnvVar) []corev1.EnvVar {
	standardEnvs := []corev1.EnvVar{
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
		{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
		{Name: "VLLM_USE_V1", Value: "1"},
		{
			Name: "REDIS_HOST",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "redis-config"},
					Key:                  "REDIS_HOST",
					Optional:             &[]bool{true}[0],
				},
			},
		},
		{
			Name: "REDIS_PORT",
			ValueFrom: &corev1.EnvVarSource{
				ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "redis-config"},
					Key:                  "REDIS_PORT",
					Optional:             &[]bool{true}[0],
				},
			},
		},
		{
			Name: "REDIS_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "redis-secret"},
					Key:                  "REDIS_PASSWORD",
					Optional:             &[]bool{true}[0],
				},
			},
		},
	}
	return append(append(append([]corev1.EnvVar(nil), backend.Env...), standardEnvs...), additionalEnvs...)
}
