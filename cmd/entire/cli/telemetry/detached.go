package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/internal/entireclient/userdirs"
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

// spawnDetachedAnalytics sends the payload from a detached `entire
// __send_analytics` child so the network call never blocks the CLI. The empty
// dir keeps the child out of the parent's working directory.
func spawnDetachedAnalytics(payloadJSON string) {
	execx.SpawnDetached("", "__send_analytics", payloadJSON)
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
	//
	// Skipped for the pre-consent cli_installed event: its disclosure
	// enumerates an exact, fixed set of properties (method/channel/
	// cli_version/os/arch) with no git_version. Other events are unaffected.
	if payload.Event != "cli_installed" {
		if v := gitVersion(context.Background()); v != "" {
			if payload.Properties == nil {
				payload.Properties = map[string]any{}
			}
			payload.Properties["git_version"] = v
		}
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

// validInstallMethods is the closed set of installation channels recorded on
// the cli_installed event. Unknown methods are dropped (no event) so manual or
// mistaken invocations can't pollute install attribution — this set matches the
// values documented in docs/security-and-privacy.md.
var validInstallMethods = map[string]bool{
	"bash":  true,
	"brew":  true,
	"scoop": true,
}

// BuildInstallEventPayload constructs the cli_installed event payload.
// Exported for testing. Returns nil for an unrecognized method or if the
// machine ID cannot be resolved.
func BuildInstallEventPayload(method, version string) *EventPayload {
	if !validInstallMethods[method] {
		return nil
	}

	machineID, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return nil
	}

	properties := map[string]any{
		"method":      method,
		"channel":     versioncheck.ReleaseChannel(version),
		"cli_version": version,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
	}

	return &EventPayload{
		Event:      "cli_installed",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackInstallDetached records a one-time install event by spawning a detached
// subprocess. Fires before the in-app telemetry consent prompt, so it is gated
// only by ENTIRE_TELEMETRY_OPTOUT (see docs/security-and-privacy.md).
func TrackInstallDetached(method, version string) {
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}

	payload := BuildInstallEventPayload(method, version)
	if payload == nil {
		return
	}

	// Fire at most once per machine. The brew/scoop post-install hooks also run
	// on upgrade/reinstall, so without this gate every upgrade (especially
	// frequent nightlies) would re-emit cli_installed and inflate install
	// counts. The sentinel keeps the event one-time, as documented.
	if !markFirstInstall() {
		return
	}

	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}

// markFirstInstall records a one-shot sentinel in the user cache dir and
// reports whether this is the first install-track on this machine. It returns
// true exactly once; later calls (upgrades/reinstalls) find the sentinel and
// return false so the cli_installed event stays one-time. Best-effort: if the
// sentinel cannot be resolved or created for any reason other than "already
// exists", it returns true rather than silently dropping a genuine install.
func markFirstInstall() bool {
	path := filepath.Join(userdirs.Cache(), "install_tracked")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return true
	}
	// #nosec G304 -- path is the trusted user cache dir plus a constant
	// filename, never user input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_ = f.Close()
		return true
	}
	if errors.Is(err, os.ErrExist) {
		return false
	}
	return true
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
