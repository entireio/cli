package cli

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// classifySearchError maps a search failure to a coarse telemetry error class
// (ENT-1938). Classification uses typed errors only — never error message
// text. Sentinel checks run before status-code checks so a mapped failure
// (e.g. the gateway 404 behind search.ErrCellUnavailable) keeps its specific
// class.
func classifySearchError(err error) string {
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return telemetry.SearchErrClassAuth
	case errors.Is(err, auth.ErrNoCellForJurisdiction):
		return telemetry.SearchErrClassCellSkip
	case errors.Is(err, search.ErrCellUnavailable), errors.Is(err, errNoRegionAvailable):
		return telemetry.SearchErrClassRegionUnavailable
	case errors.Is(err, search.ErrRepoFilterUnmatched), errors.Is(err, errNoRepoAvailable):
		return telemetry.SearchErrClassRepoUnavailable
	}

	var statusErr *search.HTTPStatusError
	if errors.As(err, &statusErr) {
		return classForHTTPStatus(statusErr.StatusCode)
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return classForHTTPStatus(httpErr.StatusCode)
	}
	// A 200 whose body was unusable is the service misbehaving, not an
	// unclassifiable client-side failure.
	var malformedErr *search.MalformedResponseError
	if errors.As(err, &malformedErr) {
		return telemetry.SearchErrClassServer
	}

	// isRecapNetworkError deliberately excludes context cancellation, so a
	// user's Ctrl-C never inflates the network-failure rate; a timeout is a
	// network outcome, so DeadlineExceeded is re-added explicitly.
	if errors.Is(err, context.DeadlineExceeded) || isRecapNetworkError(err) {
		return telemetry.SearchErrClassNetwork
	}

	return telemetry.SearchErrClassOther
}

func classForHTTPStatus(status int) string {
	switch {
	case status >= http.StatusInternalServerError:
		return telemetry.SearchErrClassServer
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return telemetry.SearchErrClassAuth
	default:
		return telemetry.SearchErrClassHTTPOther
	}
}

// searchOutcomeResult is the successful-response summary a call site hands to
// emitSearchOutcome; ignored when err is non-nil.
type searchOutcomeResult struct {
	count int
	// coverageIncomplete records that the response warned results may be
	// missing (failed regions, skipped repos, truncated index), so dashboards
	// can tell a genuine zero-result search from a degraded one.
	coverageIncomplete bool
}

// emitSearchOutcome reports one search request's outcome when telemetry is
// opted in (settings.Telemetry == true). Content-free: booleans, enums,
// counts, and durations only — never query text, results, or repo names.
// Best-effort and non-blocking; failures to load settings suppress the event.
func emitSearchOutcome(ctx context.Context, command, mode string, result searchOutcomeResult, duration time.Duration, err error) {
	if telemetry.IsEnvOptedOut() {
		return
	}
	s, loadErr := LoadEntireSettings(ctx)
	if loadErr != nil || !s.IsTelemetryEnabled() {
		return
	}

	outcome := telemetry.SearchOutcome{
		Command:    command,
		Mode:       mode,
		DurationMS: duration.Milliseconds(),
	}
	if err != nil {
		outcome.ErrorClass = classifySearchError(err)
	} else {
		outcome.ResultCount = result.count
		outcome.CoverageIncomplete = result.coverageIncomplete
	}
	telemetry.TrackSearchOutcomeDetached(outcome, s.Enabled, versioninfo.Version)
}

// instrumentSemanticSearcher wraps a semanticSearcher so EVERY invocation —
// the one-shot command, the TUI's initial fetch, interactive re-searches, and
// pagination — emits one cli_search_completed outcome. Instrumenting the seam
// rather than individual call sites means new entry points are covered by
// construction, and the timer wraps only the request, never TUI dwell time.
// command is the invoking cobra command path, fixed at wrap time because the
// TUI calls the searcher long after the command layer returns.
func instrumentSemanticSearcher(command string, searcher semanticSearcher) semanticSearcher {
	return func(ctx context.Context, cfg search.Config) (*search.Response, error) {
		start := time.Now()
		resp, err := searcher(ctx, cfg)
		var result searchOutcomeResult
		if resp != nil {
			result = searchOutcomeResult{count: len(resp.Results), coverageIncomplete: resp.CoverageIncomplete}
		}
		emitSearchOutcome(ctx, command, telemetry.SearchModeCheckpoint, result, time.Since(start), err)
		return resp, err
	}
}

// emitSearchSelection reports that the user opened one search result, when
// telemetry is opted in (settings.Telemetry == true). Content-free: enums,
// counts, and the selected result's rank only — never query text, result
// titles, repo names, or file paths. Best-effort and non-blocking; failures to
// load settings suppress the event.
//
// Gating is deliberately identical to emitSearchOutcome's, so a user opted out
// of one search event is opted out of both.
func emitSearchSelection(ctx context.Context, selection telemetry.SearchSelection) {
	if telemetry.IsEnvOptedOut() {
		return
	}
	s, loadErr := LoadEntireSettings(ctx)
	if loadErr != nil || !s.IsTelemetryEnabled() {
		return
	}

	telemetry.TrackSearchSelectionDetached(selection, s.Enabled, versioninfo.Version)
}

// searchSelectionResultType clamps a semantic result's type to the vocabulary
// the search API is known to emit, mapping anything else to "other".
//
// This is not defensive padding: search.Result.UnmarshalJSON assigns
// raw.Type verbatim and its default branch accepts types it does not model, so
// an unrecognized string from a newer (or misbehaving) server would otherwise
// reach PostHog as a property value. Clamping is what makes SearchSelection's
// "content-free by construction" claim true here rather than a matter of
// trusting the server, and it keeps the property's cardinality bounded. A new
// result type added server-side lands in "other" until it is added below.
func searchSelectionResultType(resultType string) string {
	switch resultType {
	case search.TypeCheckpoint, search.TypeCommit, search.TypeSession, search.TypeRepo, search.TypePR:
		return resultType
	default:
		return telemetry.SearchSelectionTypeOther
	}
}
