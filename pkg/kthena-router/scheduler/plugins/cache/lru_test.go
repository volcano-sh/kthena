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

package cache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewLRUCache tests the constructor.
func TestNewLRUCache(t *testing.T) {
	t.Run("valid size creates cache without error", func(t *testing.T) {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		assert.NotNil(t, c)
		assert.Equal(t, 0, c.Len())
	})

	t.Run("size zero returns error", func(t *testing.T) {
		c, err := NewLRUCache[string, int](0, nil)
		assert.Error(t, err)
		assert.Nil(t, c)
	})

	t.Run("negative size returns error", func(t *testing.T) {
		c, err := NewLRUCache[string, int](-1, nil)
		assert.Error(t, err)
		assert.Nil(t, c)
	})

	t.Run("with eviction callback does not panic on creation", func(t *testing.T) {
		evicted := false
		c, err := NewLRUCache[string, int](5, func(key string, value int) {
			evicted = true
		})
		require.NoError(t, err)
		assert.NotNil(t, c)
		assert.False(t, evicted)
	})

	t.Run("implements Cache interface", func(t *testing.T) {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		// Compile-time check that LRUCache implements Cache.
		var _ Cache[string, int] = c
	})
}

// TestLRUCache_Add tests the Add method.
func TestLRUCache_Add(t *testing.T) {
	t.Run("add single entry", func(t *testing.T) {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		c.Add("k", 1)
		assert.Equal(t, 1, c.Len())
	})

	t.Run("add multiple distinct entries", func(t *testing.T) {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		c.Add("a", 1)
		c.Add("b", 2)
		c.Add("c", 3)
		assert.Equal(t, 3, c.Len())
	})

	t.Run("add duplicate key overwrites value", func(t *testing.T) {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		c.Add("k", 1)
		c.Add("k", 99)
		assert.Equal(t, 1, c.Len())
		v, ok := c.Get("k")
		assert.True(t, ok)
		assert.Equal(t, 99, v)
	})

	t.Run("add beyond capacity evicts oldest entry", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](2, nil)
		c.Add("first", 1)
		c.Add("second", 2)
		c.Add("third", 3) // "first" should be evicted
		assert.Equal(t, 2, c.Len())
		assert.False(t, c.Contains("first"))
		assert.True(t, c.Contains("second"))
		assert.True(t, c.Contains("third"))
	})
}

// TestLRUCache_Get tests the Get method.
func TestLRUCache_Get(t *testing.T) {
	t.Run("get existing key returns value and true", func(t *testing.T) {
		c, _ := NewLRUCache[string, string](10, nil)
		c.Add("hello", "world")
		v, ok := c.Get("hello")
		assert.True(t, ok)
		assert.Equal(t, "world", v)
	})

	t.Run("get nonexistent key returns zero value and false", func(t *testing.T) {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		v, ok := c.Get("missing")
		assert.False(t, ok)
		assert.Equal(t, 0, v)
	})

	t.Run("get promotes entry to most-recently-used", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](2, nil)
		c.Add("first", 1)
		c.Add("second", 2)
		// Access "first" to promote it
		_, _ = c.Get("first")
		// Adding a third entry should evict "second" (now the LRU)
		c.Add("third", 3)
		assert.True(t, c.Contains("first"))
		assert.False(t, c.Contains("second"))
		assert.True(t, c.Contains("third"))
	})
}

// TestLRUCache_Remove tests the Remove method.
func TestLRUCache_Remove(t *testing.T) {
	t.Run("remove existing key decrements length", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("k", 1)
		c.Remove("k")
		assert.Equal(t, 0, c.Len())
		assert.False(t, c.Contains("k"))
	})

	t.Run("remove nonexistent key does not panic", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		assert.NotPanics(t, func() {
			c.Remove("missing")
		})
		assert.Equal(t, 0, c.Len())
	})

	t.Run("remove one of several entries leaves others intact", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("a", 1)
		c.Add("b", 2)
		c.Add("c", 3)
		c.Remove("b")
		assert.Equal(t, 2, c.Len())
		assert.True(t, c.Contains("a"))
		assert.False(t, c.Contains("b"))
		assert.True(t, c.Contains("c"))
	})
}

