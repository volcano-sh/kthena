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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func TestRetryOrForget(t *testing.T) {
	const key = "default/example"
	syncErr := errors.New("sync failed")

	t.Run("a failure is requeued", func(t *testing.T) {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
		defer queue.ShutDown()

		retryOrForget(queue, "gateway", key, syncErr, klog.Errorf)
		assert.Equal(t, 1, queue.NumRequeues(key))
	})

	t.Run("a success clears earlier failures", func(t *testing.T) {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
		defer queue.ShutDown()

		for i := 0; i < maxRetries-1; i++ {
			retryOrForget(queue, "gateway", key, syncErr, klog.Errorf)
		}
		retryOrForget(queue, "gateway", key, nil, klog.Errorf)
		assert.Equal(t, 0, queue.NumRequeues(key))
	})

	t.Run("failures spread across successes never exhaust the retries", func(t *testing.T) {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
		defer queue.ShutDown()

		for i := 0; i < maxRetries*2; i++ {
			retryOrForget(queue, "gateway", key, syncErr, klog.Errorf)
			retryOrForget(queue, "gateway", key, nil, klog.Errorf)
			assert.Equal(t, 0, queue.NumRequeues(key))
		}
	})

	t.Run("the key is dropped once the retries run out", func(t *testing.T) {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
		defer queue.ShutDown()

		for i := 0; i < maxRetries; i++ {
			retryOrForget(queue, "gateway", key, syncErr, klog.Errorf)
		}
		assert.Equal(t, maxRetries, queue.NumRequeues(key))

		retryOrForget(queue, "gateway", key, syncErr, klog.Errorf)
		assert.Equal(t, 0, queue.NumRequeues(key))
	})
}
