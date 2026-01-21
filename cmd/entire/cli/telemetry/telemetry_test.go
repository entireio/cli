package telemetry

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestNewClientOptOut(t *testing.T) {
	t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "1")

	client := NewClient("1.0.0", nil)

	if _, ok := client.(*NoOpClient); !ok {
		t.Error("ENTIRE_TELEMETRY_OPTOUT=1 should return NoOpClient")
	}
}

func TestNewClientOptOutWithAnyValue(t *testing.T) {
	t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "yes")

	client := NewClient("1.0.0", nil)

	if _, ok := client.(*NoOpClient); !ok {
		t.Error("ENTIRE_TELEMETRY_OPTOUT with any value should return NoOpClient")
	}
}

func TestNewClientTelemetryDisabledInSettings(t *testing.T) {
	disabled := false
	client := NewClient("1.0.0", &disabled)

	if _, ok := client.(*NoOpClient); !ok {
		t.Error("telemetryEnabled=false should return NoOpClient")
	}
}

func TestNewClientNilTelemetryDefaultsToDisabled(t *testing.T) {
	// Ensure no opt-out env var is set
	t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "")

	client := NewClient("1.0.0", nil)

	if _, ok := client.(*NoOpClient); !ok {
		t.Error("telemetryEnabled=nil should return NoOpClient (disabled by default)")
	}
}

func TestNoOpClientMethods(_ *testing.T) {
	client := &NoOpClient{}

	// Should not panic
	client.TrackCommand(nil, "", "", false)
	client.TrackCommand(&cobra.Command{Use: "test"}, "manual-commit", "claude-code", true)
	client.Close()
}

func TestPostHogClientSkipsHiddenCommands(_ *testing.T) {
	client := &PostHogClient{
		machineID: "test-id",
	}

	hiddenCmd := &cobra.Command{
		Use:    "hidden",
		Hidden: true,
	}

	// Should not panic and should skip hidden commands
	client.TrackCommand(hiddenCmd, "manual-commit", "claude-code", true)
}

func TestPostHogClientSkipsHelpCommand(_ *testing.T) {
	client := &PostHogClient{
		machineID: "test-id",
	}

	helpCmd := &cobra.Command{
		Use: "help",
	}

	// Should not panic and should skip help command
	client.TrackCommand(helpCmd, "manual-commit", "claude-code", true)
}

func TestPostHogClientSkipsCompletionCommand(_ *testing.T) {
	client := &PostHogClient{
		machineID: "test-id",
	}

	completionCmd := &cobra.Command{
		Use: "completion",
	}

	// Should not panic and should skip completion command
	client.TrackCommand(completionCmd, "manual-commit", "claude-code", true)
}

func TestPostHogClientSkipsNilCommand(_ *testing.T) {
	client := &PostHogClient{
		machineID: "test-id",
	}

	// Should not panic with nil command
	client.TrackCommand(nil, "", "", false)
}

func TestPostHogClientClose(_ *testing.T) {
	client := &PostHogClient{
		machineID: "test-id",
		// client is nil, should not panic
	}

	// Should not panic when internal client is nil
	client.Close()
}

func TestTrackCommandUsesCommandPath(t *testing.T) {
	client := &PostHogClient{
		machineID: "test-id",
	}

	cmd := &cobra.Command{
		Use: "session",
	}
	rootCmd := &cobra.Command{
		Use: "entire",
	}
	rootCmd.AddCommand(cmd)

	// Should not panic - just verify the command path is accessible
	if cmd.CommandPath() != "entire session" {
		t.Errorf("CommandPath() = %q, want %q", cmd.CommandPath(), "entire session")
	}

	// TrackCommand should not panic with nil internal client
	client.TrackCommand(cmd, "manual-commit", "claude-code", true)
}
