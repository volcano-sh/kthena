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
	"github.com/gammazero/deque"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
)

type snapshotRecord[T any] struct {
	timestamp int64
	value     T
}

type Number interface {
	int | uint | int8 | uint8 | int16 | uint16 | int32 | uint32 | int64 | uint64 | float32 | float64
}

func isFresh(freshMilliseconds int64, currentTimestamp int64, snapshotTimestamp int64) bool {
	return snapshotTimestamp+freshMilliseconds >= currentTimestamp
}

type RmqRecordSlidingWindow[T Number] struct {
	pool                deque.Deque[snapshotRecord[T]]
	freshMilliseconds   int64
	isBetter            func(me T, other T) bool
	getCurrentTimestamp func() int64
}

func NewMaximumRecordSlidingWindow[T Number](freshMilliseconds int64) *RmqRecordSlidingWindow[T] {
	return &RmqRecordSlidingWindow[T]{
		pool:              deque.Deque[snapshotRecord[T]]{},
		freshMilliseconds: freshMilliseconds,
		isBetter: func(me T, other T) bool {
			return me > other
		},
		getCurrentTimestamp: util.GetCurrentTimestamp,
	}
}

func NewMinimumRecordSlidingWindow[T Number](freshMilliseconds int64) *RmqRecordSlidingWindow[T] {
	return &RmqRecordSlidingWindow[T]{
		pool:              deque.Deque[snapshotRecord[T]]{},
		freshMilliseconds: freshMilliseconds,
		isBetter: func(me T, other T) bool {
			return me < other
		},
		getCurrentTimestamp: util.GetCurrentTimestamp,
	}
}

func (window *RmqRecordSlidingWindow[T]) expire(currentTimestamp int64) {
	for window.pool.Len() > 0 {
		if isFresh(window.freshMilliseconds, currentTimestamp, window.pool.Front().timestamp) {
			break
		}
		window.pool.PopFront()
	}
}

func (window *RmqRecordSlidingWindow[T]) Append(value T) {
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	for window.pool.Len() > 0 {
		if window.isBetter(window.pool.Back().value, value) {
			break
		}
		window.pool.PopBack()
	}
	window.pool.PushBack(snapshotRecord[T]{currentTimestamp, value})
}

func (window *RmqRecordSlidingWindow[T]) GetBest() (value T, ok bool) {
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	if window.pool.Len() == 0 || window.freshMilliseconds == 0 {
		return value, false
	}
	return window.pool.Front().value, true
}

type RmqLineChartSlidingWindow[T Number] struct {
	pool                    deque.Deque[snapshotRecord[T]]
	freshMilliseconds       int64
	driftingValue           T
	driftTimestamp          int64
	maxDriftingMilliseconds int64
	isBetter                func(me T, other T) bool
	getCurrentTimestamp     func() int64
}

func NewMaximumLineChartSlidingWindow[T Number](freshMilliseconds int64) *RmqLineChartSlidingWindow[T] {
	var driftingValue T
	return &RmqLineChartSlidingWindow[T]{
		pool:                    deque.Deque[snapshotRecord[T]]{},
		freshMilliseconds:       freshMilliseconds,
		driftingValue:           driftingValue,
		driftTimestamp:          0,
		maxDriftingMilliseconds: freshMilliseconds * 2,
		isBetter: func(me T, other T) bool {
			return me > other
		},
		getCurrentTimestamp: util.GetCurrentTimestamp,
	}
}

func NewMinimumLineChartSlidingWindow[T Number](freshMilliseconds int64) *RmqLineChartSlidingWindow[T] {
	var driftingValue T
	return &RmqLineChartSlidingWindow[T]{
		pool:                    deque.Deque[snapshotRecord[T]]{},
		freshMilliseconds:       freshMilliseconds,
		driftingValue:           driftingValue,
		driftTimestamp:          0,
		maxDriftingMilliseconds: freshMilliseconds * 2,
		isBetter: func(me T, other T) bool {
			return me < other
		},
		getCurrentTimestamp: util.GetCurrentTimestamp,
	}
}

