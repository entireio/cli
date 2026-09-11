package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// describeDispatchRepoNotFound appends, to the cell's cross-jurisdiction 404,
// the one fact the user needs to act on it: which jurisdictions the missing
// repos ARE placed in, from the control plane's repo index. Best-effort — any
// control-plane failure leaves the error as is — and the typed
// *dispatch.RepoNotFoundError stays matchable underneath.
func describeDispatchRepoNotFound(ctx context.Context, err error) error {
	var notFound *dispatchpkg.RepoNotFoundError
	if !errors.As(err, &notFound) || len(notFound.Repos) == 0 {
		return err
	}
	client, clientErr := newCellCoreClient()
	if clientErr != nil {
		logging.Debug(ctx, "dispatch: control plane unavailable for placement hint", "error", clientErr)
		return err
	}
	placements := lookupRepoJurisdictions(ctx, client, notFound.Repos)
	if len(placements) == 0 {
		return err
	}
	// The jurisdiction that just answered "not found" is not a suggestion.
	failed := notFound.FailedJurisdiction()

	var b strings.Builder
	b.WriteString(err.Error())
	for _, repo := range notFound.Repos {
		jurisdictions, ok := placements[repo]
		if !ok {
			continue
		}
		jurisdictions = slices.DeleteFunc(slices.Clone(jurisdictions), func(j string) bool { return j == failed })
		if len(jurisdictions) == 0 {
			fmt.Fprintf(&b, "\n  %s has no ready placement in another jurisdiction", repo)
			continue
		}
		fmt.Fprintf(&b, "\n  %s is placed in: %s", repo, strings.Join(jurisdictions, ", "))
	}
	return &hintError{msg: b.String(), errs: []error{err}}
}

// placementHintTimeout bounds each control-plane lookup behind the hint. It
// runs after the dispatch spinner has been cleared, so the user is looking at
// a blank line until it finishes: shorter than cellResolveTimeout on purpose —
// the hint is a garnish on an error already in hand, not a routing decision.
const placementHintTimeout = 3 * time.Second

// lookupRepoJurisdictions returns, per repo, the sorted jurisdictions of its
// READY placements — the only ones a cell can answer for, so the hint never
// points at a jurisdiction that would 404 again. A repo the control plane does
// not know is absent from the map. Lookups run concurrently, each under its
// own placementHintTimeout, and failures are dropped silently.
func lookupRepoJurisdictions(ctx context.Context, client cellCoreClient, repos []string) map[string][]string {
	results := make([][]string, len(repos))
	found := make([]bool, len(repos))
	var wg sync.WaitGroup
	for i, repo := range repos {
		wg.Go(func() {
			lookupCtx, cancel := context.WithTimeout(ctx, placementHintTimeout)
			defer cancel()
			entry, err := lookupRepoIndexEntry(lookupCtx, client, repo)
			if err != nil {
				logging.Debug(lookupCtx, "dispatch: placement hint lookup failed", "error", err)
				return
			}
			results[i] = readyPlacementJurisdictions(entry.Placements)
			found[i] = true
		})
	}
	wg.Wait()

	out := make(map[string][]string, len(repos))
	for i, repo := range repos {
		if found[i] {
			out[repo] = results[i]
		}
	}
	return out
}

// readyPlacementJurisdictions applies the same slug rule the --jurisdiction
// flag validates with, so every slug the hint offers is one the flag accepts.
func readyPlacementJurisdictions(placements []coreapi.RepoPlacement) []string {
	seen := make(map[string]struct{}, len(placements))
	for _, p := range placements {
		if p.Status != coreapi.RepoPlacementStatusReady {
			continue
		}
		if j, err := auth.NormalizeJurisdiction(p.Jurisdiction); err == nil && j != "" {
			seen[j] = struct{}{}
		}
	}
	jurisdictions := make([]string, 0, len(seen))
	for j := range seen {
		jurisdictions = append(jurisdictions, j)
	}
	sort.Strings(jurisdictions)
	return jurisdictions
}
