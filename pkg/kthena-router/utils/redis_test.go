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
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRedisClientReadsTheEnvironment(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantAddr     string
		wantPassword string
	}{
		{name: "defaults", wantAddr: "redis-server:6379"},
		{
			name:         "host, port and password",
			env:          map[string]string{"REDIS_HOST": "redis.example", "REDIS_PORT": "6380", "REDIS_PASSWORD": "secret"},
			wantAddr:     "redis.example:6380",
			wantPassword: "secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"REDIS_HOST", "REDIS_PORT", "REDIS_PASSWORD"} {
				t.Setenv(key, tt.env[key])
			}

			client := NewRedisClient()
			defer client.Close()

			assert.Equal(t, tt.wantAddr, client.Options().Addr)
			assert.Equal(t, tt.wantPassword, client.Options().Password)
		})
	}
}

func TestTryGetRedisClient(t *testing.T) {
	redisServer := miniredis.RunT(t)
	t.Setenv("REDIS_HOST", redisServer.Host())
	t.Setenv("REDIS_PORT", redisServer.Port())
	t.Setenv("REDIS_PASSWORD", "")

	client := TryGetRedisClient()
	require.NotNil(t, client, "a reachable Redis gives a client")
	defer client.Close()
	require.NoError(t, client.Ping(context.Background()).Err())

	redisServer.Close()
	assert.Nil(t, TryGetRedisClient(), "an unreachable Redis gives no client")
}
