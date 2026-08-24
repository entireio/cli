package telemetry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

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
		Agent:     "claude-code",
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
	if got := payload.Properties["agent"]; got != "claude-code" {
		t.Errorf("agent property = %v, want %q", got, "claude-code")
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
	if got := BuildSkillEventPayload(SkillInvocation{Agent: "claude-code"}, true, "1.0.0"); got != nil {
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
		Agent:     "claude-code",
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
