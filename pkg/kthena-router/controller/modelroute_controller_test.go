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

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

func TestEnqueueModelRoute(t *testing.T) {
	tests := []struct {
		name        string
		obj         interface{}
		expectedKey string
	}{
		{
			name: "normal ModelRoute object",
			obj: &aiv1alpha1.ModelRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
			},
			expectedKey: "default/test-route",
		},
		{
			name: "tombstone with DeletedFinalStateUnknown",
			obj: cache.DeletedFinalStateUnknown{
				Key: "default/deleted-route",
				Obj: &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "deleted-route",
						Namespace: "default",
					},
				},
			},
			expectedKey: "default/deleted-route",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[any]())
			defer queue.ShutDown()

			c := &ModelRouteController{
				workqueue: queue,
				store:     datastore.New(),
			}

			c.enqueueModelRoute(tt.obj)

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
