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
	"crypto/sha256"
	"encoding/hex"

	"github.com/gin-gonic/gin"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filters/auth"
)

// anonymousTenant is the one tenant every caller shares when authentication is off.
const anonymousTenant = "anonymous"

// tenantOf derives the owner from the verified token; only a caller with no token is anonymous.
func tenantOf(c *gin.Context) string {
	subject := c.GetString(common.UserIdKey)
	if _, verified := c.Get(common.JWTTokenKey); subject == "" && !verified {
		return anonymousTenant
	}
	return tenantID(auth.ClaimFromContext(c, "iss"), subject)
}

// tenantID is safe as a Redis key part and a directory name whatever the claims hold.
func tenantID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(sum[:16])
}
