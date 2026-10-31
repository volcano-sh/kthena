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

package plugins

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// --- Test helpers ---------------------------------------------------------

func setupAgentAffinityRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, client
}

func newAgentAffinityPluginForTest(t *testing.T, client *redis.Client, argsYAML string) *AgentAffinity {
	t.Helper()
	var args AgentAffinityArgs
	if argsYAML != "" {
		if err := yaml.Unmarshal([]byte(argsYAML), &args); err != nil {
			t.Fatalf("YAML parse %q: %v", argsYAML, err)
		}
	}
	args.normalize()
	return &AgentAffinity{
		name:  AgentAffinityPluginName,
		args:  args,
		redis: client,
	}
}

func testPodInfo(name, namespace string, running, waiting float64) *datastore.PodInfo {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	pi := datastore.NewPodInfo(pod, "vllm")
	pi.RequestRunningNum = running
	pi.RequestWaitingNum = waiting
	return pi
}

// --- ScorePlugin interface conformance -----------------------------------

func TestAgentAffinity_Name(t *testing.T) {
	p := &AgentAffinity{name: AgentAffinityPluginName}
	if p.Name() != AgentAffinityPluginName {
		t.Fatalf("Name = %q, want %q", p.Name(), AgentAffinityPluginName)
	}
}

func TestAgentAffinity_ImplementsFrameworkInterface(t *testing.T) {
	var _ framework.ScorePlugin = (*AgentAffinity)(nil)
}

// --- No-op / nil-score paths --------------------------------------------

func TestAgentAffinity_Score_NoHints(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	p := newAgentAffinityPluginForTest(t, client, "")
	pods := []*datastore.PodInfo{testPodInfo("a", "ns", 0, 0)}

	if scores := p.Score(nil, pods); scores != nil {
		t.Fatalf("expected nil scores for nil ctx")
	}

	ctx := &framework.Context{Model: "m"}
	if scores := p.Score(ctx, pods); scores != nil {
		t.Fatalf("expected nil scores when no AgentHints")
	}

	ctx.AgentHints = &common.AgentHints{TaskPriority: 5}
	if scores := p.Score(ctx, pods); scores != nil {
		t.Fatalf("expected nil scores when no program_id")
	}
}

func TestAgentAffinity_Score_NoRedis(t *testing.T) {
	p := &AgentAffinity{
		name:  AgentAffinityPluginName,
		args:  AgentAffinityArgs{},
		redis: nil,
	}
	p.args.normalize()

	ctx := &framework.Context{
		Model:      "m",
		AgentHints: &common.AgentHints{ProgramID: "program-1"},
	}
	pods := []*datastore.PodInfo{testPodInfo("a", "ns", 0, 0)}
	if scores := p.Score(ctx, pods); scores != nil {
		t.Fatalf("expected nil scores when redis is unavailable")
	}
}

func TestAgentAffinity_Score_EmptyPods(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	p := newAgentAffinityPluginForTest(t, client, "")
	ctx := &framework.Context{
		Model:      "m",
		AgentHints: &common.AgentHints{ProgramID: "p"},
	}
	if scores := p.Score(ctx, nil); scores != nil {
		t.Fatalf("expected nil scores for nil pods")
	}
	if scores := p.Score(ctx, []*datastore.PodInfo{}); scores != nil {
		t.Fatalf("expected nil scores for empty pod list")
	}
}

// --- Scoring core paths --------------------------------------------------

