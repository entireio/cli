package cli

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// Seams for the wizard's cloud catalogue, swapped in tests.
var (
	listDispatchWizardIndex   = listCheckpointRepoIndex
	resolveDispatchWizardHome = defaultResolveDispatchWizardHome
)

// dispatchWizardScope is the wizard's view of where the caller's repos live,
// mirroring the web app's dispatch form: a dispatch covers repos placed in
// exactly one jurisdiction, so the form asks for the jurisdiction first and
// offers only the repos placed there, making a mixed selection unrepresentable.
//
// Everything is precomputed once by newDispatchWizardScope; the form reads it
// on every render. A repo the control plane does not place (or, with no
// placement data at all, every repo) is attributed to home, which is where the
// dispatch routes when no --jurisdiction is given — under the "" key when home is
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

// loadDispatchWizardScope fetches the repo index and home concurrently. One
// index walk supplies both picker contents and placements; an unavailable or
// empty catalogue falls back to sibling repos on disk.
func loadDispatchWizardScope(ctx context.Context, currentRepo string) *dispatchWizardScope {
	var (
		repos      []string
		placements map[string][]string
		home       string
		wg         sync.WaitGroup
	)
	wg.Go(func() {
		entries, err := listDispatchWizardIndex(ctx)
		if err != nil {
			logging.Warn(ctx, "dispatch wizard repo index unavailable; offering local repos", "error", err)
		}
		repos = checkpointRepoSlugs(entries)
		placements = dispatchWizardPlacements(entries)
		if err != nil || len(repos) == 0 {
			repos = discoverLocalRepoSlugs(ctx, currentRepo)
		}
	})
	wg.Go(func() { home = resolveDispatchWizardHome(ctx) })
	wg.Wait()
	return newDispatchWizardScope(repos, placements, home)
}

// dispatchWizardPlacements keeps the READY jurisdictions keyed by picker slug.
func dispatchWizardPlacements(entries []coreapi.RepoIndexEntry) map[string][]string {
	out := make(map[string][]string, len(entries))
	for _, entry := range entries {
		if slug := checkpointRepoSlug(entry); slug != "" {
			key := strings.ToLower(slug)
			jurisdictions := out[key]
			jurisdictions = append(jurisdictions, readyPlacementJurisdictions(entry.Placements)...)
			slices.Sort(jurisdictions)
			out[key] = slices.Compact(jurisdictions)
		}
	}
	return out
}

// defaultResolveDispatchWizardHome reads home_jurisdiction from the login the
// dispatch itself is generated with (the cell client factory), so the picker's
// default and the request's routing agree on which login they mean.
func defaultResolveDispatchWizardHome(ctx context.Context) string {
	factory, err := auth.NewEntireAPICellClientFactory(ctx, false)
	if err != nil {
		logging.Debug(ctx, "dispatch wizard: home jurisdiction unavailable", "error", err)
		return ""
	}
	home, err := factory.HomeJurisdiction()
	if err != nil {
		logging.Debug(ctx, "dispatch wizard: home jurisdiction unavailable", "error", err)
		return ""
	}
	return home
}
