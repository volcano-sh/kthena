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

package utils

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/klog/v2"
)

// NewRedisClient builds a client from REDIS_HOST, REDIS_PORT and REDIS_PASSWORD without connecting.
func NewRedisClient() *redis.Client {
	redisHost := LoadEnv("REDIS_HOST", "redis-server")
	redisPort := LoadEnv("REDIS_PORT", "6379")
	redisPassword := LoadEnv("REDIS_PASSWORD", "")

	return redis.NewClient(&redis.Options{
		Addr:     redisHost + ":" + redisPort,
		Password: redisPassword,
		DB:       0,
	})
}

func TryGetRedisClient() *redis.Client {
	client := NewRedisClient()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		klog.Errorf("Redis connection failed: %v", err)
		_ = client.Close()
		return nil
	}
	klog.Infof("Redis connection successfully")
	return client
}
