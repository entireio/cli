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
// (ENT-1528). Outcome distinguishes a served response ("ok") from a request
// that failed before any results existed ("error_request", "error_auth",
// ...); on an error path the count fields are zero. FetchedCount and Total
// are deliberately distinct: FetchedCount is what this page served
// client-side, Total is the server's true match count (resp.Total) — never
// derived from len(results), so a filtered/short page doesn't masquerade as
// "few matches". Page and Limit are the normalized values the envelope
// reports (paginateSearchResults), not the raw --page/--limit flags.
type SearchPerformedEvent struct {
	SearchID     string
	Mode         string // "json" | "compact"
	Outcome      string // "ok" | "error_request" | "error_auth" | ...
	QueryLength  int    // rune count of the query text, never the text itself
	FetchedCount int    // results served on this page (client-side)
	Total        int    // server's true total match count (resp.Total)
	ZeroResults  bool
	Page, Limit  int // normalized values the envelope reports
	AllRepos     bool
	FilterAuthor bool
	FilterDate   bool
	FilterBranch bool
	FilterRepo   bool
	Reranked     bool
	TotalMS      int
	Degraded     bool // response carried completeness warnings
}

// CheckpointExplainedEvent carries the fields for a cli_checkpoint_explained
// event (ENT-1528): a successful `entire checkpoint explain` id resolution,
// the "click" that follows a search impression. DocRef is a truncated
// SHA-256 of the checkpoint id — the raw id itself never reaches analytics
// (trail 1019 high finding: "drop or hash"). Source is one of the
// ExplainSource* constants. SearchID and Rank come from the --search-id
// token embedded in compact search hints; when present they link the click
// to its search deterministically. Both optional (empty/zero = omitted).
type CheckpointExplainedEvent struct {
	DocRef   string
	Source   string
	SearchID string
	Rank     int
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
			"search_id":     event.SearchID,
			"mode":          event.Mode,
			"outcome":       event.Outcome,
			"query_length":  event.QueryLength,
			"fetched_count": event.FetchedCount,
			"total":         event.Total,
			"zero_results":  event.ZeroResults,
			"page":          event.Page,
			"limit":         event.Limit,
			"all_repos":     event.AllRepos,
			"f_author":      event.FilterAuthor,
			"f_date":        event.FilterDate,
			"f_branch":      event.FilterBranch,
			"f_repo":        event.FilterRepo,
			"reranked":      event.Reranked,
			"total_ms":      event.TotalMS,
			"degraded":      event.Degraded,
			"cli_version":   version,
			"os":            runtime.GOOS,
			"arch":          runtime.GOARCH,
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
		"doc_ref":     event.DocRef,
		"source":      event.Source,
		"cli_version": version,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
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
