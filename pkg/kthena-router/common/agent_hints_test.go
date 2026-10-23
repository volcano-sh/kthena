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

package common

import (
	"testing"
)

func TestContextType_DefaultContextWeight(t *testing.T) {
	tests := []struct {
		name     string
		ct       ContextType
		expected float64
	}{
		{"system_prompt most durable", ContextTypeSystemPrompt, 0.1},
		{"tool_def durable", ContextTypeToolDef, 0.2},
		{"conversation baseline", ContextTypeConversation, 1.0},
		{"reasoning most disposable", ContextTypeReasoning, 2.0},
		{"unknown fallback", ContextTypeUnknown, 1.0},
		{"garbage fallback", ContextType("weird"), 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ct.DefaultContextWeight(); got != tc.expected {
				t.Fatalf("DefaultContextWeight(%q) = %v, want %v", tc.ct, got, tc.expected)
			}
		})
	}
}

func TestContextType_Valid(t *testing.T) {
	tests := []struct {
		name    string
		ct      ContextType
		isValid bool
	}{
		{"system_prompt", ContextTypeSystemPrompt, true},
		{"tool_def", ContextTypeToolDef, true},
		{"conversation", ContextTypeConversation, true},
		{"reasoning", ContextTypeReasoning, true},
		{"unknown", ContextTypeUnknown, true},
		{"garbage", ContextType("bogus"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ct.Valid() != tc.isValid {
				t.Fatalf("Valid(%q) = %v, want %v", tc.ct, !tc.isValid, tc.isValid)
			}
		})
	}
}

