package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
)

// The wrapping in each case mirrors how the real search paths build their
// error chains (loginHintErr, classifySemanticCells, fmt.Errorf wrappers).
func TestClassifySearchError(t *testing.T) {
	t.Parallel()
	regionSkip := func(causes ...error) error {
		return fmt.Errorf("semantic search: %w", &hintError{
			msg:  errNoRegionAvailable.Error(),
			errs: causes,
		})
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not logged in via login hint", fmt.Errorf("search failed: %w", loginHintErr(auth.ErrNotLoggedIn)), telemetry.SearchErrClassAuth},
		{"jurisdiction cell skip", regionSkip(auth.ErrNoCellForJurisdiction), telemetry.SearchErrClassCellSkip},
		{"gateway without query-serve", regionSkip(search.ErrCellUnavailable), telemetry.SearchErrClassRegionUnavailable},
		{"bare region sentinel", fmt.Errorf("semantic search: %w", errNoRegionAvailable), telemetry.SearchErrClassRegionUnavailable},
		{"repo filter unmatched", fmt.Errorf("semantic search: %w", search.ErrRepoFilterUnmatched), telemetry.SearchErrClassRepoUnavailable},
		{"repo not indexed", fmt.Errorf("semantic search: %w", errNoRepoAvailable), telemetry.SearchErrClassRepoUnavailable},
		{"search service 5xx", &search.HTTPStatusError{StatusCode: 502, Message: "search service returned 502"}, telemetry.SearchErrClassServer},
		{"search service 401", &search.HTTPStatusError{StatusCode: 401, Message: "search service error (401): Invalid token"}, telemetry.SearchErrClassAuth},
		{"malformed 200 body", fmt.Errorf("semantic search: %w", &search.MalformedResponseError{Message: "unexpected response from search service: <html>"}), telemetry.SearchErrClassServer},
		{"error field in 200 body", fmt.Errorf("semantic search: %w", &search.MalformedResponseError{Message: "search service error: index rebuilding"}), telemetry.SearchErrClassServer},
		{"code search 5xx", fmt.Errorf("code search: %w", &api.HTTPError{StatusCode: 500}), telemetry.SearchErrClassServer},
		{"code search 404", fmt.Errorf("code search: %w", &api.HTTPError{StatusCode: 404}), telemetry.SearchErrClassHTTPOther},
		{"deadline exceeded", fmt.Errorf("calling search service: %w", context.DeadlineExceeded), telemetry.SearchErrClassNetwork},
		{"network error", fmt.Errorf("calling search service: %w", &url.Error{Op: "Get", URL: "https://example.test", Err: errors.New("connection refused")}), telemetry.SearchErrClassNetwork},
		// Ctrl-C must never inflate the network-failure rate.
		{"user cancellation", fmt.Errorf("calling search service: %w", &url.Error{Op: "Get", URL: "https://example.test", Err: context.Canceled}), telemetry.SearchErrClassOther},
		{"unclassified", errors.New("boom"), telemetry.SearchErrClassOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifySearchError(tc.err); got != tc.want {
				t.Errorf("classifySearchError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// Pins the classifySemanticCells → classifySearchError contract end-to-end:
// the all-cells-skipped error must keep every typed skip cause so the two
// "region" variants stay distinguishable, deterministically even when a
// fan-out mixes both causes (cell order must not decide the class).
func TestClassifySemanticCells_SkipCausesClassify(t *testing.T) {
	t.Parallel()
	cell := func(name string, err error) cellCallResult[*search.Response] {
		return cellCallResult[*search.Response]{group: cellGroup{cell: name}, err: err}
	}
	cases := []struct {
		name    string
		results []cellCallResult[*search.Response]
		want    string
	}{
		{"all jurisdiction skips", []cellCallResult[*search.Response]{cell("a", auth.ErrNoCellForJurisdiction)}, telemetry.SearchErrClassCellSkip},
		{"all gateway skips", []cellCallResult[*search.Response]{cell("a", search.ErrCellUnavailable)}, telemetry.SearchErrClassRegionUnavailable},
		{"mixed causes, jurisdiction last", []cellCallResult[*search.Response]{cell("a", search.ErrCellUnavailable), cell("b", auth.ErrNoCellForJurisdiction)}, telemetry.SearchErrClassCellSkip},
		{"mixed causes, gateway last", []cellCallResult[*search.Response]{cell("a", auth.ErrNoCellForJurisdiction), cell("b", search.ErrCellUnavailable)}, telemetry.SearchErrClassCellSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pages, _, lastErr := classifySemanticCells(context.Background(), tc.results)
			if len(pages) != 0 || lastErr == nil {
				t.Fatalf("pages = %d, lastErr = %v; want no pages and an error", len(pages), lastErr)
			}
			if got := lastErr.Error(); got != errNoRegionAvailable.Error() {
				t.Errorf("user-facing message = %q, want %q", got, errNoRegionAvailable.Error())
			}
			if got := classifySearchError(lastErr); got != tc.want {
				t.Errorf("classifySearchError = %q, want %q", got, tc.want)
			}
		})
	}
}

// hintError must swap the user-facing message without truncating the typed
// chain telemetry classifies from.
func TestHintErrorPreservesMessageAndChain(t *testing.T) {
	t.Parallel()
	err := loginHintErr(fmt.Errorf("resolving control-plane client: %w", auth.ErrNotLoggedIn))
	if got := err.Error(); got != "not authenticated. Run 'entire login' to authenticate" {
		t.Errorf("Error() = %q, want the login hint verbatim", got)
	}
	if !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Error("errors.Is(err, auth.ErrNotLoggedIn) = false, want true")
	}
}
