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

package connectors

import (
	"github.com/gin-gonic/gin"
)

// KVConnector is the main interface for KV cache operations
type KVConnector interface {
	// Name returns the connector type name
	Name() string

	// Proxy executes the complete prefill-decode flow with KV cache coordination.
	// hooks carries optional on-flight counter callbacks for the prefill and decode
	// pods; pass nil when not needed.
	// Returns the number of output tokens consumed, or error if the operation fails.
	Proxy(c *gin.Context, reqBody map[string]interface{}, prefillAddr, decodeAddr string, hooks *OnFlightHooks) (int, error)
}
