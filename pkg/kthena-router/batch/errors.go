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

package batch

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

// Error types from the OpenAI error object.
const (
	errInvalidRequest = "invalid_request_error"
	errAPI            = "api_error"
)

// fail answers with the OpenAI error object; param and code are null when empty.
func fail(c *gin.Context, status int, kind, param, message string) {
	body := gin.H{"message": message, "type": kind, "param": nil, "code": nil}
	if param != "" {
		body["param"] = param
	}
	c.AbortWithStatusJSON(status, gin.H{"error": body})
}

// unavailable answers a failure of the store or the volume, which is never a 404, and logs its cause.
func unavailable(c *gin.Context, message string, cause error) {
	klog.Errorf("batch: %s: %v", message, cause)
	fail(c, http.StatusServiceUnavailable, errAPI, "", message)
}
