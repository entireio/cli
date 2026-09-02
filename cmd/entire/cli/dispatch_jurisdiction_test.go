package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/internal/coreapi"
)

// These tests swap the package-level newCellCoreClient seam (via
// withFakeCellCore) and therefore must not run in parallel.

func TestDescribeDispatchRepoNotFound_AppendsPlacementHintAndKeepsType(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{{
		FullName: "entirehq/ferrata",
		Placements: []coreapi.RepoPlacement{
			{ID: "p1", Jurisdiction: "US", Status: coreapi.RepoPlacementStatusReady},
			{ID: "p2", Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady},
			{ID: "p3", Jurisdiction: "eu", Status: coreapi.RepoPlacementStatusProcessing},
			{ID: "p4", Jurisdiction: "au", Status: coreapi.RepoPlacementStatusReady},
			{ID: "p5", Jurisdiction: "not a label", Status: coreapi.RepoPlacementStatusReady},
		},
	}}}})

	original := &dispatchpkg.RepoNotFoundError{
		Jurisdiction: "au",
		Repos:        []string{"entirehq/ferrata"},
		Message:      "repository not found: entirehq/ferrata",
	}
	err := describeDispatchRepoNotFound(context.Background(), original)
	msg := err.Error()
	if !strings.HasPrefix(msg, "In AU: repository not found: entirehq/ferrata.") {
		t.Fatalf("expected the dispatch error to lead, got %q", msg)
	}
	// Deduped, ready-only, flag-valid, and never the jurisdiction that just
	// failed: the processing EU copy, the malformed slug and AU are not offered.
	if !strings.HasSuffix(msg, "\n  entirehq/ferrata is placed in: us") {
		t.Fatalf("expected placement hint, got %q", msg)
	}
	var notFound *dispatchpkg.RepoNotFoundError
	if !errors.As(err, &notFound) || notFound != original {
		t.Fatalf("the hint must keep the typed error matchable, got %T", err)
	}
}

func TestDescribeDispatchRepoNotFound_NoReadyPlacementElsewhereSaysSo(t *testing.T) {
	// Home path (no selector): the home jurisdiction is the one that failed,
	// so a repo READY only at home has nothing else to offer. The fake returns
	// the same row for every filter; the identity check drops the mismatch.
	withFakeCellCore(t, &fakeCellCore{repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{{
		FullName:   "entirehq/plans",
		Placements: []coreapi.RepoPlacement{{ID: "p1", Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady}},
	}}}})

	err := describeDispatchRepoNotFound(context.Background(), &dispatchpkg.RepoNotFoundError{
		Home:    "us",
		Repos:   []string{"entirehq/ferrata", "entirehq/plans"},
		Message: "repository not found: entirehq/ferrata, entirehq/plans",
	})
	msg := err.Error()
	if !strings.Contains(msg, "\n  entirehq/plans has no ready placement in another jurisdiction") {
		t.Fatalf("expected no-placement hint, got %q", msg)
	}
	if strings.Contains(msg, "entirehq/ferrata is placed in") || strings.Contains(msg, "entirehq/ferrata has no") {
		t.Fatalf("a repo the index does not name must not get a hint, got %q", msg)
	}
}

func TestDescribeDispatchRepoNotFound_LeavesErrorAloneWithoutHint(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("core down")})

	original := &dispatchpkg.RepoNotFoundError{Jurisdiction: "us", Repos: []string{"a/b"}, Message: "repository not found: a/b"}
	if err := describeDispatchRepoNotFound(context.Background(), original); !errors.Is(err, original) || err.Error() != original.Error() {
		t.Fatalf("expected the original error untouched on control-plane failure, got %v", err)
	}

	prev := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return nil, errors.New("no client") }
	t.Cleanup(func() { newCellCoreClient = prev })
	if err := describeDispatchRepoNotFound(context.Background(), original); !errors.Is(err, original) || err.Error() != original.Error() {
		t.Fatalf("expected the original error untouched when no client, got %v", err)
	}

	other := errors.New("boom")
	if got := describeDispatchRepoNotFound(context.Background(), other); !errors.Is(got, other) {
		t.Fatalf("non-404 errors must pass through untouched, got %v", got)
	}
}
