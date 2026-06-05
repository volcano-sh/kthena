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

package framework

import (
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
)

// Context stores information which maybe useful in Filter or Score plugins.
type Context struct {
	Model  string
	Prompt *common.ChatMessage
	// RequestBody is the parsed OpenAI-compatible request. Scheduler plugins may
	// inspect generation caps such as max_tokens before routing.
	RequestBody map[string]interface{}
	RequestID   string
	RequestCost *RequestCostEstimate

	Hashes []uint64

	// ModelServer information for efficient PDGroup scheduling
	ModelServerName types.NamespacedName
	PDGroup         *aiv1alpha1.PDGroup
	// 1. In PD Disaggregated mode, both DecodePods and PrefillPods are set.
	DecodePods  []*datastore.PodInfo
	PrefillPods []*datastore.PodInfo

	// 2. PD aggregated mode, BestPods is selected for inference.
	BestPods []*datastore.PodInfo

	// MetricsRecorder for recording scheduler plugin metrics
	MetricsRecorder *metrics.RequestMetricsRecorder
}

type RequestCostEstimate struct {
	Model             string
	PromptBytes       int64
	PromptTokens      int64
	OutputTokens      int64
	EstimatedTokens   int64
	ReservationTokens int64
	BytesPerToken     float64
}

type TokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

type Reservation struct {
	PluginName    string
	PodKey        string
	ReservationID string
	Tokens        int64
}

type ScorePlugin interface {
	Name() string
	// Score is a method that is used to rank pods that have passed the filter plugins.
	// Note each plugin should generate score for a pod within [0, 100]
	Score(ctx *Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int
}

type ReservationPlugin interface {
	Reserve(ctx *Context, pod *datastore.PodInfo) *Reservation
	Finish(ctx *Context, reservation *Reservation, usage *TokenUsage)
}

type FilterPlugin interface {
	Name() string
	// Filter is a method that is used to filter valid pods that can be sent request to.
	Filter(ctx *Context, pods []*datastore.PodInfo) []*datastore.PodInfo
}

// PostHook is an interface that is executed after the scheduling is complete.
type PostScheduleHook interface {
	Name() string
	PostSchedule(ctx *Context, index int)
}
