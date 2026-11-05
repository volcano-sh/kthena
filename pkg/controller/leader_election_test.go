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
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Losing the leader lease must terminate the process so the ousted
// leader stops reconciling. Runs the handler in a child process because
// the correct behavior exits.
func TestOnStoppedLeadingTerminatesProcess(t *testing.T) {
	if os.Getenv("KTHENA_LEADER_LOSS_CHILD") == "1" {
		onStoppedLeading()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestOnStoppedLeadingTerminatesProcess")
	cmd.Env = append(os.Environ(), "KTHENA_LEADER_LOSS_CHILD=1")
	out, err := cmd.CombinedOutput()
	assert.Error(t, err, "losing leadership must terminate the process, output: %s", out)
	if exitErr, ok := err.(*exec.ExitError); ok {
		assert.NotZero(t, exitErr.ExitCode(), "output: %s", out)
	}
	assert.Contains(t, string(out), "leader election lost")
}
