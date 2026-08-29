package telemetry

import (
	"encoding/json"
	"runtime"
	"time"
)

// Search modes for the cli_search_completed event.
const (
	SearchModeCheckpoint = "checkpoint"
	SearchModeCode       = "code"
)

// Error classes for the cli_search_completed event. Classification is derived
// from typed errors only (see classifySearchError in the cli package), never
// from matching error message text.
const (
	// SearchErrClassAuth: not logged in, or a cell/control-plane rejected the
	// bearer (401/403).
	SearchErrClassAuth = "auth"
	// SearchErrClassCellSkip: client-side skip — no entire-api cell is
	// configured for the placement's jurisdiction (auth.ErrNoCellForJurisdiction).
	SearchErrClassCellSkip = "cell_skip"
	// SearchErrClassRegionUnavailable: the cell was contacted but its gateway
	// has no query-serve route (search.ErrCellUnavailable). Distinct from
	// SearchErrClassCellSkip — these are the two "region" failure variants.
	SearchErrClassRegionUnavailable = "region_unavailable"
	// SearchErrClassRepoUnavailable: cells answered but the repo is not
	// searchable (not indexed, or not enabled for semantic search).
	SearchErrClassRepoUnavailable = "repo_unavailable"
	// SearchErrClassNetwork: network failure or timeout.
	SearchErrClassNetwork = "network"
	// SearchErrClassServer: a 5xx response, or a 200 whose body was unusable
	// (undecodable, or carrying an application-level error field).
	SearchErrClassServer = "server"
	// SearchErrClassHTTPOther: a non-5xx, non-auth HTTP error status.
	SearchErrClassHTTPOther = "http_other"
	// SearchErrClassOther: everything else.
	SearchErrClassOther = "other"
)

// SearchOutcome carries the outcome of one search request (ENT-1938).
// Content-free by construction: booleans, enums, counts, and durations only —
// never query text, result snippets, or repo names. Success is derived —
// ErrorClass empty means success — so the two can never disagree.
type SearchOutcome struct {
	// Command is the invoked cobra command path ("entire search" or
	// "entire checkpoint search").
	Command string
	// Mode is SearchModeCheckpoint or SearchModeCode.
	Mode string
	// ErrorClass is the coarse failure class (SearchErrClass*); empty means
	// the search succeeded.
	ErrorClass string
	// ResultCount is the number of results returned; only meaningful on
	// success (zero results is a distinct signal from failure) and omitted
	// from the payload on failure.
	ResultCount int
	// CoverageIncomplete reports that a successful response warned results
	// may be missing (failed regions, skipped repos, truncated index) —
	// without it a degraded success is indistinguishable from a genuine
	// zero-or-low-result search. Omitted from the payload on failure.
	CoverageIncomplete bool
	// DurationMS is the wall-clock duration of the search request.
	DurationMS int64
}

// BuildSearchOutcomePayload constructs the cli_search_completed payload.
// Exported for testing. Returns nil if the machine ID cannot be resolved.
func BuildSearchOutcomePayload(outcome SearchOutcome, isEntireEnabled bool, version string) *EventPayload {
	machineID, err := telemetryMachineID()
	if err != nil {
		return nil
	}

	success := outcome.ErrorClass == ""
	properties := map[string]any{
		"command":         outcome.Command,
		"mode":            outcome.Mode,
		"success":         success,
		"duration_ms":     outcome.DurationMS,
		"isEntireEnabled": isEntireEnabled,
		"cli_version":     version,
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
	}
	if success {
		properties["result_count"] = outcome.ResultCount
		properties["coverage_incomplete"] = outcome.CoverageIncomplete
	} else {
		properties["error_class"] = outcome.ErrorClass
	}

	return &EventPayload{
		Event:      "cli_search_completed",
		DistinctID: machineID,
		Properties: properties,
		Timestamp:  time.Now(),
	}
}

// TrackSearchOutcomeDetached records one search request's outcome by spawning
// a detached subprocess. Best-effort and non-blocking; call sites must gate on
// the user's opt-in telemetry setting. Honors ENTIRE_TELEMETRY_OPTOUT like the
// other trackers.
func TrackSearchOutcomeDetached(outcome SearchOutcome, isEntireEnabled bool, version string) {
	if IsEnvOptedOut() {
		return
	}

	payload := BuildSearchOutcomePayload(outcome, isEntireEnabled, version)
	if payload == nil {
		return
	}

	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}
