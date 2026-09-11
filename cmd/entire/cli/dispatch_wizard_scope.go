package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// dispatchWizardScopeTimeout bounds the control-plane repo index walk behind
// the wizard's jurisdiction picker (the budget code search gives the same
// call); dispatchWizardScopeBudget caps how many index entries it follows.
const (
	dispatchWizardScopeTimeout = 10 * time.Second
	dispatchWizardScopeBudget  = 5000
)

// Seams for the wizard's cloud catalogue, swapped in tests.
var (
	listDispatchWizardPlacements = defaultListDispatchWizardPlacements
	resolveDispatchWizardHome    = defaultResolveDispatchWizardHome
)

// dispatchWizardScope is the wizard's view of where the caller's repos live,
// mirroring the web app's dispatch form: a dispatch covers repos placed in
// exactly one jurisdiction, so the form asks for the jurisdiction first and
// offers only the repos placed there, making a mixed selection unrepresentable.
//
// Everything is precomputed once by newDispatchWizardScope; the form reads it
// on every render. A repo the control plane does not place (or, with no
// placement data at all, every repo) is attributed to home, which is where the
// gateway routes when no selector is sent — under the "" key when home is
// unknown, offered as a plain "Home" choice so the repo stays selectable. Only
// READY placements count — a cell cannot generate from a copy still syncing —
// which deliberately differs from routedRepoPlacement's single elected primary
// (a search-indexing rule).
type dispatchWizardScope struct {
	// repos are the offerable slugs (repos with checkpoints), recent-first.
	repos []string
	// byJurisdiction lists the offerable repos per jurisdiction, in repos order.
	byJurisdiction map[string][]string
	// jurisdictions are the non-empty keys of byJurisdiction, sorted.
	jurisdictions []string
	// defaultJurisdiction is home when the caller has repos there, else the
	// jurisdiction holding the most repos (ties alphabetical), else "".
	defaultJurisdiction string
	home                string
}

// newDispatchWizardScope indexes the catalogue. placements maps a lowercased
// slug to the sorted jurisdictions of its READY placements (nil when the
// control plane could not be asked); home is the caller's home slug or "".
func newDispatchWizardScope(repos []string, placements map[string][]string, home string) *dispatchWizardScope {
	scope := &dispatchWizardScope{repos: repos, byJurisdiction: make(map[string][]string), home: home}
	for _, slug := range repos {
		jurisdictions, ok := placements[strings.ToLower(strings.TrimSpace(slug))]
		if !ok || len(jurisdictions) == 0 {
			jurisdictions = []string{home}
		}
		for _, j := range jurisdictions {
			scope.byJurisdiction[j] = append(scope.byJurisdiction[j], slug)
		}
	}
	scope.jurisdictions = slices.DeleteFunc(slices.Sorted(maps.Keys(scope.byJurisdiction)), func(j string) bool { return j == "" })
	for _, j := range scope.jurisdictions {
		if j == home {
			scope.defaultJurisdiction = j
			break
		}
		if scope.defaultJurisdiction == "" || len(scope.byJurisdiction[j]) > len(scope.byJurisdiction[scope.defaultJurisdiction]) {
			scope.defaultJurisdiction = j
		}
	}
	return scope
}

// reposIn lists the offerable repos in a jurisdiction. "" (or the select's
// dispatchWizardJurisdictionHome sentinel) is the unscoped bucket: repos
// without a placement when home is unknown (every repo, when nothing is placed
// at all).
func (s *dispatchWizardScope) reposIn(jurisdiction string) []string {
	if jurisdiction == dispatchWizardJurisdictionHome {
		jurisdiction = ""
	}
	return s.byJurisdiction[jurisdiction]
}

// options renders the jurisdiction select, default first — huh seeds the bound
// value from the first option, so the ordering IS the default. The unscoped
// "Home" choice (selector unsent) appears when it holds repos, and alone when
// nothing is placed anywhere. Its value is the dispatchWizardJurisdictionHome
// sentinel, never "": huh pre-selects the option equal to the field's current
// value, and the field starts at "", so a ""-valued option would steal the
// default whenever it is listed. resolve() maps the sentinel back to "".
func (s *dispatchWizardScope) options() []huh.Option[string] {
	options := make([]huh.Option[string], 0, len(s.jurisdictions)+1)
	for _, j := range s.jurisdictions {
		label := strings.ToUpper(j)
		if j == s.home {
			label += " (home)"
		}
		option := huh.NewOption(label, j)
		if j == s.defaultJurisdiction {
			options = slices.Insert(options, 0, option)
		} else {
			options = append(options, option)
		}
	}
	if len(s.byJurisdiction[""]) > 0 || len(options) == 0 {
		options = append(options, huh.NewOption("Home", dispatchWizardJurisdictionHome))
	}
	return options
}

