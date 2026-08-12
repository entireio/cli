package telemetry

import (
	"encoding/json"
	"os"
	"runtime"
	"time"

	"github.com/denisbrodbeck/machineid"
)

// Source values for the cli_checkpoint_explained event, one per explain
// id-resolution path.
const (
	ExplainSourceProse     = "prose"
	ExplainSourceExport    = "export"
	ExplainSourceCrossRepo = "cross_repo"
)

// SearchPerformedEvent carries the fields for a cli_search_performed event
// (ENT-1528). QueryHash is a non-reversible fingerprint — the raw query is
// never sent.
type SearchPerformedEvent struct {
	SearchID    string
	QueryHash   string
	Mode        string // "json" or "compact"
	ResultCount int    // results served on this page
	Total       int    // total results before client-side pagination
	Page        int
	Limit       int
}

// CheckpointExplainedEvent carries the fields for a cli_checkpoint_explained
// event (ENT-1528): a successful `entire checkpoint explain` id resolution,
// the "click" that follows a search impression. CheckpointID is an opaque
// identifier (ULID or 12-hex); Source is one of the ExplainSource* constants.
// SearchID and Rank come from the --search-id token embedded in compact
// search hints; when present they link the click to its search
// deterministically. Both optional (empty/zero = omitted).
type CheckpointExplainedEvent struct {
	CheckpointID string
	Source       string
	SearchID     string
	Rank         int
}

// BuildSearchPerformedPayload constructs a cli_search_performed event payload.
// Exported for testing. Returns nil if the machine ID cannot be resolved.
func BuildSearchPerformedPayload(event SearchPerformedEvent, version string) *EventPayload {
	machineID, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return nil
	}
	return &EventPayload{
		Event:      "cli_search_performed",
		DistinctID: machineID,
		Properties: map[string]any{
			"search_id":    event.SearchID,
			"query_hash":   event.QueryHash,
			"mode":         event.Mode,
			"result_count": event.ResultCount,
			"total":        event.Total,
			"page":         event.Page,
			"limit":        event.Limit,
			"cli_version":  version,
			"os":           runtime.GOOS,
			"arch":         runtime.GOARCH,
		},
		Timestamp: time.Now(),
	}
}

// TrackSearchPerformed sends a cli_search_performed event by spawning a
// detached subprocess. Best-effort and non-blocking; callers are responsible
// for the settings.Telemetry opt-in check. Honors ENTIRE_TELEMETRY_OPTOUT
// like the other trackers.
func TrackSearchPerformed(event SearchPerformedEvent, version string) {
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}
	payload := BuildSearchPerformedPayload(event, version)
	if payload == nil {
		return
	}
	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}

// BuildCheckpointExplainedPayload constructs a cli_checkpoint_explained event
// payload. Exported for testing. Returns nil if the machine ID cannot be
// resolved.
func BuildCheckpointExplainedPayload(event CheckpointExplainedEvent, version string) *EventPayload {
	machineID, err := machineid.ProtectedID("entire-cli")
	if err != nil {
		return nil
	}
	properties := map[string]any{
		"checkpoint_id": event.CheckpointID,
		"source":        event.Source,
		"cli_version":   version,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
	}
	if event.SearchID != "" {
		properties["search_id"] = event.SearchID
		if event.Rank > 0 {
			properties["rank"] = event.Rank
		}
	}
	return &EventPayload{
		Event:      "cli_checkpoint_explained",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackCheckpointExplained sends a cli_checkpoint_explained event by spawning
// a detached subprocess. Best-effort and non-blocking; callers are responsible
// for the settings.Telemetry opt-in check. Honors ENTIRE_TELEMETRY_OPTOUT
// like the other trackers.
func TrackCheckpointExplained(event CheckpointExplainedEvent, version string) {
	if os.Getenv("ENTIRE_TELEMETRY_OPTOUT") != "" {
		return
	}
	payload := BuildCheckpointExplainedPayload(event, version)
	if payload == nil {
		return
	}
	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}
