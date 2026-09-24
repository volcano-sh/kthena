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

package sessionsticky

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMemoryStoreClose_ConcurrentCallsAreSafe(t *testing.T) {
	// Two callers could both find stopCh open and both close it.
	for i := 0; i < 64; i++ {
		store := NewMemoryStore()
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				require.NoError(t, store.Close())
			}()
		}
		wg.Wait()
	}
}

func TestMemoryStoreClose_IsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Close())
	require.NoError(t, store.Close())
}

func TestMemoryStoreClose_StopsTheSweeper(t *testing.T) {
	store := NewMemoryStore()
	binding := Binding{ModelServer: "ms", Pod: "pod"}
	_, err := store.Commit(context.Background(), "key", binding, time.Minute)
	require.NoError(t, err)

	require.NoError(t, store.Close())

	got, ok := store.Get(context.Background(), "key")
	require.True(t, ok)
	require.Equal(t, binding, got)
}