// dispatchJurisdictionAccessor is the Jurisdiction select's value accessor. huh
// writes it on the Bubble Tea loop, while the repo picker's OptionsFunc reads
// the chosen jurisdiction from a tea.Cmd goroutine — so the accessor keeps an
// atomic snapshot for that reader, and the plain field for the loop (which is
// also what the repo picker's binding hashes to know when to refresh).
type dispatchJurisdictionAccessor struct {
	state    *dispatchWizardState
	snapshot atomic.Pointer[string]
}

func (a *dispatchJurisdictionAccessor) Get() string { return a.state.jurisdiction }

func (a *dispatchJurisdictionAccessor) Set(value string) {
	a.state.jurisdiction = value
	a.snapshot.Store(&value)
}

// Snapshot is the goroutine-safe read of the last value huh set.
func (a *dispatchJurisdictionAccessor) Snapshot() string {
	if v := a.snapshot.Load(); v != nil {
		return *v
	}
	return ""
}

// loadDispatchWizardScope fetches the three independent sources concurrently:
// the authenticated repo listing (falling back to sibling repos on disk, as
// before), the control-plane placements, and the home jurisdiction. Each is
// best-effort; a missing one degrades to the pre-picker behaviour.
func loadDispatchWizardScope(ctx context.Context, currentRepo string) *dispatchWizardScope {
	var (
		repos      []string
		placements map[string][]string
		home       string
		wg         sync.WaitGroup
	)
	wg.Go(func() {
		slugs, err := listDispatchWizardRepos(ctx)
		if err != nil || len(slugs) == 0 {
			slugs = discoverLocalRepoSlugs(ctx, currentRepo)
		}
		repos = slugs
	})
	wg.Go(func() {
		var err error
		if placements, err = listDispatchWizardPlacements(ctx); err != nil {
			logging.Warn(ctx, "dispatch wizard placements unavailable; offering repos unscoped", "error", err)
		}
	})
	wg.Go(func() { home = resolveDispatchWizardHome(ctx) })
	wg.Wait()
	return newDispatchWizardScope(repos, placements, home)
}

// defaultListDispatchWizardPlacements walks the caller's repo index from the
// control plane, keeping per repo the jurisdictions of its READY placements.
func defaultListDispatchWizardPlacements(ctx context.Context) (map[string][]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dispatchWizardScopeTimeout)
	defer cancel()

	client, err := newCellCoreClient()
	if err != nil {
		return nil, fmt.Errorf("control plane unavailable: %w", err)
	}
	truncated := false
	entries, partial, err := fetchPagesBounded(ctx, dispatchWizardScopeBudget, func(ctx context.Context, cursor string) ([]coreapi.RepoIndexEntry, string, error) {
		params := coreapi.ListReposParams{}
		if cursor != "" {
			params.PageToken = coreapi.NewOptString(cursor)
		}
		out, err := client.ListRepos(ctx, params)
		if err != nil {
			return nil, "", err //nolint:wrapcheck // the caller logs and degrades; no extra context to add
		}
		next := out.NextPageToken.Or("")
		// Truncated with a cursor is just "more pages"; without one the server
		// itself could not reach every repo.
		if out.Truncated && next == "" {
			truncated = true
		}
		return out.Repos, next, nil
	})
	if err != nil {
		return nil, err
	}
	if partial || truncated {
		// Repos beyond the walk are attributed to home; the --jurisdiction
		// flag still reaches them.
		logging.Warn(ctx, "repo index truncated; dispatch wizard may attribute some repos to home")
	}
	out := make(map[string][]string, len(entries))
	for _, entry := range entries {
		if slug := strings.ToLower(strings.TrimSpace(entry.FullName)); slug != "" {
			out[slug] = readyPlacementJurisdictions(entry.Placements)
		}
	}
	return out, nil
}

// defaultResolveDispatchWizardHome reads home_jurisdiction from the same
// account access token the dispatch itself will send, so the picker's default
// and the request's routing agree on which login they mean.
func defaultResolveDispatchWizardHome(ctx context.Context) string {
	token, err := auth.ResolveDataAPIToken(ctx, api.BaseURL())
	if err != nil {
		logging.Debug(ctx, "dispatch wizard: home jurisdiction unavailable", "error", err)
		return ""
	}
	home, err := auth.HomeJurisdictionFromLoginJWT(token)
	if err != nil {
		logging.Debug(ctx, "dispatch wizard: home jurisdiction unavailable", "error", err)
		return ""
	}
	return home
}
