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
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

const (
	CostAwarePluginName = "cost-aware"

	costAwareModeObserve = "observe"
	costAwareModeScore   = "score"
)

var _ framework.ScorePlugin = &CostAware{}
var _ framework.ReservationPlugin = &CostAware{}

type CostAwareArgs struct {
	Mode                 string  `yaml:"mode,omitempty"`
	PodBudgetTokens      int64   `yaml:"podBudgetTokens,omitempty"`
	RouterBudgetDivisor  int64   `yaml:"routerBudgetDivisor,omitempty"`
	InputWeight          float64 `yaml:"inputWeight,omitempty"`
	OutputWeight         float64 `yaml:"outputWeight,omitempty"`
	SafetyFactor         float64 `yaml:"safetyFactor,omitempty"`
	DefaultOutputTokens  int64   `yaml:"defaultOutputTokens,omitempty"`
	MinReservationTokens int64   `yaml:"minReservationTokens,omitempty"`
	MaxReservationTokens int64   `yaml:"maxReservationTokens,omitempty"`
	EMAAlpha             float64 `yaml:"emaAlpha,omitempty"`
	DefaultBytesPerToken float64 `yaml:"defaultBytesPerToken,omitempty"`
}

type podTokenPressure struct {
	reservedInflight int64
	reservations     map[string]int64
}

type CostAware struct {
	name string
	args CostAwareArgs

	mu            sync.Mutex
	podPressure   map[string]*podTokenPressure
	bytesPerToken map[string]float64
}

func NewCostAware(pluginArg runtime.RawExtension) *CostAware {
	args := defaultCostAwareArgs()
	if len(pluginArg.Raw) > 0 {
		if err := yaml.Unmarshal(pluginArg.Raw, &args); err != nil {
			klog.Errorf("Unmarshal CostAwareArgs error, using default values: %v", err)
			args = defaultCostAwareArgs()
		}
	}
	args = normalizeCostAwareArgs(args)

	return &CostAware{
		name:          CostAwarePluginName,
		args:          args,
		podPressure:   make(map[string]*podTokenPressure),
		bytesPerToken: make(map[string]float64),
	}
}

func defaultCostAwareArgs() CostAwareArgs {
	return CostAwareArgs{
		Mode:                 costAwareModeObserve,
		PodBudgetTokens:      262144,
		RouterBudgetDivisor:  1,
		InputWeight:          1,
		OutputWeight:         1,
		SafetyFactor:         1.2,
		DefaultOutputTokens:  1024,
		MinReservationTokens: 64,
		MaxReservationTokens: 32768,
		EMAAlpha:             0.2,
		DefaultBytesPerToken: 4,
	}
}

func normalizeCostAwareArgs(args CostAwareArgs) CostAwareArgs {
	defaults := defaultCostAwareArgs()
	if args.Mode == "" {
		args.Mode = defaults.Mode
	}
	if args.Mode != costAwareModeObserve && args.Mode != costAwareModeScore {
		klog.Warningf("Invalid cost-aware mode %q, using %q", args.Mode, defaults.Mode)
		args.Mode = defaults.Mode
	}
	if args.PodBudgetTokens <= 0 {
		args.PodBudgetTokens = defaults.PodBudgetTokens
	}
	if args.RouterBudgetDivisor <= 0 {
		args.RouterBudgetDivisor = defaults.RouterBudgetDivisor
	}
	if args.InputWeight < 0 {
		args.InputWeight = defaults.InputWeight
	}
	if args.OutputWeight < 0 {
		args.OutputWeight = defaults.OutputWeight
	}
	if args.InputWeight+args.OutputWeight == 0 {
		args.InputWeight = defaults.InputWeight
		args.OutputWeight = defaults.OutputWeight
	}
	if args.SafetyFactor < 1 {
		args.SafetyFactor = defaults.SafetyFactor
	}
	if args.DefaultOutputTokens < 0 {
		args.DefaultOutputTokens = defaults.DefaultOutputTokens
	}
	if args.MinReservationTokens <= 0 {
		args.MinReservationTokens = defaults.MinReservationTokens
	}
	if args.MaxReservationTokens < args.MinReservationTokens {
		args.MaxReservationTokens = args.MinReservationTokens
	}
	if args.EMAAlpha <= 0 || args.EMAAlpha > 1 {
		args.EMAAlpha = defaults.EMAAlpha
	}
	if args.DefaultBytesPerToken <= 0 {
		args.DefaultBytesPerToken = defaults.DefaultBytesPerToken
	}
	return args
}

