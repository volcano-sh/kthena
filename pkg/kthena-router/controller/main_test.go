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

import (
	"os"
	"testing"
)

// client-go v0.35 enables WatchListClient by default, but the gateway-api v1.4.0
// fake clientset neither supports WatchList nor implements the
// IsWatchListSemanticsUnSupported fallback marker, so its informers never sync.
// Use list and watch only in tests. Remove this once gateway-api ships a fake
// clientset with the fallback marker.
func TestMain(m *testing.M) {
	if err := os.Setenv("KUBE_FEATURE_WatchListClient", "false"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