func TestAgentHints_IsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		hints    *AgentHints
		expected bool
	}{
		{"nil is empty", nil, true},
		{"zero struct is empty", &AgentHints{}, true},
		{"program_id makes it non-empty", &AgentHints{ProgramID: "a"}, false},
		{"priority 0 alone is empty", &AgentHints{TaskPriority: 0}, true},
		{"priority 1 makes it non-empty", &AgentHints{TaskPriority: 1}, false},
		{"speculative false alone is empty", &AgentHints{SpeculativePrefill: false}, true},
		{"speculative true makes it non-empty", &AgentHints{SpeculativePrefill: true}, false},
		{"ttl <= 0 alone is empty", &AgentHints{SessionTTLHint: 0}, true},
		{"ttl > 0 makes it non-empty", &AgentHints{SessionTTLHint: 1}, false},
		{"all fields", &AgentHints{
			ProgramID:          "p",
			TaskPriority:       5,
			ContextType:        ContextTypeSystemPrompt,
			SpeculativePrefill: true,
			SessionTTLHint:     30,
			SessionID:          "s",
			AgentVersion:       "v2",
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.hints.IsEmpty(); got != tc.expected {
				t.Fatalf("IsEmpty() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestAgentHints_DefaultPriority(t *testing.T) {
	if (&AgentHints{}).DefaultPriority() != 1 {
		t.Fatalf("empty hints default priority should be 1")
	}
	got := (&AgentHints{TaskPriority: 7}).DefaultPriority()
	if got != 7 {
		t.Fatalf("got %d, want 7", got)
	}
}

func TestAgentHints_DefaultSessionTTL(t *testing.T) {
	if (&AgentHints{}).DefaultSessionTTL() != 60 {
		t.Fatalf("empty hints default ttl should be 60")
	}
	if (&AgentHints{SessionTTLHint: 120}).DefaultSessionTTL() != 120 {
		t.Fatalf("ttl hint should be passed through")
	}
}

func TestAgentHints_HasProgramAffinity(t *testing.T) {
	tests := []struct {
		name string
		h    *AgentHints
		want bool
	}{
		{"nil", nil, false},
		{"empty program_id", &AgentHints{}, false},
		{"present", &AgentHints{ProgramID: "p"}, true},
	}
	for _, tc := range tests {
		if got := tc.h.HasProgramAffinity(); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgentHints_HasSessionPinning(t *testing.T) {
	tests := []struct {
		name string
		h    *AgentHints
		want bool
	}{
		{"nil", nil, false},
		{"empty session_id", &AgentHints{}, false},
		{"present", &AgentHints{SessionID: "s"}, true},
	}
	for _, tc := range tests {
		if got := tc.h.HasSessionPinning(); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAgentHints_Equal(t *testing.T) {
	a := &AgentHints{ProgramID: "p", TaskPriority: 3, ContextType: ContextTypeConversation}
	b := &AgentHints{ProgramID: "p", TaskPriority: 3, ContextType: ContextTypeConversation}
	c := &AgentHints{ProgramID: "q", TaskPriority: 3, ContextType: ContextTypeConversation}
	if !a.Equal(b) {
		t.Fatalf("a and b should be equal")
	}
	if a.Equal(c) {
		t.Fatalf("a and c should not be equal")
	}
	if a.Equal(nil) {
		t.Fatalf("a != nil")
	}
	var empty *AgentHints
	if !empty.Equal(nil) {
		t.Fatalf("nil == nil")
	}
}

func TestAgentHints_MarshalUnmarshal(t *testing.T) {
	original := &AgentHints{
		ProgramID:          "code-agent/v2",
		TaskPriority:       7,
		ContextType:        ContextTypeSystemPrompt,
		SpeculativePrefill: true,
		SessionTTLHint:     180,
		SessionID:          "sess-42",
		AgentVersion:       "2.1.0",
	}
	raw, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	decoded, err := UnmarshalAgentHints(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded == nil {
		t.Fatalf("decoded should not be nil")
	}
	if !original.Equal(decoded) {
		t.Fatalf("roundtrip mismatch: orig=%s decoded=%s", original, decoded)
	}
}

func TestUnmarshalAgentHints_Empty(t *testing.T) {
	h, err := UnmarshalAgentHints("")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if h != nil {
		t.Fatalf("expected nil for empty input")
	}
}

func TestUnmarshalAgentHints_ForwardCompat(t *testing.T) {
	// Future version that adds new unknown fields — they must be silently dropped.
	input := `{"program_id":"p","unknown_new_field":42,"another":"yes"}`
	h, err := UnmarshalAgentHints(input)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if h == nil {
		t.Fatalf("decoded should not be nil — known field present")
	}
	if h.ProgramID != "p" {
		t.Fatalf("program_id should parse despite unknown fields")
	}
}

func TestParseAgentHintsFromHeader(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantNil  bool
		wantPID  string
		wantPrio int
		wantSID  string
		wantTTL  int
	}{
		{"empty yields nil", "", true, "", 0, "", 0},
		{"whitespace only", "   ", true, "", 0, "", 0},
		{"valid JSON", `{"program_id":"p1","task_priority":5,"session_id":"s1","session_ttl_hint":120}`, false, "p1", 5, "s1", 120},
		{"KV fallback", "program_id=kv-agent,task_priority=3,session_id=s-123", false, "kv-agent", 3, "s-123", 0},
		{"KV fallback with ttl", "program_id=p,ttl=60", false, "p", 0, "", 60},
		{"kv parse tolerance - unknown keys ignored", "garbage=here,program_id=real", false, "real", 0, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := ParseAgentHintsFromHeader(tc.raw)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.wantNil {
				if h != nil {
					t.Fatalf("expected nil, got %v", h)
				}
				return
			}
			if h == nil {
				t.Fatalf("expected non-nil hints")
			}
			if h.ProgramID != tc.wantPID {
				t.Fatalf("ProgramID = %q, want %q", h.ProgramID, tc.wantPID)
			}
			if h.TaskPriority != tc.wantPrio {
				t.Fatalf("TaskPriority = %d, want %d", h.TaskPriority, tc.wantPrio)
			}
			if h.SessionID != tc.wantSID {
				t.Fatalf("SessionID = %q, want %q", h.SessionID, tc.wantSID)
			}
			if tc.wantTTL != 0 && h.SessionTTLHint != tc.wantTTL {
				t.Fatalf("SessionTTLHint = %d, want %d", h.SessionTTLHint, tc.wantTTL)
			}
		})
	}
}

func TestAgentHints_String(t *testing.T) {
	h := &AgentHints{ProgramID: "a", SessionID: "s", TaskPriority: 3}
	s := h.String()
	if s == "" {
		t.Fatalf("String should not be empty")
	}
	// minimal sanity — the string form contains key identifiers
	if !contains(s, "a") || !contains(s, "s") {
		t.Fatalf("String missing program/session ids: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