// TestLRUCache_Contains tests the Contains method.
func TestLRUCache_Contains(t *testing.T) {
	t.Run("contains returns true for existing key", func(t *testing.T) {
		c, _ := NewLRUCache[int, string](10, nil)
		c.Add(42, "value")
		assert.True(t, c.Contains(42))
	})

	t.Run("contains returns false for missing key", func(t *testing.T) {
		c, _ := NewLRUCache[int, string](10, nil)
		assert.False(t, c.Contains(99))
	})

	t.Run("contains returns false after remove", func(t *testing.T) {
		c, _ := NewLRUCache[int, string](10, nil)
		c.Add(1, "a")
		c.Remove(1)
		assert.False(t, c.Contains(1))
	})

	t.Run("contains returns false after clear", func(t *testing.T) {
		c, _ := NewLRUCache[int, string](10, nil)
		c.Add(1, "a")
		c.Add(2, "b")
		c.Clear()
		assert.False(t, c.Contains(1))
		assert.False(t, c.Contains(2))
	})
}

// TestLRUCache_Len tests the Len method.
func TestLRUCache_Len(t *testing.T) {
	tests := []struct {
		name    string
		ops     func(c *LRUCache[string, int])
		wantLen int
	}{
		{
			name:    "empty cache has length 0",
			ops:     func(c *LRUCache[string, int]) {},
			wantLen: 0,
		},
		{
			name: "length increases on add",
			ops: func(c *LRUCache[string, int]) {
				c.Add("a", 1)
				c.Add("b", 2)
			},
			wantLen: 2,
		},
		{
			name: "length unchanged when overwriting existing key",
			ops: func(c *LRUCache[string, int]) {
				c.Add("a", 1)
				c.Add("a", 2)
			},
			wantLen: 1,
		},
		{
			name: "length decreases on remove",
			ops: func(c *LRUCache[string, int]) {
				c.Add("a", 1)
				c.Add("b", 2)
				c.Remove("a")
			},
			wantLen: 1,
		},
		{
			name: "length is zero after clear",
			ops: func(c *LRUCache[string, int]) {
				c.Add("a", 1)
				c.Add("b", 2)
				c.Clear()
			},
			wantLen: 0,
		},
		{
			name: "length capped at capacity",
			ops: func(c *LRUCache[string, int]) {
				// Capacity is 10 per NewLRUCache call below; add more.
				for i := 0; i < 15; i++ {
					c.Add(string(rune('a'+i)), i)
				}
			},
			wantLen: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewLRUCache[string, int](10, nil)
			require.NoError(t, err)
			tt.ops(c)
			assert.Equal(t, tt.wantLen, c.Len())
		})
	}
}

// TestLRUCache_Clear tests the Clear method.
func TestLRUCache_Clear(t *testing.T) {
	t.Run("clear empty cache does not panic", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		assert.NotPanics(t, func() { c.Clear() })
		assert.Equal(t, 0, c.Len())
	})

	t.Run("clear removes all entries", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("x", 10)
		c.Add("y", 20)
		c.Add("z", 30)
		c.Clear()
		assert.Equal(t, 0, c.Len())
	})

	t.Run("cache is usable after clear", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("before", 1)
		c.Clear()
		c.Add("after", 2)
		assert.Equal(t, 1, c.Len())
		v, ok := c.Get("after")
		assert.True(t, ok)
		assert.Equal(t, 2, v)
		_, ok = c.Get("before")
		assert.False(t, ok)
	})
}

