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
	"k8s.io/client-go/util/workqueue"
)

// retryOrForget decides what happens to key after syncHandler returns. A failure
// is requeued with backoff until maxRetries, then dropped. Success clears the
// key's failure count so a later failure starts its retries from zero. Each
// controller passes its own logf so it keeps the verbosity it logged at.
func retryOrForget(queue workqueue.TypedRateLimitingInterface[any], resource, key string, err error, logf func(format string, args ...any)) {
	if err == nil {
		queue.Forget(key)
		return
	}
	if queue.NumRequeues(key) < maxRetries {
		logf("error syncing %s %q: %v, requeuing", resource, key, err)
		queue.AddRateLimited(key)
		return
	}
	logf("giving up on syncing %s %q after %d retries: %v", resource, key, maxRetries, err)
	queue.Forget(key)
}

// ResourceType identifies the informer resource represented by a QueueItem.
type ResourceType string

const (
	ResourceTypeModelServer           ResourceType = "ModelServer"
	ResourceTypePod                   ResourceType = "Pod"
	ResourceTypeSecret                ResourceType = "Secret"
	ResourceTypeExternalModelProvider ResourceType = "ExternalModelProvider"
)

// QueueItem is the typed workqueue key shared by controllers that reconcile
// more than one Kubernetes resource type.
type QueueItem struct {
	ResourceType ResourceType
	Key          string
}