func (c *CostAware) Name() string {
	return c.name
}

func (c *CostAware) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scoreResults := make(map[*datastore.PodInfo]int)
	if len(pods) == 0 {
		return scoreResults
	}

	estimate := c.ensureEstimate(ctx)
	if estimate == nil || c.args.Mode == costAwareModeObserve {
		for _, pod := range pods {
			scoreResults[pod] = 100
		}
		return scoreResults
	}

	effectiveBudget := c.effectiveBudget()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, pod := range pods {
		podKey := podKey(pod)
		pressure := c.getPodPressureLocked(podKey)
		projected := pressure.reservedInflight + estimate.ReservationTokens
		score := 100 * (1 - float64(projected)/float64(effectiveBudget))
		scoreResults[pod] = clampScore(score)
	}

	return scoreResults
}

func (c *CostAware) Reserve(ctx *framework.Context, pod *datastore.PodInfo) *framework.Reservation {
	if pod == nil || c.args.Mode != costAwareModeScore {
		return nil
	}
	estimate := c.ensureEstimate(ctx)
	if estimate == nil || estimate.ReservationTokens <= 0 {
		return nil
	}

	podKey := podKey(pod)
	reservationID := c.reservationID(ctx, podKey)

	c.mu.Lock()
	defer c.mu.Unlock()
	pressure := c.getPodPressureLocked(podKey)
	if _, exists := pressure.reservations[reservationID]; exists {
		return &framework.Reservation{
			PluginName:    c.name,
			PodKey:        podKey,
			ReservationID: reservationID,
			Tokens:        estimate.ReservationTokens,
		}
	}
	pressure.reservedInflight += estimate.ReservationTokens
	pressure.reservations[reservationID] = estimate.ReservationTokens

	return &framework.Reservation{
		PluginName:    c.name,
		PodKey:        podKey,
		ReservationID: reservationID,
		Tokens:        estimate.ReservationTokens,
	}
}

func (c *CostAware) Finish(ctx *framework.Context, reservation *framework.Reservation, usage *framework.TokenUsage) {
	if reservation != nil {
		c.mu.Lock()
		pressure := c.getPodPressureLocked(reservation.PodKey)
		if tokens, exists := pressure.reservations[reservation.ReservationID]; exists {
			pressure.reservedInflight -= tokens
			if pressure.reservedInflight < 0 {
				pressure.reservedInflight = 0
			}
			delete(pressure.reservations, reservation.ReservationID)
		}
		c.mu.Unlock()
	}

	c.updateBytesPerTokenEMA(ctx, usage)
}

