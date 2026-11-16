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
	"os"
	"strconv"
	"time"

	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/batch"
	"github.com/volcano-sh/kthena/pkg/kthena-router/router"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

const (
	defaultBatchStoragePath = "/var/lib/kthena/batch"
	// batchStartupCheckTimeout bounds the Redis check made when the batch API starts.
	batchStartupCheckTimeout = 2 * time.Second
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

// startBatch makes the router serve the batch API when BATCH_ENABLED is set.
func startBatch(ctx context.Context, r *router.Router) {
	options := batchOptionsFromEnv()
	if !options.enabled {
		return
	}
	files := batch.NewFSStore(options.storagePath)
	client := utils.NewRedisClient()
	// A dependency that is down now is only logged: requests answer 503 until it is back.
	if err := files.Probe(ctx); err != nil {
		klog.Errorf("Batch API: %v", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, batchStartupCheckTimeout)
	defer cancel()
	if err := client.Ping(checkCtx).Err(); err != nil {
		klog.Errorf("Batch API: Redis is not reachable: %v", err)
	}
	r.SetAPIExtension(batch.NewService(batch.NewRedisStore(client, ""), files, batch.Config{MaxFileBytes: options.maxFileBytes}))
	klog.Infof("Batch API enabled, files under %s", options.storagePath)
}
