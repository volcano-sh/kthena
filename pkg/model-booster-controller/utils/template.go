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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	ModelNameLabelKey   = workload.GroupName + "/model-name"
	BackendNameLabelKey = workload.GroupName + "/backend-name"
	ManageBy            = workload.GroupName + "/managed-by"
	RevisionLabelKey    = workload.GroupName + "/revision"
	OwnerUIDKey         = workload.GroupName + "/model-uid"
)

func ReplaceEmbeddedPlaceholders(s string, values *map[string]interface{}) (string, error) {
	var result strings.Builder
	pos := 0

	for {
		start := strings.Index(s[pos:], "${")
		if start == -1 {
			result.WriteString(s[pos:])
			break
		}
		start += pos

		end := strings.Index(s[start:], "}")
		if end == -1 {
			return "", fmt.Errorf("not found end } in: %s", s[start:])
		}
		end += start

		result.WriteString(s[pos:start])

		key := s[start+2 : end]

		if val, exists := (*values)[key]; exists {
			switch v := val.(type) {
			case string:
				result.WriteString(v)
			case int, int32, int64, float32, float64:
				result.WriteString(fmt.Sprintf("%v", v))
			case bool:
				result.WriteString(strconv.FormatBool(v))
			default:
				jsonBytes, err := json.Marshal(val)
				if err != nil {
					return "", fmt.Errorf("failed to marshal value to JSON: %w", err)
				}
				result.WriteString(string(jsonBytes))
			}
		} else {
			return "", fmt.Errorf("key not found: %s", key)
		}

		pos = end + 1
	}

	return result.String(), nil
}

// ConvertEngineArgsFromJson flattens a JSON object into `--foo-bar VALUE` flags,
// sorted by key. Shared by vLLM and SGLang.
func ConvertEngineArgsFromJson(config *apiextensionsv1.JSON) ([]string, error) {
	if config == nil || config.Raw == nil {
		return []string{}, nil
	}
	var configMap map[string]interface{}
	if err := json.Unmarshal(config.Raw, &configMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	keys := make([]string, 0, len(configMap))
	for k := range configMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, len(configMap)*2)
	for _, key := range keys {
		value := configMap[key]

		keyStr := fmt.Sprintf("--%s", strings.ReplaceAll(key, "_", "-"))

		var strValue string
		switch v := value.(type) {
		case string:
			strValue = v
		case bool:
			strValue = fmt.Sprintf("%t", v)
		case json.Number:
			strValue = value.(json.Number).String()
		default:
			strValue = fmt.Sprintf("%v", v)
		}
		args = append(args, keyStr)
		if strValue != "" {
			args = append(args, strValue)
		}
	}

	return args, nil
}

func deepCopyValue(src interface{}) interface{} {
	if src == nil {
		return nil
	}

	switch src.(type) {
	case string, bool, int, int32, int64, float32, float64:
		return src
	}

	bytes, err := json.Marshal(src)
	if err != nil {
		return src
	}

	var dest interface{}
	if err := json.Unmarshal(bytes, &dest); err != nil {
		return src
	}

	return dest
}

func ReplacePlaceholders(data *interface{}, values *map[string]interface{}) error {
	switch v := (*data).(type) {
	case map[string]interface{}:
		for key, val := range v {
			if err := ReplacePlaceholders(&val, values); err != nil {
				return err
			}
			v[key] = val
		}
	case []interface{}:
		for i := range v {
			if err := ReplacePlaceholders(&v[i], values); err != nil {
				return err
			}
		}
	case string:
		if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
			key := strings.TrimSuffix(strings.TrimPrefix(v, "${"), "}")
			if val, exists := (*values)[key]; exists {
				*data = deepCopyValue(val)
				return ReplacePlaceholders(data, values)
			}
			return fmt.Errorf("not found placeholder: %s", key)
		} else if strings.Contains(v, "${") {
			newStr, err := ReplaceEmbeddedPlaceholders(v, values)
			if err != nil {
				return err
			}
			*data = newStr
		}
	}
	return nil
}

func GetBackendResourceName(modelName string, backendName string) string {
	if backendName == "" {
		return modelName
	}
	return fmt.Sprintf("%s-%s", modelName, backendName)
}

func GetModelControllerLabels(model *workload.ModelBooster, backendName string, revision string) map[string]string {
	return map[string]string{
		ModelNameLabelKey:   model.Name,
		BackendNameLabelKey: backendName,
		ManageBy:            workload.GroupName,
		RevisionLabelKey:    revision,
		OwnerUIDKey:         string(model.UID),
	}
}
