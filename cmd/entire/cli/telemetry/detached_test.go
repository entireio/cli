package telemetry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// testAgentName is the agent used across the skill/checkpoint payload tests.
const testAgentName = "claude-code"

func TestBuildPluginEventPayload(t *testing.T) {
	t.Parallel()
	payload := BuildPluginEventPayload("pgr", true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildPluginEventPayload returned nil")
		return
	}
	if payload.Event != "cli_plugin_executed" {
		t.Errorf("Event = %q, want %q", payload.Event, "cli_plugin_executed")
	}
	if got := payload.Properties["plugin"]; got != "pgr" {
		t.Errorf("plugin property = %v, want %q", got, "pgr")
	}
	if got := payload.Properties["command"]; got != "entire pgr" {
		t.Errorf("command property = %v, want %q", got, "entire pgr")
	}
	if got := payload.Properties["cli_version"]; got != "1.2.3" {
		t.Errorf("cli_version property = %v, want %q", got, "1.2.3")
	}
	if got := payload.Properties["isEntireEnabled"]; got != true {
		t.Errorf("isEntireEnabled property = %v, want true", got)
	}
	// Plugin args/flags must never appear in the payload.
	if _, ok := payload.Properties["flags"]; ok {
		t.Error("plugin payload must not include 'flags'")
	}
	if _, ok := payload.Properties["args"]; ok {
		t.Error("plugin payload must not include 'args'")
	}
}

func TestBuildPluginEventPayload_EmptyName(t *testing.T) {
	t.Parallel()
	if got := BuildPluginEventPayload("", true, "1.0.0"); got != nil {
		t.Errorf("expected nil for empty plugin name, got %+v", got)
	}
}

func TestEventPayloadSerialization(t *testing.T) {
	payload := EventPayload{
		Event:      "cli_command_executed",
		DistinctID: "test-machine-id",
		Properties: map[string]any{
			"command":         "entire status",
			"strategy":        "manual-commit",
			"agent":           "claude-code",
			"isEntireEnabled": true,
			"cli_version":     "1.0.0",
			"os":              "darwin",
			"arch":            "arm64",
		},
		Timestamp: time.Date(2026, 1, 28, 12, 0, 0, 0, time.UTC),
	}

	// Serialize
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal EventPayload: %v", err)
	}

	// Deserialize
	var decoded EventPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal EventPayload: %v", err)
	}

	// Verify fields
	if decoded.Event != payload.Event {
		t.Errorf("Event = %q, want %q", decoded.Event, payload.Event)
	}
	if decoded.DistinctID != payload.DistinctID {
		t.Errorf("DistinctID = %q, want %q", decoded.DistinctID, payload.DistinctID)
	}
	if !decoded.Timestamp.Equal(payload.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", decoded.Timestamp, payload.Timestamp)
	}

	// Verify properties
	if cmd, ok := decoded.Properties["command"].(string); !ok || cmd != "entire status" {
		t.Errorf("Properties[command] = %v, want %q", decoded.Properties["command"], "entire status")
	}
}

func TestTrackCommandDetachedSkipsNilCommand(_ *testing.T) {
	// Should not panic with nil command
	TrackCommandDetached(nil, "claude-code", true, "1.0.0")
}

func TestTrackCommandDetachedSkipsHiddenCommands(_ *testing.T) {
	hiddenCmd := &cobra.Command{
		Use:    "__send_analytics",
		Hidden: true,
	}

	// Should not panic and should skip hidden commands
	TrackCommandDetached(hiddenCmd, "claude-code", true, "1.0.0")
}

func TestTrackCommandDetachedRespectsOptOut(t *testing.T) {
	t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "1")

	cmd := &cobra.Command{
		Use: "status",
	}

	// Should not panic and should respect opt-out
	TrackCommandDetached(cmd, "claude-code", true, "1.0.0")
}

