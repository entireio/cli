package telemetry

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/posthog/posthog-go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	// PostHogAPIKey is set at build time for production
	PostHogAPIKey = "phc_development_key"
	// PostHogEndpoint is set at build time for production
	PostHogEndpoint = "https://eu.i.posthog.com"
)

// EventPayload represents the data passed to the detached subprocess.
// Note: APIKey and Endpoint are intentionally excluded to avoid exposing
// them in process listings (ps/top). SendEvent reads them from package-level vars.
type EventPayload struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
	Timestamp  time.Time      `json:"timestamp"`
}

// silentLogger suppresses PostHog log output - expected for CLI best-effort telemetry
type silentLogger struct{}

func (silentLogger) Logf(_ string, _ ...interface{})   {}
func (silentLogger) Debugf(_ string, _ ...interface{}) {}
func (silentLogger) Warnf(_ string, _ ...interface{})  {}
func (silentLogger) Errorf(_ string, _ ...interface{}) {}

// BuildEventPayload constructs the event payload for tracking.
// Exported for testing. Returns nil if the payload cannot be built.
func BuildEventPayload(cmd *cobra.Command, strategy, agent string, isEntireEnabled bool, version string) *EventPayload {
	if cmd == nil {
		return nil
	}

	// Get machine ID for distinct_id
	machineID, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return nil
	}

	// Collect flag names (not values) for privacy
	var flags []string
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		flags = append(flags, flag.Name)
	})

	selectedAgent := agent
	if selectedAgent == "" {
		selectedAgent = "auto"
	}

	properties := map[string]any{
		"command":         cmd.CommandPath(),
		"strategy":        strategy,
		"agent":           selectedAgent,
		"isEntireEnabled": isEntireEnabled,
		"cli_version":     version,
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
	}

	if len(flags) > 0 {
		properties["flags"] = strings.Join(flags, ",")
	}

	return &EventPayload{
		Event:      "cli_command_executed",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackCommandDetached tracks a command execution by spawning a detached subprocess.
// This returns immediately without blocking the CLI.
func TrackCommandDetached(cmd *cobra.Command, strategy, agent string, isEntireEnabled bool, version string) {
	// Check opt-out environment variables
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}

	if cmd == nil {
		return
	}

	// Skip hidden commands
	if cmd.Hidden {
		return
	}

	payload := BuildEventPayload(cmd, strategy, agent, isEntireEnabled, version)
	if payload == nil {
		return
	}

	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}

// SendEvent processes an event payload in the detached subprocess.
// This is called by the hidden __send_analytics command.
func SendEvent(payloadJSON string) {
	var payload EventPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return
	}

	// Create PostHog client - no need for fast timeouts since we're detached
	// Read API key and endpoint from package-level vars (not passed via argv for security)
	client, err := posthog.NewWithConfig(PostHogAPIKey, posthog.Config{
		Endpoint:     PostHogEndpoint,
		Logger:       silentLogger{},
		DisableGeoIP: posthog.Ptr(true),
	})
	if err != nil {
		return
	}
	defer func() {
		_ = client.Close()
	}()

	// Build properties
	props := posthog.NewProperties()
	for k, v := range payload.Properties {
		props.Set(k, v)
	}

	//nolint:errcheck // Best effort telemetry - don't block on result
	_ = client.Enqueue(posthog.Capture{
		DistinctId: payload.DistinctID,
		Event:      payload.Event,
		Properties: props,
		Timestamp:  payload.Timestamp,
	})
}
