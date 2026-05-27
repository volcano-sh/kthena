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

package datastore

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// redisOnFlightHashKey is the Redis hash key used to store per-pod in-flight counts.
const redisOnFlightHashKey = "kthena:on-flight-requests"

// OnFlightCounter tracks the number of in-flight requests dispatched by the router
// to each backend pod. In a multi-router deployment, a Redis-backed implementation
// lets all router replicas share a single global counter, so scheduling decisions
// incorporate cross-router visibility instead of only per-instance view.
type OnFlightCounter interface {
	// Incr atomically increments the counter for podName and returns the new value.
	Incr(ctx context.Context, podName types.NamespacedName) (int64, error)
	// Decr atomically decrements the counter for podName and returns the new value.
	Decr(ctx context.Context, podName types.NamespacedName) (int64, error)
	// Delete removes the counter entry for podName. Call this when a pod is
	// removed so stale keys do not accumulate in Redis.
	Delete(ctx context.Context, podName types.NamespacedName) error
	// BatchGet retrieves current counters for a set of pods in one round-trip.
	BatchGet(ctx context.Context, podNames []types.NamespacedName) (map[types.NamespacedName]int64, error)
}

func podOnFlightField(podName types.NamespacedName) string {
	return podName.Namespace + "/" + podName.Name
}

// RedisOnFlightCounter uses a Redis hash (HINCRBY / HMGET) to maintain globally
// visible in-flight request counts across multiple router replicas.
type RedisOnFlightCounter struct {
	client *redis.Client
}

// NewRedisOnFlightCounter creates a new Redis-backed on-flight counter.
func NewRedisOnFlightCounter(client *redis.Client) *RedisOnFlightCounter {
	return &RedisOnFlightCounter{client: client}
}

func (r *RedisOnFlightCounter) Incr(ctx context.Context, podName types.NamespacedName) (int64, error) {
	return r.client.HIncrBy(ctx, redisOnFlightHashKey, podOnFlightField(podName), 1).Result()
}

func (r *RedisOnFlightCounter) Decr(ctx context.Context, podName types.NamespacedName) (int64, error) {
	val, err := r.client.HIncrBy(ctx, redisOnFlightHashKey, podOnFlightField(podName), -1).Result()
	if err != nil {
		return 0, err
	}
	if val < 0 {
		// Guard against stale negative values after a router restart.
		_ = r.client.HSet(ctx, redisOnFlightHashKey, podOnFlightField(podName), 0).Err()
		val = 0
	}
	return val, nil
}

func (r *RedisOnFlightCounter) Delete(ctx context.Context, podName types.NamespacedName) error {
	return r.client.HDel(ctx, redisOnFlightHashKey, podOnFlightField(podName)).Err()
}

func (r *RedisOnFlightCounter) BatchGet(ctx context.Context, podNames []types.NamespacedName) (map[types.NamespacedName]int64, error) {
	if len(podNames) == 0 {
		return nil, nil
	}
	fields := make([]string, len(podNames))
	for i, n := range podNames {
		fields[i] = podOnFlightField(n)
	}
	vals, err := r.client.HMGet(ctx, redisOnFlightHashKey, fields...).Result()
	if err != nil {
		return nil, err
	}
	result := make(map[types.NamespacedName]int64, len(podNames))
	for i, v := range vals {
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			klog.V(4).Infof("failed to parse on-flight count for pod %s: %v", podNames[i], err)
			continue
		}
		result[podNames[i]] = n
	}
	return result, nil
}
