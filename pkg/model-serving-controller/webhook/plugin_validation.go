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

package webhook

import (
	"bytes"

	"k8s.io/apimachinery/pkg/util/validation/field"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// validatePluginsImmutable protects existing Pods and auxiliary resources from
// changes that the controller cannot restore from their historical revisions.
func validatePluginsImmutable(oldModelServing, modelServing *workloadv1alpha1.ModelServing) field.ErrorList {
	pluginsPath := field.NewPath("spec", "plugins")
	// Project only plugins so scaling and template changes remain independent.
	// BuildRevisionData deep-copies and normalizes defaults and JSON object keys,
	// while preserving plugin execution order and arrays inside opaque config.
	canonicalPlugins := func(ms *workloadv1alpha1.ModelServing) ([]byte, error) {
		return utils.BuildRevisionData(&workloadv1alpha1.ModelServing{
			Spec: workloadv1alpha1.ModelServingSpec{Plugins: ms.Spec.Plugins},
		})
	}
	oldPlugins, err := canonicalPlugins(oldModelServing)
	if err != nil {
		return field.ErrorList{field.Forbidden(pluginsPath, "cannot compare existing plugin configuration")}
	}
	newPlugins, err := canonicalPlugins(modelServing)
	if err != nil {
		return field.ErrorList{field.Forbidden(pluginsPath, "cannot compare requested plugin configuration")}
	}
	if !bytes.Equal(oldPlugins, newPlugins) {
		return field.ErrorList{field.Forbidden(pluginsPath, "field is immutable after creation; create a new ModelServing to use a different plugin configuration")}
	}
	return nil
}