func TestBuildEventPayloadAgent(t *testing.T) {
	tests := []struct {
		name          string
		inputAgent    string
		expectedAgent string
	}{
		{"defaults empty to auto", "", "auto"},
		{"preserves explicit agent", "claude-code", "claude-code"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "test"}
			payload := BuildEventPayload(cmd, tt.inputAgent, true, "1.0.0")
			if payload == nil {
				t.Fatal("Expected non-nil payload")
				return
			}

			agent, ok := payload.Properties["agent"].(string)
			if !ok {
				t.Fatal("Expected agent property to be a string")
				return
			}
			if agent != tt.expectedAgent {
				t.Errorf("agent = %q, want %q", agent, tt.expectedAgent)
			}
		})
	}
}

func TestSendEventHandlesInvalidJSON(_ *testing.T) {
	// Should not panic with invalid JSON
	SendEvents("invalid json")
	SendEvents("")
	SendEvents("{}")
}

func TestParseGitVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"standard", "git version 2.43.0\n", "2.43.0"},
		{"apple suffix", "git version 2.39.3 (Apple Git-146)\n", "2.39.3"},
		{"windows suffix", "git version 2.45.1.windows.1\n", "2.45.1.windows.1"},
		{"no trailing newline", "git version 2.40.0", "2.40.0"},
		{"empty", "", ""},
		{"unexpected prefix", "not git output", ""},
		{"missing version token", "git version", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseGitVersion(tt.out); got != tt.want {
				t.Errorf("parseGitVersion(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}

func TestBuildSkillEventPayload(t *testing.T) {
	t.Parallel()
	payload := BuildSkillEventPayload(SkillInvocation{
		Skill:     "entire",
		Agent:     testAgentName,
		Signal:    "prompt_slash_command",
		EventType: "prompt_invocation",
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSkillEventPayload returned nil")
		return
	}
	if payload.Event != "cli_skill_invoked" {
		t.Errorf("Event = %q, want %q", payload.Event, "cli_skill_invoked")
	}
	if got := payload.Properties["skill"]; got != "entire" {
		t.Errorf("skill property = %v, want %q", got, "entire")
	}
	if got := payload.Properties["agent"]; got != testAgentName {
		t.Errorf("agent property = %v, want %q", got, testAgentName)
	}
	if got := payload.Properties["signal"]; got != "prompt_slash_command" {
		t.Errorf("signal property = %v, want %q", got, "prompt_slash_command")
	}
	if got := payload.Properties["event_type"]; got != "prompt_invocation" {
		t.Errorf("event_type property = %v, want %q", got, "prompt_invocation")
	}
	if got := payload.Properties["cli_version"]; got != "1.2.3" {
		t.Errorf("cli_version property = %v, want %q", got, "1.2.3")
	}
	if got := payload.Properties["isEntireEnabled"]; got != true {
		t.Errorf("isEntireEnabled property = %v, want true", got)
	}
	// The payload must stay content-free: skill/plugin names only, never
	// prompt text, arguments, or flags.
	for _, forbidden := range []string{"prompt", "args", "flags"} {
		if _, ok := payload.Properties[forbidden]; ok {
			t.Errorf("skill payload must not include %q", forbidden)
		}
	}
}

func TestBuildSkillEventPayload_EmptySkill(t *testing.T) {
	t.Parallel()
	if got := BuildSkillEventPayload(SkillInvocation{Agent: testAgentName}, true, "1.0.0"); got != nil {
		t.Errorf("expected nil for empty skill name, got %+v", got)
	}
}

func TestBuildSkillEventPayload_UnlistedSkillNameIsNotSent(t *testing.T) {
	t.Parallel()
	// Skill names are user-defined tokens: a custom slash command like
	// /customer-acme-incident-123 can carry sensitive identifiers, so only
	// allowlisted names pass through verbatim.
	payload := BuildSkillEventPayload(SkillInvocation{
		Skill:     "customer-acme-incident-123",
		Agent:     testAgentName,
		Signal:    "prompt_slash_command",
		EventType: "prompt_invocation",
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildSkillEventPayload returned nil")
		return
	}
	if got := payload.Properties["skill"]; got != "custom" {
		t.Errorf("skill property = %v, want %q", got, "custom")
	}
	for _, v := range payload.Properties {
		if s, ok := v.(string); ok && s == "customer-acme-incident-123" {
			t.Errorf("raw skill name leaked into payload property %v", v)
		}
	}
}

func TestBuildCommitCondensedPayload(t *testing.T) {
	t.Parallel()
	usedSearch := true
	priorAI := true
	payload := BuildCommitCondensedPayload(CommitCondensedSignal{
		Agent:            testAgentName,
		UsedSearch:       &usedSearch,
		UsedSearchSource: "command",
		PriorAIHistory:   &priorAI,
		FilesCommitted:   4,
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildCommitCondensedPayload returned nil")
		return
	}
	if payload.Event != "cli_commit_condensed" {
		t.Errorf("Event = %q, want %q", payload.Event, "cli_commit_condensed")
	}
	if got := payload.Properties["agent"]; got != testAgentName {
		t.Errorf("agent property = %v, want %q", got, testAgentName)
	}
	if got := payload.Properties["used_search"]; got != true {
		t.Errorf("used_search property = %v, want true", got)
	}
	if got := payload.Properties["used_search_source"]; got != "command" {
		t.Errorf("used_search_source property = %v, want %q", got, "command")
	}
	if got := payload.Properties["prior_ai_history"]; got != true {
		t.Errorf("prior_ai_history property = %v, want true", got)
	}
	if got := payload.Properties["files_committed"]; got != 4 {
		t.Errorf("files_committed property = %v, want 4", got)
	}
	// The payload must stay content-free: booleans and counts only, never
	// file paths, prompts, or transcript content.
	for _, forbidden := range []string{"files", "paths", "prompt", "transcript"} {
		if _, ok := payload.Properties[forbidden]; ok {
			t.Errorf("checkpoint payload must not include %q", forbidden)
		}
	}
}

// TestBuildCommitCondensedPayload_UnmeasurableOmitsUsedSearch pins the shape
// that keeps the missed-opportunity ratio honest: when the probe could not run,
// used_search is ABSENT rather than false, so a consumer filtering on
// `used_search = false` excludes those rows instead of counting them as "did
// not search". A missing PostHog property is not false.
func TestBuildCommitCondensedPayload_UnmeasurableOmitsUsedSearch(t *testing.T) {
	t.Parallel()
	priorAI := true
	payload := BuildCommitCondensedPayload(CommitCondensedSignal{
		Agent:            testAgentName,
		UsedSearch:       nil,
		UsedSearchSource: "unsupported",
		PriorAIHistory:   &priorAI,
		FilesCommitted:   2,
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildCommitCondensedPayload returned nil")
		return
	}
	if _, ok := payload.Properties["used_search"]; ok {
		t.Errorf("used_search must be absent when unmeasurable, got %v", payload.Properties["used_search"])
	}
	if got := payload.Properties["used_search_source"]; got != "unsupported" {
		t.Errorf("used_search_source property = %v, want %q", got, "unsupported")
	}
	if got := payload.Properties["prior_ai_history"]; got != true {
		t.Errorf("prior_ai_history property = %v, want true", got)
	}
}

// TestBuildCommitCondensedPayload_UnmeasurableOmitsPriorAIHistory gives
// prior_ai_history the same honesty contract as used_search: when the git-log
// probe could not run, the property is ABSENT rather than false, so a consumer
// filtering on `prior_ai_history = false` excludes those rows instead of
// counting them as "touched no AI-dense files".
func TestBuildCommitCondensedPayload_UnmeasurableOmitsPriorAIHistory(t *testing.T) {
	t.Parallel()
	usedSearch := false
	payload := BuildCommitCondensedPayload(CommitCondensedSignal{
		Agent:            testAgentName,
		UsedSearch:       &usedSearch,
		UsedSearchSource: "none",
		PriorAIHistory:   nil,
		FilesCommitted:   2,
	}, true, "1.2.3")
	if payload == nil {
		t.Fatal("BuildCommitCondensedPayload returned nil")
		return
	}
	if _, ok := payload.Properties["prior_ai_history"]; ok {
		t.Errorf("prior_ai_history must be absent when unmeasurable, got %v", payload.Properties["prior_ai_history"])
	}
	if got := payload.Properties["used_search"]; got != false {
		t.Errorf("used_search property = %v, want false", got)
	}
}
