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

package datastructure

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func dateFunc(sec int) func() int64 {
	return func() int64 { return time.Date(2025, 1, 1, 9, 0, sec, 0, time.UTC).UnixMilli() }
}

func Test_maximumRecordSlidingWindow(t *testing.T) {
	assert := assert.New(t)

	window := NewMaximumRecordSlidingWindow[int](10000)

	_, ok := window.GetBest()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append(6)

	window.getCurrentTimestamp = dateFunc(3)
	window.Append(3)

	window.getCurrentTimestamp = dateFunc(5)
	window.Append(5)

	window.getCurrentTimestamp = dateFunc(7)
	window.Append(2)

	best, ok := window.GetBest()
	assert.Equal(6, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(12)
	best, ok = window.GetBest()
	assert.Equal(5, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(14)
	best, ok = window.GetBest()
	assert.Equal(5, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(16)
	best, ok = window.GetBest()
	assert.Equal(2, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(18)
	_, ok = window.GetBest()
	assert.False(ok)
}

func Test_minimumRecordSlidingWindow(t *testing.T) {
	assert := assert.New(t)

	window := NewMinimumRecordSlidingWindow[int](10000)

	_, ok := window.GetBest()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append(6)

	window.getCurrentTimestamp = dateFunc(3)
	window.Append(3)

	window.getCurrentTimestamp = dateFunc(5)
	window.Append(5)

	window.getCurrentTimestamp = dateFunc(7)
	window.Append(7)

	best, ok := window.GetBest()
	assert.Equal(3, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(12)
	best, ok = window.GetBest()
	assert.Equal(3, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(14)
	best, ok = window.GetBest()
	assert.Equal(5, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(16)
	best, ok = window.GetBest()
	assert.Equal(7, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(18)
	_, ok = window.GetBest()
	assert.False(ok)
}

func Test_recordSlidingWindowWithZeroTTL(t *testing.T) {
	assert := assert.New(t)

	window := NewMinimumRecordSlidingWindow[int](0)

	_, ok := window.GetBest()
	assert.False(ok)

	window.Append(0)
	_, ok = window.GetBest()
	assert.False(ok)
}

func Test_maximumLineChartSlidingWindow(t *testing.T) {
	assert := assert.New(t)

	window := NewMaximumLineChartSlidingWindow[int](10000)

	best, ok := window.GetBest(6)
	assert.Equal(6, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append(6)

	window.getCurrentTimestamp = dateFunc(3)
	window.Append(3)

	window.getCurrentTimestamp = dateFunc(5)
	window.Append(5)

	window.getCurrentTimestamp = dateFunc(7)
	window.Append(2)

	best, ok = window.GetBest(0)
	assert.Equal(6, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(12)
	best, ok = window.GetBest(0)
	assert.Equal(6, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(14)
	best, ok = window.GetBest(0)
	assert.Equal(5, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(16)
	best, ok = window.GetBest(0)
	assert.Equal(5, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(18)
	best, ok = window.GetBest(0)
	assert.Equal(2, best)
	assert.True(ok)

	best, ok = window.GetBest(3)
	assert.Equal(3, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(26)
	best, ok = window.GetBest(0)
	assert.Equal(2, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(28)
	best, ok = window.GetBest(0)
	assert.Equal(0, best)
	assert.True(ok)
}

func Test_minimumLineChartSlidingWindow(t *testing.T) {
	assert := assert.New(t)

	window := NewMinimumLineChartSlidingWindow[int](10000)

	best, ok := window.GetBest(6)
	assert.Equal(6, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append(6)

	window.getCurrentTimestamp = dateFunc(3)
	window.Append(3)

	window.getCurrentTimestamp = dateFunc(5)
	window.Append(9)

	window.getCurrentTimestamp = dateFunc(7)
	window.Append(4)

	best, ok = window.GetBest(10)
	assert.Equal(3, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(12)
	best, ok = window.GetBest(10)
	assert.Equal(3, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(14)
	best, ok = window.GetBest(10)
	assert.Equal(3, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(16)
	best, ok = window.GetBest(10)
	assert.Equal(4, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(18)
	best, ok = window.GetBest(10)
	assert.Equal(4, best)
	assert.True(ok)

	best, ok = window.GetBest(2)
	assert.Equal(2, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(26)
	best, ok = window.GetBest(10)
	assert.Equal(4, best)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(28)
	best, ok = window.GetBest(10)
	assert.Equal(10, best)
	assert.True(ok)
}

func Test_lineChartSlidingWindowWithZeroTTL(t *testing.T) {
	assert := assert.New(t)

	window := NewMinimumLineChartSlidingWindow[int](0)

	_, ok := window.GetBest(0)
	assert.False(ok)

	window.Append(0)
	_, ok = window.GetBest(0)
	assert.False(ok)
}

func Test_snapshotSlidingWindow(t *testing.T) {
	assert := assert.New(t)

	window := NewSnapshotSlidingWindow[string](10000, 15000)

	_, ok := window.GetLastUnfreshSnapshot()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append("foo")

	window.getCurrentTimestamp = dateFunc(3)
	window.Append("bar")

	window.getCurrentTimestamp = dateFunc(5)
	window.Append("quz")

	window.getCurrentTimestamp = dateFunc(7)
	window.Append("abc")

	_, ok = window.GetLastUnfreshSnapshot()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(12)
	last, ok := window.GetLastUnfreshSnapshot()
	assert.Equal("foo", last)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(14)
	last, ok = window.GetLastUnfreshSnapshot()
	assert.Equal("bar", last)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(16)
	last, ok = window.GetLastUnfreshSnapshot()
	assert.Equal("quz", last)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(18)
	last, ok = window.GetLastUnfreshSnapshot()
	assert.Equal("abc", last)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(21)
	last, ok = window.GetLastUnfreshSnapshot()
	assert.Equal("abc", last)
	assert.True(ok)

	window.getCurrentTimestamp = dateFunc(23)
	_, ok = window.GetLastUnfreshSnapshot()
	assert.False(ok)
}

func Test_snapshotSlidingWindowWithZeroTTL(t *testing.T) {
	assert := assert.New(t)

	window := NewSnapshotSlidingWindow[string](0, 0)

	_, ok := window.GetLastUnfreshSnapshot()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append("foo")

	_, ok = window.GetLastUnfreshSnapshot()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(50)
	_, ok = window.GetLastUnfreshSnapshot()
	assert.False(ok)
}

func Test_snapshotSlidingWindowGetLastSnapshot(t *testing.T) {
	assert := assert.New(t)

	window := NewSnapshotSlidingWindow[string](10000, 15000)

	_, ok := window.GetLastSnapshot()
	assert.False(ok)

	window.getCurrentTimestamp = dateFunc(1)
	window.Append("foo")

	last, ok := window.GetLastSnapshot()
	assert.True(ok)
	assert.Equal("foo", last)

	window.getCurrentTimestamp = dateFunc(3)
	window.Append("bar")

	last, ok = window.GetLastSnapshot()
	assert.True(ok)
	assert.Equal("bar", last)

	window.getCurrentTimestamp = dateFunc(19)
	_, ok = window.GetLastSnapshot()
	assert.False(ok)
}