func (c *CostAware) ensureEstimate(ctx *framework.Context) *framework.RequestCostEstimate {
	if ctx == nil {
		return nil
	}
	if ctx.RequestCost != nil {
		return ctx.RequestCost
	}

	prompt := utils.GetPromptString(ctx.Prompt)
	promptBytes := int64(len([]byte(prompt)))
	bytesPerToken := c.bytesPerTokenForModel(ctx.Model)
	promptTokens := int64(0)
	if promptBytes > 0 {
		promptTokens = int64(math.Ceil(float64(promptBytes) / bytesPerToken))
	}
	outputTokens := c.estimateOutputTokens(ctx.RequestBody)

	estimatedTokens := int64(math.Ceil(
		c.args.InputWeight*float64(promptTokens) +
			c.args.OutputWeight*float64(outputTokens),
	))
	reservationTokens := int64(math.Ceil(float64(estimatedTokens) * c.args.SafetyFactor))
	reservationTokens = clampInt64(reservationTokens, c.args.MinReservationTokens, c.args.MaxReservationTokens)

	ctx.RequestCost = &framework.RequestCostEstimate{
		Model:             ctx.Model,
		PromptBytes:       promptBytes,
		PromptTokens:      promptTokens,
		OutputTokens:      outputTokens,
		EstimatedTokens:   estimatedTokens,
		ReservationTokens: reservationTokens,
		BytesPerToken:     bytesPerToken,
	}
	return ctx.RequestCost
}

func (c *CostAware) estimateOutputTokens(requestBody map[string]interface{}) int64 {
	if requestBody == nil {
		return c.args.DefaultOutputTokens
	}

	for _, key := range []string{"max_completion_tokens", "max_tokens", "max_new_tokens", "max_output_tokens"} {
		if tokens, ok := numericInt64(requestBody[key]); ok && tokens > 0 {
			return tokens * estimateChoices(requestBody)
		}
	}
	return c.args.DefaultOutputTokens * estimateChoices(requestBody)
}

func estimateChoices(requestBody map[string]interface{}) int64 {
	if n, ok := numericInt64(requestBody["n"]); ok && n > 0 {
		return n
	}
	return 1
}

func (c *CostAware) bytesPerTokenForModel(model string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.bytesPerToken[model]; ok && value > 0 {
		return value
	}
	return c.args.DefaultBytesPerToken
}

func (c *CostAware) updateBytesPerTokenEMA(ctx *framework.Context, usage *framework.TokenUsage) {
	if ctx == nil || ctx.RequestCost == nil || usage == nil {
		return
	}
	if ctx.RequestCost.PromptBytes <= 0 || usage.PromptTokens <= 0 {
		return
	}
	observed := float64(ctx.RequestCost.PromptBytes) / float64(usage.PromptTokens)
	if observed <= 0 || math.IsNaN(observed) || math.IsInf(observed, 0) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists := c.bytesPerToken[ctx.Model]
	if !exists || current <= 0 {
		c.bytesPerToken[ctx.Model] = observed
		return
	}
	c.bytesPerToken[ctx.Model] = c.args.EMAAlpha*observed + (1-c.args.EMAAlpha)*current
}

func (c *CostAware) effectiveBudget() int64 {
	budget := c.args.PodBudgetTokens / c.args.RouterBudgetDivisor
	if budget <= 0 {
		return 1
	}
	return budget
}

func (c *CostAware) getPodPressureLocked(podKey string) *podTokenPressure {
	pressure, exists := c.podPressure[podKey]
	if !exists {
		pressure = &podTokenPressure{reservations: make(map[string]int64)}
		c.podPressure[podKey] = pressure
	}
	return pressure
}

func (c *CostAware) reservationID(ctx *framework.Context, podKey string) string {
	if ctx != nil && ctx.RequestID != "" {
		return fmt.Sprintf("%s/%s", ctx.RequestID, podKey)
	}
	return fmt.Sprintf("%s/%d", podKey, time.Now().UnixNano())
}

func podKey(pod *datastore.PodInfo) string {
	if pod == nil || pod.Pod == nil {
		return ""
	}
	return types.NamespacedName{Namespace: pod.Pod.Namespace, Name: pod.Pod.Name}.String()
}

func clampScore(score float64) int {
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return int(score)
}

func clampInt64(value, minValue, maxValue int64) int64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func numericInt64(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case float32:
		return int64(v), true
	case float64:
		return int64(v), true
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return i, true
		}
		f, err := v.Float64()
		return int64(f), err == nil
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}
