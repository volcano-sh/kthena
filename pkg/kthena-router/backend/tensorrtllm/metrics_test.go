/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tensorrtllm

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

func TestParseIterStatsJSON(t *testing.T) {
	body := []byte(`[
		{
			"numActiveRequests": 1,
			"numQueuedRequests": 2,
			"kvCacheStats": {
				"freeNumBlocks": 97,
				"maxNumBlocks": 100,
				"usedNumBlocks": 3
			}
		},
		{
			"numActiveRequests": 4,
			"numQueuedRequests": 0,
			"kvCacheStats": {
				"freeNumBlocks": 50,
				"maxNumBlocks": 100,
				"usedNumBlocks": 50
			}
		}
	]`)

	got, err := parseIterStatsJSON(body)
	require.NoError(t, err)
	require.Len(t, got, 3)

	engine := NewTensorRTLLMEngine()
	info := engine.GetCountMetricsInfo(got)
	assert.InDelta(t, 4.0, info[utils.RequestRunningNum], 1e-9)
	assert.InDelta(t, 0.0, info[utils.RequestWaitingNum], 1e-9)
	assert.InDelta(t, 0.5, info[utils.KVCacheUsage], 1e-9)
}

func TestParseIterStatsJSON_Empty(t *testing.T) {
	got, err := parseIterStatsJSON([]byte(`[]`))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseIterStatsJSON_UsedFromFree(t *testing.T) {
	body := []byte(`[{
		"numActiveRequests": 1,
		"kvCacheStats": {"freeNumBlocks": 25, "maxNumBlocks": 100}
	}]`)
	got, err := parseIterStatsJSON(body)
	require.NoError(t, err)
	engine := NewTensorRTLLMEngine()
	info := engine.GetCountMetricsInfo(got)
	assert.InDelta(t, 0.75, info[utils.KVCacheUsage], 1e-9)
}

func TestGetCountMetricsInfo(t *testing.T) {
	engine := NewTensorRTLLMEngine()
	allMetrics := map[string]*dto.MetricFamily{
		KVCacheUsage:      gaugeFamily(0.25),
		RequestWaitingNum: gaugeFamily(3),
		RequestRunningNum: gaugeFamily(7),
	}
	got := engine.GetCountMetricsInfo(allMetrics)
	assert.InDelta(t, 0.25, got[utils.KVCacheUsage], 1e-9)
	assert.InDelta(t, 3.0, got[utils.RequestWaitingNum], 1e-9)
	assert.InDelta(t, 7.0, got[utils.RequestRunningNum], 1e-9)
}

func TestGetHistogramPodMetrics_Empty(t *testing.T) {
	engine := NewTensorRTLLMEngine()
	m, h := engine.GetHistogramPodMetrics(nil, nil)
	assert.Empty(t, m)
	assert.Empty(t, h)
}