// TestLRUCache_Keys tests the Keys method.
func TestLRUCache_Keys(t *testing.T) {
	t.Run("keys on empty cache returns empty slice", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		keys := c.Keys()
		assert.Empty(t, keys)
	})

	t.Run("keys returns all inserted keys", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("a", 1)
		c.Add("b", 2)
		c.Add("c", 3)
		keys := c.Keys()
		assert.Len(t, keys, 3)
		assert.ElementsMatch(t, []string{"a", "b", "c"}, keys)
	})

	t.Run("keys are ordered oldest to newest", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("first", 1)
		c.Add("second", 2)
		c.Add("third", 3)
		keys := c.Keys()
		assert.Equal(t, []string{"first", "second", "third"}, keys)
	})

	t.Run("get promotes key to newest position in keys order", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("a", 1)
		c.Add("b", 2)
		c.Add("c", 3)
		// Access "a" — it should move to the end (newest)
		_, _ = c.Get("a")
		keys := c.Keys()
		assert.Equal(t, []string{"b", "c", "a"}, keys)
	})

	t.Run("keys after clear returns empty slice", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](10, nil)
		c.Add("a", 1)
		c.Clear()
		assert.Empty(t, c.Keys())
	})

	t.Run("evicted entries are not present in keys", func(t *testing.T) {
		c, _ := NewLRUCache[string, int](2, nil)
		c.Add("first", 1)
		c.Add("second", 2)
		c.Add("third", 3) // evicts "first"
		keys := c.Keys()
		assert.Len(t, keys, 2)
		assert.NotContains(t, keys, "first")
		assert.Contains(t, keys, "second")
		assert.Contains(t, keys, "third")
	})
}

// TestLRUCache_EvictionCallback tests that the onEvict callback is invoked correctly.
func TestLRUCache_EvictionCallback(t *testing.T) {
	t.Run("callback called with correct key and value on eviction", func(t *testing.T) {
		type evictRecord struct {
			key   string
			value int
		}
		var records []evictRecord

		c, err := NewLRUCache[string, int](2, func(key string, value int) {
			records = append(records, evictRecord{key, value})
		})
		require.NoError(t, err)

		c.Add("first", 10)
		c.Add("second", 20)
		c.Add("third", 30) // evicts "first"

		require.Len(t, records, 1)
		assert.Equal(t, "first", records[0].key)
		assert.Equal(t, 10, records[0].value)
	})

	t.Run("callback called multiple times as capacity is exceeded", func(t *testing.T) {
		evictCount := 0
		c, err := NewLRUCache[int, int](2, func(key int, value int) {
			evictCount++
		})
		require.NoError(t, err)

		for i := 0; i < 5; i++ {
			c.Add(i, i*10)
		}
		// 5 adds into a cache of size 2 → 3 evictions
		assert.Equal(t, 3, evictCount)
		assert.Equal(t, 2, c.Len())
	})

	t.Run("nil callback does not panic on eviction", func(t *testing.T) {
		c, err := NewLRUCache[string, int](1, nil)
		require.NoError(t, err)
		assert.NotPanics(t, func() {
			c.Add("a", 1)
			c.Add("b", 2) // triggers eviction with nil callback
		})
	})
}

// TestLRUCache_InterfaceCompliance verifies LRUCache satisfies Cache via table-driven interface calls.
func TestLRUCache_InterfaceCompliance(t *testing.T) {
	newCache := func(t *testing.T) Cache[string, int] {
		c, err := NewLRUCache[string, int](10, nil)
		require.NoError(t, err)
		return c
	}

	t.Run("interface Add and Get", func(t *testing.T) {
		c := newCache(t)
		c.Add("k", 7)
		v, ok := c.Get("k")
		assert.True(t, ok)
		assert.Equal(t, 7, v)
	})

	t.Run("interface Remove and Contains", func(t *testing.T) {
		c := newCache(t)
		c.Add("k", 7)
		c.Remove("k")
		assert.False(t, c.Contains("k"))
	})

	t.Run("interface Len and Clear", func(t *testing.T) {
		c := newCache(t)
		c.Add("a", 1)
		c.Add("b", 2)
		assert.Equal(t, 2, c.Len())
		c.Clear()
		assert.Equal(t, 0, c.Len())
	})

	t.Run("interface Keys", func(t *testing.T) {
		c := newCache(t)
		c.Add("x", 1)
		c.Add("y", 2)
		keys := c.Keys()
		assert.Equal(t, []string{"x", "y"}, keys)
	})
}
