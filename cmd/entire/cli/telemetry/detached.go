package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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
func BuildEventPayload(cmd *cobra.Command, agent string, isEntireEnabled bool, version string) *EventPayload {
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
func TrackCommandDetached(cmd *cobra.Command, agent string, isEntireEnabled bool, version string) {
	// Check opt-out environment variables
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}

	if cmd == nil {
		return
	}

	if cmd.Hidden {
		return
	}

	payload := BuildEventPayload(cmd, agent, isEntireEnabled, version)
	if payload == nil {
		return
	}

	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}

// BuildPluginEventPayload deliberately omits plugin args/flags — only the
// allowlisted plugin name is recorded. Returns nil on failure.
func BuildPluginEventPayload(pluginName string, isEntireEnabled bool, version string) *EventPayload {
	if pluginName == "" {
		return nil
	}

	machineID, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return nil
	}

	properties := map[string]any{
		"command":         "entire " + pluginName,
		"plugin":          pluginName,
		"isEntireEnabled": isEntireEnabled,
		"cli_version":     version,
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
	}

	return &EventPayload{
		Event:      "cli_plugin_executed",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackPluginDetached records a plugin invocation. Call sites must gate
// on the plugin allowlist — this function does no name filtering itself.
func TrackPluginDetached(pluginName string, isEntireEnabled bool, version string) {
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}

	payload := BuildPluginEventPayload(pluginName, isEntireEnabled, version)
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

	// Resolve the installed git version best-effort. A missing or failing
	// git must never block the rest of the telemetry — the property is simply
	// omitted when it can't be determined.
	if v := gitVersion(context.Background()); v != "" {
		if payload.Properties == nil {
			payload.Properties = map[string]any{}
		}
		payload.Properties["git_version"] = v
	}

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

// gitVersion returns the installed git version (e.g. "2.43.0"), best-effort.
// It returns "" when git is absent, the command fails or times out, or the
// output cannot be parsed — callers must treat "" as "unknown" and move on.
func gitVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return ""
	}
	return parseGitVersion(string(out))
}

// parseGitVersion extracts the version token from `git --version` output, which
// looks like "git version 2.43.0" (sometimes with a platform suffix such as
// "git version 2.39.3 (Apple Git-146)"). Returns "" if the shape is unexpected.
func parseGitVersion(out string) string {
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return ""
	}
	return fields[2]
}
