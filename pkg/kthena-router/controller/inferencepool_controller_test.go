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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestEnqueueInferencePool(t *testing.T) {
	tests := []struct {
		name        string
		obj         interface{}
		expectedKey string
	}{
		{
			name: "normal InferencePool object",
			obj: &inferencev1.InferencePool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
			},
			expectedKey: "default/test-pool",
		},
		{
			name: "tombstone with DeletedFinalStateUnknown",
			obj: cache.DeletedFinalStateUnknown{
				Key: "default/deleted-pool",
				Obj: &inferencev1.InferencePool{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "deleted-pool",
						Namespace: "default",
					},
				},
			},
			expectedKey: "default/deleted-pool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
			defer queue.ShutDown()

			c := &InferencePoolController{
				workqueue: queue,
				store:     datastore.New(),
			}

			c.enqueueInferencePool(tt.obj)

			if queue.Len() != 1 {
				t.Fatalf("expected 1 item in queue, got %d", queue.Len())
			}

			item, shutdown := queue.Get()
			if shutdown {
				t.Fatal("unexpected queue shutdown")
			}
			if item != tt.expectedKey {
				t.Errorf("expected key %q, got %q", tt.expectedKey, item)
			}
		})
	}
}
