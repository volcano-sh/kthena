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
	"slices"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

const ModelServerLeastRequestPluginName = "modelserver-least-request"

// ModelServerLeastRequest is a filter that enforces the per-ModelServer
// least-request threshold on the ModelServer targeted by the request
// (trafficPolicy.loadBalancer.leastRequest.maxWaitingRequests). It
// complements the global least-request filter, and the two are independent:
// this threshold applies whenever it is configured, regardless of whether
// the global filter is enabled. When the field is unset or the ModelServer
// is unknown, the filter is a no-op.
var _ framework.FilterPlugin = &ModelServerLeastRequest{}

type ModelServerLeastRequest struct {
	name  string
	store datastore.Store
}

func NewModelServerLeastRequest(store datastore.Store) *ModelServerLeastRequest {
	return &ModelServerLeastRequest{
		name:  ModelServerLeastRequestPluginName,
		store: store,
	}
}

func (l *ModelServerLeastRequest) Name() string {
	return l.name
}

func (l *ModelServerLeastRequest) Filter(ctx *framework.Context, pods []*datastore.PodInfo) []*datastore.PodInfo {
	if l.store == nil || ctx == nil {
		return pods
	}
	modelServer := l.store.GetModelServer(ctx.ModelServerName)
	if modelServer == nil {
		return pods
	}
	tp := modelServer.Spec.TrafficPolicy
	if tp == nil || tp.LoadBalancer == nil || tp.LoadBalancer.LeastRequest == nil ||
		tp.LoadBalancer.LeastRequest.MaxWaitingRequests == nil {
		// Field unset for this ModelServer: the filter is a no-op.
		return pods
	}
	threshold := float64(*tp.LoadBalancer.LeastRequest.MaxWaitingRequests)
	return slices.DeleteFunc(pods, func(info *datastore.PodInfo) bool {
		return info.GetRequestWaitingNum() >= threshold
	})
}
