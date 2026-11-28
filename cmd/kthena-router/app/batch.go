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

package app

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/batch"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/router"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

const (
	// batchShutdownWait bounds how long the router waits for the batch worker; the chart's grace period leaves room for it.
	batchShutdownWait       = 15 * time.Second
	defaultBatchStoragePath = "/var/lib/kthena/batch"
	// batchFileExpiration is the proposal's default lifetime of a file uploaded without expires_after.
	batchFileExpiration = 30 * 24 * time.Hour
)

// batchOptions is read from the environment, which the chart sets from kthenaRouter.batch.
type batchOptions struct {
	enabled     bool
	storagePath string
	// maxFileBytes is 0 when unset, which leaves the limit to the batch package.
	maxFileBytes int64
}

func batchOptionsFromEnv() batchOptions {
	options := batchOptions{storagePath: defaultBatchStoragePath}
	if value := os.Getenv("BATCH_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			klog.Warningf("Invalid BATCH_ENABLED %q, the batch API stays off", value)
		}
		options.enabled = enabled
	}
	if value := os.Getenv("BATCH_STORAGE_PATH"); value != "" {
		options.storagePath = value
	}
	if value := os.Getenv("BATCH_MAX_FILE_BYTES"); value != "" {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit <= 0 {
			klog.Warningf("Invalid BATCH_MAX_FILE_BYTES %q, using the default", value)
		} else {
			options.maxFileBytes = limit
		}
	}
	return options
}

// startBatch makes the router serve the batch API and run batches when BATCH_ENABLED is set; the channel closes once both stop.
func startBatch(ctx context.Context, r *router.Router) <-chan struct{} {
	options := batchOptionsFromEnv()
	if !options.enabled {
		return nil
	}
	store := batch.NewRedisStore(utils.NewRedisClient(), "")
	service := batch.NewService(store, batch.NewFSStore(options.storagePath), batch.Config{
		MaxFileBytes:   options.maxFileBytes,
		FileExpiration: batchFileExpiration,
		Gateway:        gatewayOf,
		Dispatcher:     batch.NewHandlerDispatcher(newBatchPipeline(r)),
		Holder:         os.Getenv("POD_NAME"),
	})
	// The storage check runs in the background: requests answer 503 and no batch runs until it passes, and the listeners never wait for it.
	done := service.Start(ctx)
	r.SetAPIExtension(service)
	klog.Infof("Batch API enabled, files under %s", options.storagePath)
	return done
}

// gatewayOf reads the Gateway API listener that the listener chain matched for this request; empty outside Gateway API mode.
func gatewayOf(c *gin.Context) (string, string, int) {
	return c.GetString(router.GatewayKey), c.GetString(router.GatewayListenerNameKey), c.GetInt(router.GatewayListenerPortKey)
}

// newBatchPipeline is the chain a batch line takes, as a listener's but with the batch identity in place of authentication; no port serves it.
func newBatchPipeline(r *router.Router) http.Handler {
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(batchIdentity())
	engine.Use(AccessLogMiddleware(r))
	engine.Any("/*path", r.HandlerFunc())
	return engine.Handler()
}

// batchIdentity sets what authentication and the Gateway listener would set, from the identity only the worker can put in the Go context.
func batchIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity, ok := batch.IdentityFromContext(c.Request.Context())
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "only batch lines may use this pipeline"})
			return
		}
		if identity.Subject != "" {
			c.Set(common.UserIdKey, identity.Subject)
		}
		if origin := identity.Origin; origin.Gateway != "" {
			c.Set(router.GatewayKey, origin.Gateway)
			c.Set(router.GatewayListenerNameKey, origin.Listener)
			c.Set(router.GatewayListenerPortKey, origin.Port)
		}
		c.Next()
	}
}

// waitForBatch gives the batch worker a bounded time to hand its batches back after the listeners have drained.
func waitForBatch(done <-chan struct{}, limit time.Duration) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(limit):
		klog.Warningf("The batch worker did not stop within %v; a batch it was still running stays in_progress", limit)
	}
}
