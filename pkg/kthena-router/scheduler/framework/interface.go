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
	// Model is the name the client sent, which selects the ModelRoute.
	Model  string
	Prompt *common.ChatMessage

	// SessionID is the session identifier extracted from the HTTP header configured
	// via the SESSION_BOOST_HEADER environment variable.
	SessionID string

	Hashes []uint64

	// ModelServer information for efficient PDGroup scheduling
	ModelServerName types.NamespacedName
	// UpstreamModel is the name the engine serves. It can differ from Model when
	// ModelServer.spec.model is set.
	UpstreamModel string
	PDGroup       *aiv1alpha1.PDGroup
	// 1. In PD Disaggregated mode, both DecodePods and PrefillPods are set.
	DecodePods  []*datastore.PodInfo
	PrefillPods []*datastore.PodInfo

	// 2. PD aggregated mode, BestPods is selected for inference.
	BestPods []*datastore.PodInfo

	// StickyPodName pins aggregated scheduling or the PD Decode Pod if it survives filters.
	StickyPodName string
	// StickyPrefillPodName completes the PD pair identified by StickyPodName.
	StickyPrefillPodName string

	// MetricsRecorder for recording scheduler plugin metrics
	MetricsRecorder *metrics.RequestMetricsRecorder

	// ScheduleChain is the plugin chain this scheduling round uses; set by
	// the router before Schedule, read back by RunPostHooks. Nil means the
	// global chain.
	ScheduleChain ScheduleChain
}

// WeightedScorePlugin pairs a score plugin with the weight its chain
// configuration assigns it for aggregation.
type WeightedScorePlugin struct {
	Plugin ScorePlugin
	Weight int
}

// ScheduleChain is the request-scoped handle to the plugin chain in use.
type ScheduleChain interface {
	// NeedsOnFlightSync reports whether on-flight request counts must be
	// synced from Redis before this chain's scoring round.
	NeedsOnFlightSync() bool
	// Filters returns the chain's filter plugins.
	Filters() []FilterPlugin
	// Scores returns the chain's score plugins with their weights.
	Scores() []WeightedScorePlugin
	// PostScheduleHooks returns the chain's post-schedule hooks.
	PostScheduleHooks() []PostScheduleHook
}

type ScorePlugin interface {
	Name() string
	// Score is a method that is used to rank pods that have passed the filter plugins.
	// Note each plugin should generate score for a pod within [0, 100]
	Score(ctx *Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int
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

// PluginCloser is implemented by plugins that hold per-chain resources
// (background workers, store callbacks). The ProfileChainManager closes the plugins
// of replaced or deleted chains. Close must be idempotent.
type PluginCloser interface {
	Close() error
}
