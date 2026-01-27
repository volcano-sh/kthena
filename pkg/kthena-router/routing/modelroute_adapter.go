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

package routing

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// ModelRouteAdapter adapts a ModelRoute to the Route interface
type ModelRouteAdapter struct {
	modelRoute *aiv1alpha1.ModelRoute
	store      Store
}

// NewModelRouteAdapter creates a new ModelRouteAdapter
func NewModelRouteAdapter(modelRoute *aiv1alpha1.ModelRoute, store Store) *ModelRouteAdapter {
	return &ModelRouteAdapter{
		modelRoute: modelRoute,
		store:      store,
	}
}

// GetName returns the route name
func (a *ModelRouteAdapter) GetName() string {
	return a.modelRoute.Name
}

// GetNamespace returns the route namespace
func (a *ModelRouteAdapter) GetNamespace() string {
	return a.modelRoute.Namespace
}

// GetKey returns the namespaced name as a string key
func (a *ModelRouteAdapter) GetKey() string {
	return fmt.Sprintf("%s/%s", a.modelRoute.Namespace, a.modelRoute.Name)
}

// Matches checks if this ModelRoute matches the given request
func (a *ModelRouteAdapter) Matches(request *RouteRequest) (MatchResult, error) {
	// Check if the model name matches (either as model or LoRA adapter)
	var isLora bool
	var matchedAsModel bool
	var matchedAsLora bool

	if a.modelRoute.Spec.ModelName != "" && a.modelRoute.Spec.ModelName == request.ModelName {
		matchedAsModel = true
	}

	for _, lora := range a.modelRoute.Spec.LoraAdapters {
		if lora == request.ModelName {
			matchedAsLora = true
			isLora = true
			break
		}
	}

	if !matchedAsModel && !matchedAsLora {
		return MatchResult{Matched: false}, nil
	}

	// Check parentRefs against gateway
	if len(a.modelRoute.Spec.ParentRefs) > 0 {
		if request.GatewayKey == "" {
			// ModelRoute has parentRefs but request doesn't specify gateway
			return MatchResult{Matched: false}, nil
		}

		if !a.matchesGateway(request.GatewayKey) {
			return MatchResult{Matched: false}, nil
		}
	} else {
		// ModelRoute without parentRefs should not match when gatewayKey is specified
		if request.GatewayKey != "" {
			return MatchResult{Matched: false}, nil
		}
	}

	// Try to match rules and select a target
	for _, rule := range a.modelRoute.Spec.Rules {
		if matched, target, err := a.matchRule(rule, request); matched {
			if err != nil {
				return MatchResult{Matched: false}, err
			}
			return MatchResult{
				Matched:       true,
				Target:        target,
				IsLora:        isLora,
				OriginalRoute: a.modelRoute,
			}, nil
		}
	}

	return MatchResult{Matched: false}, nil
}

// matchesGateway checks if the ModelRoute matches the specified gateway
func (a *ModelRouteAdapter) matchesGateway(gatewayKey string) bool {
	gateway := a.store.GetGateway(gatewayKey)
	if gateway == nil {
		return false
	}

	for _, parentRef := range a.modelRoute.Spec.ParentRefs {
		// Get namespace from parentRef, default to ModelRoute's namespace
		namespace := a.modelRoute.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		// Get name from parentRef
		name := string(parentRef.Name)
		key := fmt.Sprintf("%s/%s", namespace, name)

		// Check if this parentRef matches the specified gateway
		if key == gatewayKey {
			// If sectionName is specified, check if the listener exists
			if parentRef.SectionName != nil {
				sectionName := string(*parentRef.SectionName)
				for _, listener := range gateway.Spec.Listeners {
					if string(listener.Name) == sectionName {
						return true
					}
				}
			} else {
				// No sectionName specified, match any listener
				return true
			}
		}
	}

	return false
}

// matchRule attempts to match a rule against the request
func (a *ModelRouteAdapter) matchRule(rule *aiv1alpha1.Rule, request *RouteRequest) (bool, RouteTarget, error) {
	// Check ModelMatch conditions
	if rule.ModelMatch != nil {
		// Check model name in body
		if rule.ModelMatch.Body != nil && rule.ModelMatch.Body.Model != nil {
			if request.ModelName != *rule.ModelMatch.Body.Model {
				return false, nil, nil
			}
		}

		// Check headers
		for key, stringMatch := range rule.ModelMatch.Headers {
			reqValue := request.HTTPRequest.Header.Get(key)
			if !matchStringMatch(stringMatch, reqValue) {
				return false, nil, nil
			}
		}

		// Check URI
		if rule.ModelMatch.Uri != nil {
			if !matchStringMatch(rule.ModelMatch.Uri, request.HTTPRequest.URL.Path) {
				return false, nil, nil
			}
		}
	}

	// Rule matched, select a target
	target, err := a.selectTarget(rule.TargetModels)
	if err != nil {
		return false, nil, err
	}

	return true, target, nil
}

