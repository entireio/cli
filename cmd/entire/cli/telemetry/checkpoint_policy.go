package telemetry

import (
	"encoding/json"
	"os"
	"runtime"
	"time"

	"github.com/denisbrodbeck/machineid"
)

// Property values for the checkpoint_policy_blocked event.
const (
	PolicyBlockedHookTypeGit   = "git"
	PolicyBlockedHookTypeAgent = "agent"

	PolicyBlockedReasonUnsupported = "policy_unsupported"
	PolicyBlockedReasonUnreadable  = "policy_unreadable"

	PolicyBlockedOutcomeSkipped = "skipped"
	PolicyBlockedOutcomeBlocked = "blocked"
)

// CheckpointPolicyBlockedEvent carries the gate-derived fields for a
// checkpoint_policy_blocked telemetry event. Agent, CheckpointVersion, and
// CheckpointMinVersion are optional and omitted from the payload when empty.
type CheckpointPolicyBlockedEvent struct {
	Hook                 string
	HookType             string
	Reason               string
	Outcome              string
	Agent                string
	CheckpointVersion    string
	CheckpointMinVersion string
}

// BuildCheckpointPolicyBlockedPayload constructs the event payload. Exported for
// testing. Returns nil if the machine ID cannot be resolved.
func BuildCheckpointPolicyBlockedPayload(event CheckpointPolicyBlockedEvent, version string) *EventPayload {
	machineID, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return nil
	}

	properties := map[string]any{
		"hook":        event.Hook,
		"hook_type":   event.HookType,
		"reason":      event.Reason,
		"outcome":     event.Outcome,
		"cli_version": version,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
	}
	if event.Agent != "" {
		properties["agent"] = event.Agent
	}
	if event.CheckpointVersion != "" {
		properties["checkpoint_version"] = event.CheckpointVersion
	}
	if event.CheckpointMinVersion != "" {
		properties["checkpoint_min_version"] = event.CheckpointMinVersion
	}

	return &EventPayload{
		Event:      "checkpoint_policy_blocked",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackCheckpointPolicyBlocked sends a checkpoint_policy_blocked event by
// spawning a detached subprocess. Best-effort and non-blocking; callers are
// responsible for the settings.Telemetry opt-in check. Honors
// ENTIRE_TELEMETRY_OPTOUT like the other trackers.
func TrackCheckpointPolicyBlocked(event CheckpointPolicyBlockedEvent, version string) {
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}

	payload := BuildCheckpointPolicyBlockedPayload(event, version)
	if payload == nil {
		return
	}

	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}