func TestAgentAffinity_Score_KnownPodGetsAffinity(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	p := newAgentAffinityPluginForTest(t, client, `{"affinityScore":80,"loadBonus":15}`)

	programID := "code-agent/v1"
	podA := testPodInfo("pod-a", "ns", 0, 0)
	podB := testPodInfo("pod-b", "ns", 0, 0)

	now := time.Now().Unix()
	key := BuildAgentAffinityKey(programID)
	ctxRedis := context.Background()
	client.HSet(ctxRedis, key, "pod-a.ns", fmt.Sprintf("%d", now))

	ctx := &framework.Context{
		Model:      "m",
		AgentHints: &common.AgentHints{ProgramID: programID},
	}

	scores := p.Score(ctx, []*datastore.PodInfo{podA, podB})
	if scores == nil {
		t.Fatalf("expected non-nil scores when program_id is present")
	}

	gotA, okA := scores[podA]
	if !okA {
		t.Fatalf("pod-a missing from scores")
	}
	if gotA != 95 {
		t.Fatalf("pod-a score = %d, want 95 (80+15 load bonus)", gotA)
	}

	gotB := scores[podB]
	if gotB != 0 {
		t.Fatalf("pod-b score = %d, want 0 (unknown pod)", gotB)
	}
}

func TestAgentAffinity_Score_NoLoadBonusWhenBusy(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	p := newAgentAffinityPluginForTest(t, client, `{"affinityScore":80,"loadBonus":15,"maxRunningRequests":32}`)

	programID := "p"
	podBusy := testPodInfo("busy", "ns", 33, 0)
	podWait := testPodInfo("wait", "ns", 10, 65)

	key := BuildAgentAffinityKey(programID)
	now := time.Now().Unix()
	ctxRedis := context.Background()
	client.HSet(ctxRedis, key, "busy.ns", fmt.Sprintf("%d", now))
	client.HSet(ctxRedis, key, "wait.ns", fmt.Sprintf("%d", now))

	ctx := &framework.Context{
		AgentHints: &common.AgentHints{ProgramID: programID},
	}
	scores := p.Score(ctx, []*datastore.PodInfo{podBusy, podWait})

	if scores[podBusy] != 0 {
		t.Fatalf("busy pod = %d, want 0 (above running cap)", scores[podBusy])
	}
	if scores[podWait] != 80 {
		t.Fatalf("wait pod = %d, want 80 (affinity only, waiting exceeds cap)", scores[podWait])
	}
}

func TestAgentAffinity_Score_CapAt100(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	p := newAgentAffinityPluginForTest(t, client, `{"affinityScore":90,"loadBonus":20}`)

	programID := "p"
	pod := testPodInfo("pod", "ns", 0, 0)

	key := BuildAgentAffinityKey(programID)
	now := time.Now().Unix()
	client.HSet(context.Background(), key, "pod.ns", fmt.Sprintf("%d", now))

	ctx := &framework.Context{AgentHints: &common.AgentHints{ProgramID: programID}}
	scores := p.Score(ctx, []*datastore.PodInfo{pod})
	if scores[pod] != 100 {
		t.Fatalf("score = %d, want 100 (clamped)", scores[pod])
	}
}

// --- Redis refresh behaviour --------------------------------------------

func TestAgentAffinity_Score_RefreshesOwners(t *testing.T) {
	mr, client := setupAgentAffinityRedis(t)
	p := newAgentAffinityPluginForTest(t, client, `{"programKeyTTLSeconds":120}`)

	programID := "p"
	pod := testPodInfo("pod", "ns", 0, 0)

	key := BuildAgentAffinityKey(programID)
	old := time.Now().Add(-10 * time.Minute).Unix()
	client.HSet(context.Background(), key, "pod.ns", fmt.Sprintf("%d", old))

	ctx := &framework.Context{AgentHints: &common.AgentHints{ProgramID: programID}}
	p.Score(ctx, []*datastore.PodInfo{pod})

	ttl, err := client.TTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > 125*time.Second {
		t.Fatalf("TTL = %v, expected ~120s", ttl)
	}

	val, err := client.HGet(context.Background(), key, "pod.ns").Result()
	if err != nil {
		t.Fatalf("HGet: %v", err)
	}
	refreshed, _ := strconv.ParseInt(val, 10, 64)
	if refreshed < time.Now().Add(-5*time.Second).Unix() {
		t.Fatalf("timestamp not refreshed: %d", refreshed)
	}
	_ = mr // silence assignment-used warning when miniredis var is just for defer
}

// --- NewAgentAffinity construction --------------------------------------

