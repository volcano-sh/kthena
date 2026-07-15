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

package predictor

import (
	"fmt"

	"github.com/volcano-sh/kthena/pkg/autoscaler/histogram"
)

// Predictor defines the interface for traffic prediction
type Predictor interface {
	// GetPredictedMetric returns the predicted metric value for the given look-ahead duration
	// It uses historical histogram data to forecast future values
	GetPredictedMetric(metricName string, lookAheadMinutes int) (float64, error)
}

// HistogramBasedPredictor uses histogram data to predict future metric values
type HistogramBasedPredictor struct {
	// DataProvider provides access to historical histogram data
	DataProvider HistogramDataProvider
}

// HistogramDataProvider defines the interface for accessing historical histogram data
type HistogramDataProvider interface {
	// GetHistoricalSnapshots returns historical histogram snapshots for a metric
	GetHistoricalSnapshots(metricName string, windowMinutes int) ([]*histogram.Snapshot, error)
}

// NewHistogramBasedPredictor creates a new predictor
func NewHistogramBasedPredictor(provider HistogramDataProvider) *HistogramBasedPredictor {
	return &HistogramBasedPredictor{
		DataProvider: provider,
	}
}

// GetPredictedMetric returns the predicted metric value
func (p *HistogramBasedPredictor) GetPredictedMetric(metricName string, lookAheadMinutes int) (float64, error) {
	if p.DataProvider == nil {
		return 0, fmt.Errorf("no histogram data provider available")
	}

	// Use a fixed history window (30 minutes) for prediction
	// TODO: Make this configurable via PredictiveScalingConfig.HistoryWindowMinutes
	const defaultHistoryWindow = 30
	snapshots, err := p.DataProvider.GetHistoricalSnapshots(metricName, defaultHistoryWindow)
	if err != nil {
		return 0, err
	}

	if len(snapshots) == 0 {
		return 0, fmt.Errorf("no historical data available for metric %s", metricName)
	}

	// Simple prediction: use the average of recent values
	return p.predictAverage(snapshots)
}

// predictAverage calculates the average of the provided snapshots
func (p *HistogramBasedPredictor) predictAverage(snapshots []*histogram.Snapshot) (float64, error) {
	if len(snapshots) == 0 {
		return 0, fmt.Errorf("no snapshots available")
	}

	// Calculate average of all snapshots
	var total float64
	var count int
	for _, snap := range snapshots {
		if snap != nil && snap.Count() > 0 {
			total += snap.Sum() / float64(snap.Count())
			count++
		}
	}

	if count == 0 {
		return 0, fmt.Errorf("no valid snapshots with data")
	}

	// Return average as prediction
	// TODO: Implement more sophisticated prediction using linear regression
	// or moving average with trend detection
	return total / float64(count), nil
}
