package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
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
	machineID, err := telemetryMachineID()
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

// spawnAnalyticsHook replaces the detached spawn when non-nil so tests can
// assert how many sends a call performs and what each carries. Production
// leaves it nil. execx.SpawnDetached itself no-ops under `go test`, so counting
// sends needs a seam here rather than a spy on the child.
//
//nolint:gochecknoglobals // test seam, set and restored by in-package tests.
var spawnAnalyticsHook func(payloadJSON string)

// spawnDetachedAnalytics sends the payload from a detached `entire
// __send_analytics` child so the network call never blocks the CLI. The empty
// dir keeps the child out of the parent's working directory. payloadJSON is
// one event object or an array of them — see SendEvents.
func spawnDetachedAnalytics(payloadJSON string) {
	if spawnAnalyticsHook != nil {
		spawnAnalyticsHook(payloadJSON)
		return
	}
	execx.SpawnDetached("", "__send_analytics", payloadJSON)
}

// maxDetachedPayloadBytes bounds the JSON handed to one child. The payload
// travels as an argv element and argv limits are per-platform and modest
// (macOS caps the whole vector near 256KB), so an oversized batch is split
// across children rather than risking a spawn that fails wholesale. Far above
// any realistic batch: one event payload runs a few hundred bytes.
const maxDetachedPayloadBytes = 96 * 1024

// spawnDetachedAnalyticsBatch sends every payload using as few detached
// children as the size budget allows — normally exactly one.
//
// Nothing is dropped: a batch too large for one argv is split, so an unusually
// long backlog costs an extra process instead of lost events. Batching also
// means the child resolves the git version once per send rather than once per
// event.
func spawnDetachedAnalyticsBatch(payloads []*EventPayload) {
	const (
		bracketsBytes = 2 // "[" and "]"
		commaBytes    = 1
	)
	batch := make([]json.RawMessage, 0, len(payloads))
	size := bracketsBytes

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if batchJSON, err := json.Marshal(batch); err == nil {
			spawnDetachedAnalytics(string(batchJSON))
		}
		batch = batch[:0]
		size = bracketsBytes
	}

	for _, payload := range payloads {
		if payload == nil {
			continue
		}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		// Flush before appending so the batch stays within budget. A single
		// payload over the budget still goes out alone: dropping it would be
		// the silent loss this batching exists to remove.
		if len(batch) > 0 && size+len(payloadJSON)+commaBytes > maxDetachedPayloadBytes {
			flush()
		}
		batch = append(batch, payloadJSON)
		size += len(payloadJSON) + commaBytes
	}
	flush()
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

	machineID, err := telemetryMachineID()
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

// SkillInvocation is the content-free view of one recorded skill event: which
// skill fired, on which agent, and how it was detected. Deliberately no prompt
// text, arguments, or transcript content — and the skill name itself is only
// sent verbatim when allowlisted (see skillNameForTelemetry), because custom
// slash-command names are user content too.
type SkillInvocation struct {
	// Skill is the invoked skill's name (e.g. "entire"). Names outside the
	// official allowlist are reported as "custom".
	Skill string
	// Agent is the agent that surfaced the signal (e.g. "claude-code").
	Agent string
	// Signal is the detection signal (e.g. "prompt_slash_command",
	// "skill_tool_use").
	Signal string
	// EventType is the skill event type ("prompt_invocation" or
	// "tool_invocation").
	EventType string
}

// BuildSkillEventPayload constructs the telemetry payload for one skill
// invocation. Exported for testing. Returns nil if the payload cannot be built.
func BuildSkillEventPayload(inv SkillInvocation, isEntireEnabled bool, version string) *EventPayload {
	if inv.Skill == "" {
		return nil
	}

	machineID, err := telemetryMachineID()
	if err != nil {
		return nil
	}

	// Match BuildEventPayload's defaulting so the agent property is always a
	// non-empty, queryable value.
	agentName := inv.Agent
	if agentName == "" {
		agentName = "auto"
	}

	properties := map[string]any{
		"skill":           skillNameForTelemetry(inv.Skill),
		"agent":           agentName,
		"signal":          inv.Signal,
		"event_type":      inv.EventType,
		"isEntireEnabled": isEntireEnabled,
		"cli_version":     version,
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
	}

	return &EventPayload{
		Event:      "cli_skill_invoked",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackSkillInvocationsDetached records skill invocations, batching every
// event into a single detached send (split only if it would exceed
// maxDetachedPayloadBytes). Like TrackPluginDetached, it only honors the env
// opt-out itself — call sites must gate on the user's opt-in telemetry setting.
//
// No per-call cap: an earlier version truncated to 10 events, which silently
// dropped real invocations once condensation began extracting from transcript
// offset 0 — the first condensation of a session drains its whole skill-event
// backlog in one call, and a dropped event is never re-reported because the
// dedupe in session state has already recorded it.
func TrackSkillInvocationsDetached(invocations []SkillInvocation, isEntireEnabled bool, version string) {
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}

	payloads := make([]*EventPayload, 0, len(invocations))
	for _, inv := range invocations {
		if payload := BuildSkillEventPayload(inv, isEntireEnabled, version); payload != nil {
			payloads = append(payloads, payload)
		}
	}
	spawnDetachedAnalyticsBatch(payloads)
}

// SendEvents processes one or more event payloads in the detached subprocess.
// This is called by the hidden __send_analytics command.
//
// The argv carries either a single event object or an array of them; both
// shapes are accepted. Leniency matters beyond convenience: the child is
// re-executed from os.Executable(), so a self-update that replaces the binary
// between spawn and exec can hand a payload to a build other than the one that
// wrote it.
func SendEvents(payloadJSON string) {
	payloads := decodeEventPayloads(payloadJSON)
	if len(payloads) == 0 {
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

	// Resolve the installed git version best-effort, once for the whole batch.
	// A missing or failing git must never block the rest of the telemetry — the
	// property is simply omitted when it can't be determined.
	gitVer := gitVersion(context.Background())

	for _, payload := range payloads {
		props := posthog.NewProperties()
		for k, v := range payload.Properties {
			props.Set(k, v)
		}
		if gitVer != "" {
			props.Set("git_version", gitVer)
		}

		//nolint:errcheck // Best effort telemetry - don't block on result
		_ = client.Enqueue(posthog.Capture{
			DistinctId: payload.DistinctID,
			Event:      payload.Event,
			Properties: props,
			Timestamp:  payload.Timestamp,
		})
	}
}

// decodeEventPayloads parses the argv payload as either an array of events or a
// single event. Malformed input yields no payloads: telemetry is best-effort and
// the detached child has nowhere to report a parse failure.
func decodeEventPayloads(payloadJSON string) []EventPayload {
	trimmed := strings.TrimSpace(payloadJSON)
	if strings.HasPrefix(trimmed, "[") {
		var batch []EventPayload
		if err := json.Unmarshal([]byte(trimmed), &batch); err != nil {
			return nil
		}
		return batch
	}
	var single EventPayload
	if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
		return nil
	}
	return []EventPayload{single}
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
