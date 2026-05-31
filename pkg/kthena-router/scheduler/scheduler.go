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

// Model server scheduling
package scheduler

import (
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

type Scheduler interface {
	Schedule(ctx *framework.Context, pods []*datastore.PodInfo) error
	Reserve(ctx *framework.Context, pod *datastore.PodInfo) []*framework.Reservation
	Finish(ctx *framework.Context, reservations []*framework.Reservation, usage *framework.TokenUsage)
	RunPostHooks(ctx *framework.Context, index int)
}
