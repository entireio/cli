package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/oklog/ulid/v2"
)

// Search relevance telemetry (ENT-1528). Agent-driven search has no web-style
// click signal, so two stateless events reconstruct the funnel in PostHog:
// cli_search_performed (a search served results) and cli_checkpoint_explained
// (an agent fetched one result's full detail — the "click"). Correlation
// happens entirely in the analytics layer by distinct_id + time; nothing is
// persisted locally.

// newSearchID mints a client-side search identifier. The search server
// response carries no request id (and the CLI merges several cell responses
// into one page), so the id that ties a response to its telemetry event is
// minted here. Returns "" on entropy failure; callers treat that as
// "tracking off".
func newSearchID() string {
	u, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return ""
	}
	return u.String()
}

// hashSearchQuery returns a short, non-reversible fingerprint of the query
// for telemetry: SHA-256 of the lowercased, whitespace-collapsed query,
// truncated to 16 hex chars. Raw query text is never sent (privacy
// convention: operational metadata only).
func hashSearchQuery(query string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(query), " "))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

// emitSearchPerformed reports a cli_search_performed telemetry event when
// telemetry is opted in (settings.Telemetry == true). Best-effort and
// non-blocking; search output has already been written, so failures simply
// suppress the event.
func emitSearchPerformed(ctx context.Context, searchID, query, mode string, resultCount, total, page, limit int) {
	if searchID == "" {
		return
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil || s.Telemetry == nil || !*s.Telemetry {
		return
	}
	telemetry.TrackSearchPerformed(telemetry.SearchPerformedEvent{
		SearchID:    searchID,
		QueryHash:   hashSearchQuery(query),
		Mode:        mode,
		ResultCount: resultCount,
		Total:       total,
		Page:        page,
		Limit:       limit,
	}, versioninfo.Version)
}

// emitCheckpointExplained reports a cli_checkpoint_explained telemetry event
// when telemetry is opted in — the "click" that follows a search impression
// (ENT-1528). Zero agent cooperation: it fires on every successful explain
// id resolution, and the search→explain funnel is assembled in the analytics
// layer. Best-effort and non-blocking; explain output is never affected.
func emitCheckpointExplained(ctx context.Context, checkpointID, source string) {
	if checkpointID == "" {
		return
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil || s.Telemetry == nil || !*s.Telemetry {
		return
	}
	telemetry.TrackCheckpointExplained(telemetry.CheckpointExplainedEvent{
		CheckpointID: checkpointID,
		Source:       source,
	}, versioninfo.Version)
}