// selectTarget selects a target from the list based on weights
func (a *ModelRouteAdapter) selectTarget(targets []*aiv1alpha1.TargetModel) (RouteTarget, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no targets available")
	}

	// Use the same weighted selection logic as the original implementation
	weights, err := a.toWeightedSlice(targets)
	if err != nil {
		return nil, err
	}

	index := selectFromWeights(weights)
	targetModel := targets[index]

	// Create ModelServerTarget
	modelServerName := types.NamespacedName{
		Namespace: a.modelRoute.Namespace,
		Name:      targetModel.ModelServerName,
	}

	return NewModelServerTarget(modelServerName, a.store), nil
}

// toWeightedSlice converts targets to a slice of weights
func (a *ModelRouteAdapter) toWeightedSlice(targets []*aiv1alpha1.TargetModel) ([]uint32, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("empty targets list")
	}

	var isWeighted bool
	if targets[0].Weight != nil {
		isWeighted = true
	}

	res := make([]uint32, len(targets))
	for i, target := range targets {
		if (isWeighted && target.Weight == nil) || (!isWeighted && target.Weight != nil) {
			return nil, fmt.Errorf("weight field must be either fully specified or not specified")
		}

		if isWeighted {
			res[i] = *target.Weight
		} else {
			res[i] = 1
		}
	}

	return res, nil
}

// GetRateLimit returns the rate limit configuration
func (a *ModelRouteAdapter) GetRateLimit() *RateLimitConfig {
	if a.modelRoute.Spec.RateLimit == nil {
		return nil
	}

	rl := a.modelRoute.Spec.RateLimit
	config := &RateLimitConfig{
		InputTokensPerUnit:  rl.InputTokensPerUnit,
		OutputTokensPerUnit: rl.OutputTokensPerUnit,
		Unit:                string(rl.Unit),
	}

	if rl.Global != nil && rl.Global.Redis != nil {
		config.Global = &GlobalRateLimitConfig{
			Redis: &RedisConfig{
				Address: rl.Global.Redis.Address,
			},
		}
	}

	return config
}

// GetParentRefs returns the parent gateway references
func (a *ModelRouteAdapter) GetParentRefs() []ParentReference {
	refs := make([]ParentReference, 0, len(a.modelRoute.Spec.ParentRefs))
	for _, ref := range a.modelRoute.Spec.ParentRefs {
		namespace := a.modelRoute.Namespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}

		sectionName := ""
		if ref.SectionName != nil {
			sectionName = string(*ref.SectionName)
		}

		refs = append(refs, ParentReference{
			Name:        string(ref.Name),
			Namespace:   namespace,
			SectionName: sectionName,
		})
	}
	return refs
}

// matchStringMatch matches a string value against a StringMatch specification
func matchStringMatch(sm *aiv1alpha1.StringMatch, value string) bool {
	switch {
	case sm.Exact != nil:
		return value == *sm.Exact
	case sm.Prefix != nil:
		return strings.HasPrefix(value, *sm.Prefix)
	case sm.Regex != nil:
		matched, _ := regexp.MatchString(*sm.Regex, value)
		return matched
	default:
		return true
	}
}

// ModelServerTarget implements RouteTarget for ModelServer
type ModelServerTarget struct {
	name  types.NamespacedName
	store Store
}

// NewModelServerTarget creates a new ModelServerTarget
func NewModelServerTarget(name types.NamespacedName, store Store) *ModelServerTarget {
	return &ModelServerTarget{
		name:  name,
		store: store,
	}
}

// GetPods returns the pods for this ModelServer
func (t *ModelServerTarget) GetPods() ([]*datastore.PodInfo, error) {
	return t.store.GetPodsByModelServer(t.name)
}

// GetPort returns the port for this ModelServer
func (t *ModelServerTarget) GetPort() int32 {
	ms := t.store.GetModelServer(t.name)
	if ms == nil {
		return 0
	}
	return ms.Spec.WorkloadPort.Port
}

// GetTargetType returns the target type
func (t *ModelServerTarget) GetTargetType() TargetType {
	return TargetTypeModelServer
}

// GetNamespacedName returns the namespaced name
func (t *ModelServerTarget) GetNamespacedName() types.NamespacedName {
	return t.name
}

// GetModelServer returns the underlying ModelServer
func (t *ModelServerTarget) GetModelServer() *aiv1alpha1.ModelServer {
	return t.store.GetModelServer(t.name)
}
