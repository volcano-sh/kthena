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

package histogram

import (
	"fmt"
	"math"
	"sort"

	io_prometheus_client "github.com/prometheus/client_model/go"
)

type Snapshot struct {
	sum     float64
	count   int64
	buckets []Bucket
}

type Bucket struct {
	leValue float64
	count   int64
}

// ResetSince reports whether the histogram was reset after past was taken.
// The difference s - past then cannot be a histogram of the observations made
// in between: the count difference is negative, the buckets differ, or the
// cumulative bucket differences go down or exceed the count difference.
func (s *Snapshot) ResetSince(past *Snapshot) bool {
	countDiff := s.count - past.count
	if countDiff < 0 || len(s.buckets) != len(past.buckets) {
		return true
	}
	lastDiff := int64(0)
	for i, bucket := range s.buckets {
		if bucket.leValue != past.buckets[i].leValue {
			return true
		}
		diff := bucket.count - past.buckets[i].count
		if diff < lastDiff || diff > countDiff {
			return true
		}
		lastDiff = diff
	}
	return false
}

func NewDefaultSnapshot() *Snapshot {
	return &Snapshot{
		sum:     0,
		count:   0,
		buckets: []Bucket{},
	}
}

func NewSnapshotOfHistogram(histogram *io_prometheus_client.Histogram) *Snapshot {
	buckets := make([]Bucket, 0, len(histogram.GetBucket()))
	for _, bucket := range histogram.GetBucket() {
		buckets = append(buckets, Bucket{bucket.GetUpperBound(), int64(bucket.GetCumulativeCount())})
	}
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].leValue < buckets[j].leValue
	})
	return &Snapshot{
		sum:     histogram.GetSampleSum(),
		count:   int64(histogram.GetSampleCount()),
		buckets: buckets,
	}
}

func QuantileInDiff(percentile int32, now *Snapshot, past *Snapshot) (float64, error) {
	if percentile < 1 || percentile > 100 {
		return 0, fmt.Errorf("invalid percentile (%v invalid out of [1,100])", percentile)
	}
	if now == nil {
		return 0, fmt.Errorf("now is nil")
	}
	if past == nil {
		return 0, fmt.Errorf("past is nil")
	}
	if now.buckets == nil {
		return 0, fmt.Errorf("now.buckets is nil")
	}
	if past.buckets == nil {
		return 0, fmt.Errorf("past.buckets is nil")
	}

	totalCountDiff := now.count - past.count
	if totalCountDiff < 0 {
		return 0, fmt.Errorf("invalid totalCountDiff (%v invalid out of [0, +Inf))", totalCountDiff)
	}
	if totalCountDiff == 0 {
		return 0, nil
	}

	length := len(now.buckets)
	pastBucketsIsDefault := len(past.buckets) == 0
	if !pastBucketsIsDefault && length != len(past.buckets) {
		return 0, fmt.Errorf("unmatched buckets lengths (len(now.buckets): %v, len(past.buckets): %v)", length, len(past.buckets))
	}
	rankFracMolecular := totalCountDiff * int64(percentile)
	rankCeil := rankFracMolecular / 100
	if rankFracMolecular%100 > 0 {
		rankCeil++
	}
	lowValue := 0.0
	lastHighRank := int64(0)
	for i := range length {
		nowBucket := now.buckets[i]
		lowRank := lastHighRank + 1
		highRank := nowBucket.count
		if !pastBucketsIsDefault {
			pastBucket := past.buckets[i]
			highRank -= pastBucket.count
		}
		if highRank < lastHighRank {
			return 0, fmt.Errorf("non-decreasing is broken")
		}
		countBucket := highRank - lowRank + 1
		highValue := nowBucket.leValue
		if math.IsInf(highValue, 1) {
			highValue = lowValue * 2.0
		}
		if countBucket > 0 && rankCeil <= highRank {
			result := highValue - float64(highRank-rankCeil)*(highValue-lowValue)/float64(countBucket)
			return result, nil
		}
		lowValue = highValue
		lastHighRank = highRank
	}
	return 0, fmt.Errorf("percentile %v not found", percentile)
}