func (window *RmqLineChartSlidingWindow[T]) expire(currentTimestamp int64) {
	for window.pool.Len() > 0 {
		if isFresh(window.freshMilliseconds, currentTimestamp, window.pool.Front().timestamp) {
			break
		}
		window.pool.PopFront()
	}
	if !isFresh(window.maxDriftingMilliseconds, currentTimestamp, window.driftTimestamp) {
		window.driftTimestamp = 0
	}
}

func (window *RmqLineChartSlidingWindow[T]) Append(value T) {
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	if window.driftTimestamp > 0 {
		for window.pool.Len() > 0 {
			if window.isBetter(window.pool.Back().value, window.driftingValue) {
				break
			}
			window.pool.PopBack()
		}
		window.pool.PushBack(snapshotRecord[T]{currentTimestamp, window.driftingValue})
	}
	window.driftingValue = value
	window.driftTimestamp = currentTimestamp
}

func (window *RmqLineChartSlidingWindow[T]) GetBest(currentValue T) (value T, ok bool) {
	if window.freshMilliseconds == 0 {
		return value, false
	}
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	value = currentValue
	if window.driftTimestamp > 0 {
		if window.isBetter(window.driftingValue, value) {
			value = window.driftingValue
		}
	}
	if window.pool.Len() > 0 {
		frontValue := window.pool.Front().value
		if window.isBetter(frontValue, value) {
			value = frontValue
		}
	}
	return value, true
}

type SnapshotSlidingWindow[T any] struct {
	pool                deque.Deque[snapshotRecord[T]]
	freshMilliseconds   int64
	expireMilliseconds  int64
	getCurrentTimestamp func() int64
}

func NewSnapshotSlidingWindow[T any](freshMilliseconds int64, expireMilliseconds int64) *SnapshotSlidingWindow[T] {
	return &SnapshotSlidingWindow[T]{
		pool:                deque.Deque[snapshotRecord[T]]{},
		freshMilliseconds:   freshMilliseconds,
		expireMilliseconds:  expireMilliseconds,
		getCurrentTimestamp: util.GetCurrentTimestamp,
	}
}

func (window *SnapshotSlidingWindow[T]) expire(currentTimestamp int64) {
	for window.pool.Len() > 0 {
		if isFresh(window.expireMilliseconds, currentTimestamp, window.pool.Front().timestamp) {
			break
		}
		window.pool.PopFront()
	}
	for window.pool.Len() > 1 {
		if isFresh(window.freshMilliseconds, currentTimestamp, window.pool.At(1).timestamp) {
			break
		}
		window.pool.PopFront()
	}
}

func (window *SnapshotSlidingWindow[T]) Append(value T) {
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	window.pool.PushBack(snapshotRecord[T]{currentTimestamp, value})
}

func (window *SnapshotSlidingWindow[T]) GetLastUnfreshSnapshot() (value T, ok bool) {
	if window.freshMilliseconds == 0 {
		return value, false
	}
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	if window.pool.Len() == 0 {
		return value, false
	}
	front := window.pool.Front()
	if isFresh(window.freshMilliseconds, currentTimestamp, front.timestamp) {
		return value, false
	}
	return front.value, true
}

func (window *SnapshotSlidingWindow[T]) GetLastUnfreshSnapshotWithTimestamp() (value T, timestamp int64, ok bool) {
	if window.freshMilliseconds == 0 {
		return value, 0, false
	}
	currentTimestamp := window.getCurrentTimestamp()
	window.expire(currentTimestamp)
	if window.pool.Len() == 0 {
		return value, 0, false
	}
	front := window.pool.Front()
	if isFresh(window.freshMilliseconds, currentTimestamp, front.timestamp) {
		return value, 0, false
	}
	return front.value, front.timestamp, true
}
