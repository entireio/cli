package telemetry

import (
	"encoding/json"
	"runtime"
	"time"
)

// SearchSelectionTypeCode is the result_type reported for a code-search hit.
// Semantic results carry their own type from the search API (checkpoint,
// commit, session, ...); code results have no such field, so they are
// reported under this single kind.
const SearchSelectionTypeCode = "code"

// SearchSelectionTypeOther is the result_type reported for a semantic result
// whose type is outside the vocabulary the CLI models. The search API's
// decoder passes unrecognized types through verbatim, so the cli package
// clamps to this rather than forwarding an arbitrary server-supplied string
// (see searchSelectionResultType).
const SearchSelectionTypeOther = "other"

// SearchSelection carries one act of the user opening a search result
// (ENT-2073) — the CLI counterpart of the web app's search_result_clicked.
// Without it, cli_search_completed can say a search returned results but not
// whether any of them were worth opening, which is the click-through half of
// "is search any good?".
//
// Content-free by construction, exactly like SearchOutcome: enums, counts,
// and positions only — never query text, result titles, repo names, file
// paths, or snippets.
type SearchSelection struct {
	// Command is the invoked cobra command path ("entire search").
	Command string
	// Mode is SearchModeCheckpoint or SearchModeCode.
	Mode string
	// ResultType is the selected result's kind — the search API's own enum
	// (search.TypeCheckpoint, TypeCommit, TypeSession, ...) for semantic
	// results, or SearchSelectionTypeCode for code hits. A fixed vocabulary,
	// never user content.
	ResultType string
	// Rank is the selected result's zero-based position in the list the user
	// was looking at. This is the core relevance signal: a healthy ranker is
	// selected at rank 0 far more often than at rank 20.
	Rank int
	// ResultCount is how many results were loaded in the active tab when the
	// selection happened, so Rank can be read against the size of the list it
	// indexes into.
	ResultCount int
}

// BuildSearchSelectionPayload constructs the cli_search_result_selected
// payload. Exported for testing. Returns nil if the machine ID cannot be
// resolved.
func BuildSearchSelectionPayload(selection SearchSelection, isEntireEnabled bool, version string) *EventPayload {
	machineID, err := telemetryMachineID()
	if err != nil {
		return nil
	}

	return &EventPayload{
		Event:      "cli_search_result_selected",
		DistinctID: machineID,
		Properties: map[string]any{
			"command":         selection.Command,
			"mode":            selection.Mode,
			"result_type":     selection.ResultType,
			"rank":            selection.Rank,
			"result_count":    selection.ResultCount,
			"isEntireEnabled": isEntireEnabled,
			"cli_version":     version,
			"os":              runtime.GOOS,
			"arch":            runtime.GOARCH,
		},
		Timestamp: time.Now(),
	}
}

// TrackSearchSelectionDetached records one result selection by spawning a
// detached subprocess. Best-effort and non-blocking; call sites must gate on
// the user's opt-in telemetry setting. Honors ENTIRE_TELEMETRY_OPTOUT like the
// other trackers.
func TrackSearchSelectionDetached(selection SearchSelection, isEntireEnabled bool, version string) {
	if IsEnvOptedOut() {
		return
	}

	payload := BuildSearchSelectionPayload(selection, isEntireEnabled, version)
	if payload == nil {
		return
	}

	if payloadJSON, err := json.Marshal(payload); err == nil {
		spawnDetachedAnalytics(string(payloadJSON))
	}
}