func TestNewAgentAffinity_ArgsNormalization(t *testing.T) {
	raw := runtime.RawExtension{Raw: []byte(`{}`)}
	plugin := NewAgentAffinity(nil, raw)
	if plugin == nil {
		t.Fatalf("plugin should not be nil")
	}
	if plugin.args.AffinityScore != defaultAffinityScore {
		t.Fatalf("AffinityScore = %d, want %d", plugin.args.AffinityScore, defaultAffinityScore)
	}
	if plugin.args.LoadBonus != defaultLoadBonus {
		t.Fatalf("LoadBonus = %d, want %d", plugin.args.LoadBonus, defaultLoadBonus)
	}
	if plugin.args.MaxRunningRequests != float64(defaultMaxRunningRequests) {
		t.Fatalf("MaxRunningRequests = %.0f, want %.0f", plugin.args.MaxRunningRequests, float64(defaultMaxRunningRequests))
	}
}

func TestNewAgentAffinity_InvalidYAML_Defaults(t *testing.T) {
	raw := runtime.RawExtension{Raw: []byte(`not yaml`)}
	plugin := NewAgentAffinity(nil, raw)
	if plugin == nil {
		t.Fatalf("plugin should still be created with defaults")
	}
}

// --- Program ownership recording ---------------------------------------

func TestAgentAffinity_RecordProgramOwnership(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	plugin := &AgentAffinity{
		name:  AgentAffinityPluginName,
		args:  AgentAffinityArgs{ProgramKeyTTLSeconds: 60},
		redis: client,
	}

	plugin.RecordProgramOwnership("", "pod.ns")
	plugin.RecordProgramOwnership("p", "")
	plugin.RecordProgramOwnership("p", "pod.ns")

	key := BuildAgentAffinityKey("p")
	exists, _ := client.HExists(context.Background(), key, "pod.ns").Result()
	if !exists {
		t.Fatalf("RecordProgramOwnership did not write owner")
	}
}

func TestAgentAffinity_SnapshotProgramOwners(t *testing.T) {
	_, client := setupAgentAffinityRedis(t)
	plugin := &AgentAffinity{
		name:  AgentAffinityPluginName,
		args:  AgentAffinityArgs{},
		redis: client,
	}

	plugin.RecordProgramOwnership("p1", "a.ns")
	plugin.RecordProgramOwnership("p1", "b.ns")
	plugin.RecordProgramOwnership("p2", "c.ns")

	snaps := plugin.SnapshotProgramOwners(context.Background(), "p1", "p2")
	if len(snaps) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(snaps))
	}
	var p1 *ProgramOwnersSnapshot
	for i := range snaps {
		if snaps[i].ProgramID == "p1" {
			p1 = &snaps[i]
		}
	}
	if p1 == nil || len(p1.Owners) != 2 {
		t.Fatalf("p1 should have 2 owners, got %+v", p1)
	}
}

// --- Helpers: AgentPriorityScore / ContextWeight -------------------------

func TestAgentPriorityScore(t *testing.T) {
	tests := []struct {
		name string
		h    *common.AgentHints
		want float64
	}{
		{"nil → 1.0", nil, 1.0},
		{"zero priority → 1.0", &common.AgentHints{}, 1.0},
		{"priority 1 → 1.0", &common.AgentHints{TaskPriority: 1}, 1.0},
		{"priority 10 → 2.0", &common.AgentHints{TaskPriority: 10}, 2.0},
		{"priority 11 clamped to 10 → 2.0", &common.AgentHints{TaskPriority: 11}, 2.0},
		{"negative → 1.0 (clamped)", &common.AgentHints{TaskPriority: -5}, 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentPriorityScore(tc.h); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContextWeight(t *testing.T) {
	if ContextWeight(nil) != 1.0 {
		t.Fatalf("nil hints weight = 1.0")
	}
	h := &common.AgentHints{ContextType: common.ContextTypeSystemPrompt}
	if ContextWeight(h) != 0.1 {
		t.Fatalf("system_prompt weight = 0.1")
	}
}
