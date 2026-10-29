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

package controller

import "time"

type Config struct {
	EnableLeaderElection         bool
	Workers                      int
	Kubeconfig                   string
	MasterURL                    string
	Controllers                  map[string]bool
	KubeAPIQPS                   float32
	KubeAPIBurst                 int
	DebugPort                    int
	AutoscalingSyncPeriodSeconds int
	// DrainTimeout is the max wait for in-flight requests to drain before
	// deleting a pod during lossless upgrade. 0 disables draining (delete
	// immediately). Default 0 (disabled); set >0 to enable lossless drain.
	DrainTimeout time.Duration
}
