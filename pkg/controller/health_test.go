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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthCheck(t *testing.T) {
	tests := []struct {
		name    string
		stop    func(manager, leader context.CancelFunc)
		started []string
		wantErr string
	}{
		{
			name: "standby or disabled, nothing started",
		},
		{
			name:    "leadership lost while the manager runs",
			stop:    func(_, leader context.CancelFunc) { leader() },
			started: []string{"ModelServing", "Autoscaler"},
			wantErr: "controllers stopped: Autoscaler, ModelServing",
		},
		{
			name:    "graceful shutdown",
			stop:    func(manager, _ context.CancelFunc) { manager() },
			started: []string{"ModelServing", "Autoscaler"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, cancelManager := context.WithCancel(context.Background())
			defer cancelManager()
			leader, cancelLeader := context.WithCancel(manager)
			defer cancelLeader()

			h := &Health{}
			done := make(chan struct{}, len(tt.started))
			for _, name := range tt.started {
				go func() {
					h.run(manager, name, func() { <-leader.Done() })
					done <- struct{}{}
				}()
			}
			if tt.stop != nil {
				tt.stop(cancelManager, cancelLeader)
			}
			for range tt.started {
				<-done
			}

			err := h.Check()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestHealthCheckRunningController(t *testing.T) {
	manager, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Health{}
	go h.run(manager, "ModelServing", func() { <-manager.Done() })
	assert.NoError(t, h.Check())
}

func TestNilHealth(t *testing.T) {
	var h *Health
	ran := false
	h.run(context.Background(), "ModelServing", func() { ran = true })
	assert.True(t, ran)
	assert.NoError(t, h.Check())
}
