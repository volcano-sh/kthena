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

func Test_snapshotSlidingWindowSinceLastUnfresh(t *testing.T) {
	window := NewSnapshotSlidingWindow[string](10000, 15000)
	for _, step := range []struct {
		append string
		at     int
	}{{"t1", 1}, {"t3", 3}, {"t5", 5}, {"t7", 7}} {
		window.getCurrentTimestamp = dateFunc(step.at)
		window.Append(step.append)
	}

	// The steps share one window and must run in this order, because each read
	// also removes the snapshots that have expired by then.
	steps := []struct {
		name string
		at   int
		want []string
	}{
		{name: "every snapshot is still fresh", at: 7, want: nil},
		{name: "first snapshot turned unfresh", at: 12, want: []string{"t1", "t3", "t5", "t7"}},
		{name: "older unfresh snapshot dropped", at: 14, want: []string{"t3", "t5", "t7"}},
		{name: "only the newest unfresh snapshot is kept", at: 16, want: []string{"t5", "t7"}},
		{name: "last snapshot is the baseline", at: 18, want: []string{"t7"}},
		{name: "baseline kept until it expires", at: 21, want: []string{"t7"}},
		{name: "baseline expired", at: 23, want: nil},
	}
	for _, step := range steps {
		window.getCurrentTimestamp = dateFunc(step.at)
		assert.Equal(t, step.want, window.GetSnapshotsSinceLastUnfresh(), step.name)
	}
}

func Test_snapshotSlidingWindowSinceLastUnfreshWithZeroTTL(t *testing.T) {
	window := NewSnapshotSlidingWindow[string](0, 0)
	window.getCurrentTimestamp = dateFunc(1)
	window.Append("t1")

	window.getCurrentTimestamp = dateFunc(50)
	assert.Nil(t, window.GetSnapshotsSinceLastUnfresh())
}
