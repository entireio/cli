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
	SendEvent("invalid json")
	SendEvent("")
	SendEvent("{}")
}
