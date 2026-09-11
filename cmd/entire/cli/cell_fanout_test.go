package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// Placement ULIDs and cell/slug names reused across the fan-out tests.
const (
	testPlacementFallback = "01FALLBACK"
	testClusterSlugEU     = "eu-prod"
	testPlacementUS       = "01US"
	testPlacementA        = "01A"
	usEastCell            = "aws-us-east-2"
	euCentralCell         = "aws-eu-central-1"
	testPlacementEU       = "01EU"
	testPlacementAU       = "01AU"
)

// readyPlacement is the common fan-out fixture. Mirror is true because real
// /repos rows mark EVERY placement Mirror:true (the pick must never key on
// it); deliberate exceptions (unready, empty ID, no slug) stay literal so the
// exception is the thing that looks unusual.
func readyPlacement(id, cell, slug, jurisdiction string) coreapi.RepoPlacement {
	return coreapi.RepoPlacement{ID: id, Cell: cell, ClusterSlug: slug, Jurisdiction: jurisdiction, Mirror: true, Status: coreapi.RepoPlacementStatusReady}
}

func TestGroupReposByCell(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{ID: "01B", Cell: usEastCell, ClusterSlug: testClusterSlugUS, Jurisdiction: "us"},
		{ID: "01C", Cell: "AWS-US-EAST-2", ClusterSlug: testClusterSlugUS, Jurisdiction: "US"}, // case-folds into same group
		{ID: testPlacementA, Cell: euWestCell, ClusterSlug: testClusterSlugEU, Jurisdiction: "eu"},
		{ID: "", Cell: euWestCell}, // no ID → skipped
		// Blank cell in different jurisdictions must NOT collapse into one
		// group — each routes via its own jurisdiction fallback.
		{ID: "01D", Jurisdiction: "eu"},
		{ID: "01E", Jurisdiction: "us"},
	}
	cells, _ := groupReposByCell(repos)
	if len(cells) != 4 {
		t.Fatalf("groups = %d, want 4: %+v", len(cells), cells)
	}
	// Deterministic order by cell name, jurisdiction tiebreak:
	// ""/eu < ""/us < aws-eu-west-1 < aws-us-east-2.
	order := []struct{ cell, jurisdiction string }{
		{"", "eu"}, {"", "us"}, {euWestCell, "eu"}, {usEastCell, "us"},
	}
	for i, want := range order {
		if cells[i].cell != want.cell || cells[i].jurisdiction != want.jurisdiction {
			t.Fatalf("group[%d] = %q/%q, want %q/%q", i, cells[i].cell, cells[i].jurisdiction, want.cell, want.jurisdiction)
		}
	}
	if got := strings.Join(cells[0].repoIDs, ","); got != "01D" {
		t.Fatalf("blank-cell eu repoIDs = %q, want 01D", got)
	}
	us := cells[3]
	if got := strings.Join(us.repoIDs, ","); got != "01B,01C" {
		t.Fatalf("us repoIDs = %q, want 01B,01C", got)
	}
	if us.clusterSlug != testClusterSlugUS || us.jurisdiction != "us" {
		t.Fatalf("us group coordinates = %+v, want us-prod/us", us)
	}
}

// TestGroupReposByCell_Placements verifies that a repo with Placements routes
// to ONE placement — with no election on the row, the canonical convention
// picks the placement whose ID equals the entry's ID, never the Mirror flag
// (see routedRepoPlacement).
func TestGroupReposByCell_Placements(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			// US-homed repo with an EU mirror.
			ID: testPlacementUS, Cell: usEastCell, ClusterSlug: testClusterSlugUS, Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				readyPlacement(testPlacementUS, usEastCell, testClusterSlugUS, "us"),
				readyPlacement(testPlacementEU, euCentralCell, testClusterSlugEU, "eu"),
			},
		},
		{
			// Repo without placements (legacy index) — top-level fields used.
			ID: "01LEGACY", Cell: usEastCell, ClusterSlug: testClusterSlugUS, Jurisdiction: "us",
		},
	}
	cells, skipped := groupReposByCell(repos)
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if len(cells) != 1 {
		t.Fatalf("groups = %d, want 1 (canonical cell only): %+v", len(cells), cells)
	}
	us := cells[0]
	if us.cell != usEastCell || us.jurisdiction != "us" {
		t.Fatalf("us group = %+v", us)
	}
	// Canonical placement ID plus the legacy entry; the EU mirror ID must not
	// appear anywhere.
	if got := strings.Join(us.repoIDs, ","); got != "01US,01LEGACY" {
		t.Fatalf("us repoIDs = %q, want 01US,01LEGACY", got)
	}
	if us.clusterSlug != testClusterSlugUS {
		t.Fatalf("us clusterSlug = %q, want us-prod", us.clusterSlug)
	}
}

// TestGroupReposByCell_ProcessingPrimaryPreferred verifies that core's
// explicit primaries.processing ULID outranks the row-ID convention: when the
// two disagree, the processing primary wins (it names the cell doing the
// repo's heavy lifting — where searchable content originates). An unknown
// processing ULID (not among the placements) falls back to the convention.
func TestGroupReposByCell_ProcessingPrimaryPreferred(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			// Row ID says US, primaries.processing says EU — EU must win.
			ID: testPlacementUS, Jurisdiction: "us",
			Primaries: coreapi.NewOptRepoPrimaries(coreapi.RepoPrimaries{Processing: testPlacementEU}),
			Placements: []coreapi.RepoPlacement{
				readyPlacement(testPlacementUS, usEastCell, testClusterSlugUS, "us"),
				readyPlacement(testPlacementEU, euCentralCell, testClusterSlugEU, "eu"),
			},
		},
		{
			// primaries.processing names a ULID that is not among the
			// placements (inconsistent row) — fall back to the row-ID match.
			ID: testPlacementFallback, Jurisdiction: "us",
			Primaries: coreapi.NewOptRepoPrimaries(coreapi.RepoPrimaries{Processing: "01ELSEWHERE"}),
			Placements: []coreapi.RepoPlacement{
				readyPlacement(testPlacementFallback, usEastCell, testClusterSlugUS, "us"),
			},
		},
	}
	cells, skipped := groupReposByCell(repos)
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if len(cells) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(cells), cells)
	}
	// Sorted: aws-eu-central-1 < aws-us-east-2.
	eu, us := cells[0], cells[1]
	if eu.cell != euCentralCell || strings.Join(eu.repoIDs, ",") != testPlacementEU {
		t.Fatalf("eu group = %+v, want processing primary %s", eu, testPlacementEU)
	}
	if us.cell != usEastCell || strings.Join(us.repoIDs, ",") != testPlacementFallback {
		t.Fatalf("us group = %+v, want row-ID fallback %s", us, testPlacementFallback)
	}
}

// TestGroupReposByCell_CanonicalNotFirst verifies fallback selection (rows
// predating primaries.processing) scans all placements for the row-ID match
// rather than assuming position, and that with no ID match the first
// placement serves as fallback (the BFF's canonicalPlacement convention:
// placements.find(p => p.id === row.id) ?? placements[0]).
func TestGroupReposByCell_CanonicalNotFirst(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			ID: testPlacementAU, Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				readyPlacement(testPlacementUS, usEastCell, testClusterSlugUS, "us"),
				readyPlacement(testPlacementEU, euCentralCell, testClusterSlugEU, "eu"),
				readyPlacement(testPlacementAU, "aws-ap-southeast-2", "au-prod", "au"),
			},
		},
		{
			// No placement matches the entry ID — fall back to placements[0].
			ID: "01GONE", Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				readyPlacement("01FIRST", usEastCell, testClusterSlugUS, "us"),
				readyPlacement("01SECOND", euCentralCell, testClusterSlugEU, "eu"),
			},
		},
	}
	cells, skipped := groupReposByCell(repos)
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if len(cells) != 2 {
		t.Fatalf("groups = %d, want 2: %+v", len(cells), cells)
	}
	// Sorted: aws-ap-southeast-2 < aws-us-east-2.
	au, us := cells[0], cells[1]
	if au.cell != "aws-ap-southeast-2" || strings.Join(au.repoIDs, ",") != testPlacementAU {
		t.Fatalf("au group = %+v, want canonical %s", au, testPlacementAU)
	}
	if us.cell != usEastCell || strings.Join(us.repoIDs, ",") != "01FIRST" {
		t.Fatalf("us group = %+v, want fallback 01FIRST", us)
	}
}

// TestGroupReposByCell_UnreadyCanonicalSkipped verifies a picked placement
// that is not ready (processing/failed/suspended) is not routed — there is
// nothing searchable there yet — and the repo is reported so callers can warn
// instead of silently narrowing the search.
func TestGroupReposByCell_UnreadyCanonicalSkipped(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			ID: "01CLONING", FullName: "acme/cloning", Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				{ID: "01CLONING", Cell: usEastCell, ClusterSlug: testClusterSlugUS, Jurisdiction: "us", Status: coreapi.RepoPlacementStatusProcessing},
				readyPlacement("01CLONEEU", euCentralCell, testClusterSlugEU, "eu"),
			},
		},
	}
	cells, skipped := groupReposByCell(repos)
	// The unready canonical is skipped entirely — the ready mirror is NOT a
	// substitute (it may predate the clone; the BFF treats the repo as
	// unroutable the same way).
	if strings.Join(skipped, ",") != "acme/cloning" {
		t.Fatalf("skipped = %v, want [acme/cloning]", skipped)
	}
	if len(cells) != 0 {
		t.Fatalf("groups = %d, want 0: %+v", len(cells), cells)
	}
}

// TestRoutedRepoPlacement_NoPlacements pins the defensive guard: an entry
// with no placements is unroutable (ok=false), never a panic. groupReposByCell
// routes such entries via the top-level legacy fields before calling this.
func TestRoutedRepoPlacement_NoPlacements(t *testing.T) {
	t.Parallel()
	if _, ok := routedRepoPlacement(coreapi.RepoIndexEntry{ID: testPlacementA}); ok {
		t.Fatal("ok = true for entry with no placements, want false")
	}
}

// TestPlacementByID pins the normalization contract: both sides of the match
// are trimmed, and an empty or blank id never matches anything.
func TestPlacementByID(t *testing.T) {
	t.Parallel()
	placements := []coreapi.RepoPlacement{{ID: " 01X "}}
	if p, ok := placementByID(placements, "01X"); !ok || p.ID != " 01X " {
		t.Fatalf("trimmed match = %+v, %v; want the placement, true", p, ok)
	}
	for _, id := range []string{"", "   ", "01MISSING"} {
		if _, ok := placementByID(placements, id); ok {
			t.Fatalf("id %q matched, want no match", id)
		}
	}
}

// TestReportableSkippedRepos pins the surfacing gate: pinned requests report
// every skip; broad requests stay silent while any cell is queried, but
// report when the skips left nothing to query (a bare "no results" would be
// misleading). Regression guard for inverting the gate — nothing else fails.
func TestReportableSkippedRepos(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	skipped := []string{"acme/cloning"}
	cases := []struct {
		name         string
		pinned       bool
		queriedCells int
		want         int
	}{
		{"pinned always reports", true, 2, 1},
		{"broad with cells stays silent", false, 2, 0},
		{"broad with nothing to query reports", false, 0, 1},
	}
	for _, tc := range cases {
		if got := reportableSkippedRepos(ctx, tc.pinned, tc.queriedCells, skipped); len(got) != tc.want {
			t.Fatalf("%s: reported %v, want %d entries", tc.name, got, tc.want)
		}
	}
	if got := reportableSkippedRepos(ctx, true, 0, nil); got != nil {
		t.Fatalf("no skips: reported %v, want nil", got)
	}
}

// TestGroupReposByCell_PlacementEmptyID verifies that a canonical placement
// with an empty ID is not routed AND is reported via skipped — an ID-less
// placement must not become a silent-loss path now that a reporting channel
// exists.
func TestGroupReposByCell_PlacementEmptyID(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			ID: testPlacementA, Cell: usEastCell, Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				{ID: "", Cell: usEastCell, Jurisdiction: "us"}, // empty ID → skipped + reported
			},
		},
	}
	cells, skipped := groupReposByCell(repos)
	if len(cells) != 0 {
		t.Fatalf("groups = %d, want 0 (all placement IDs empty): %+v", len(cells), cells)
	}
	if strings.Join(skipped, ",") != testPlacementA {
		t.Fatalf("skipped = %v, want the ID-less repo reported as [%s]", skipped, testPlacementA)
	}
}

// TestGroupReposByCell_PlacementUsesOwnSlug verifies the canonical placement
// routes by its OWN cluster slug, independent of the deprecated top-level
// Cell/ClusterSlug, so resolveCellBaseURLs can do the exact catalog join
// instead of the jurisdiction-default fallback.
func TestGroupReposByCell_PlacementUsesOwnSlug(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			// Top-level Cell/ClusterSlug intentionally empty; routing must come
			// entirely from the canonical placement's fields.
			ID: testPlacementUS, Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				{ID: testPlacementUS, Cell: usEastCell, ClusterSlug: testClusterSlugUS, Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady},
				{ID: testPlacementEU, Cell: euCentralCell, ClusterSlug: testClusterSlugEU, Jurisdiction: "eu", Mirror: true, Status: coreapi.RepoPlacementStatusReady},
			},
		},
	}
	cells, _ := groupReposByCell(repos)
	if len(cells) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(cells), cells)
	}
	us := cells[0]
	if us.cell != usEastCell || us.clusterSlug != testClusterSlugUS {
		t.Fatalf("canonical group = %+v, want cell aws-us-east-2 with slug us-prod", us)
	}
}

// TestResolveCellBaseURLs_RefusesBaseURLWithoutJurisdiction pins the guard: a
// concrete baseURL is only usable together with the jurisdiction its token
// must be minted for; a catalog row with no jurisdiction leaves the group on
// home routing instead of dialing a foreign cell with a home token.
func TestResolveCellBaseURLs_RefusesBaseURLWithoutJurisdiction(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{{cell: "aws-eu-west-1", clusterSlug: testClusterSlugEU}} // no jurisdiction anywhere
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		{Slug: testClusterSlugEU, ApiUrl: coreapi.NewOptString(euCellAPIURL)}, // row has no jurisdiction either
	}}
	resolveCellBaseURLs(context.Background(), fake, cells)
	if cells[0].baseURL != "" || cells[0].jurisdiction != "" {
		t.Fatalf("group = %+v, want untouched (home routing)", cells[0])
	}
}

// TestResolveCellBaseURLs_JoinsOnClusterSlug pins the catalog join key: the
// cluster catalog has no cell field, so groups must join on ClusterSlug —
// joining the cell name against Cluster.Slug only works when the two happen to
// coincide.
func TestResolveCellBaseURLs_JoinsOnClusterSlug(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{
		// Slug (testClusterSlugEU) differs from the cell name (euWestCell).
		{cell: euWestCell, clusterSlug: testClusterSlugEU, jurisdiction: "eu"},
		{cell: "aws-ap-south-1", clusterSlug: "ap-prod", jurisdiction: "ap"}, // not in catalog
	}
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		{Slug: "EU-Prod", Jurisdiction: "EU", ApiUrl: coreapi.NewOptString("https://aws-eu-west-1.api.entire.io/")},
	}}
	resolveCellBaseURLs(context.Background(), fake, cells)
	if got := cells[0].baseURL; got != "https://aws-eu-west-1.api.entire.io" {
		t.Fatalf("eu baseURL = %q, want the catalog apiUrl (trimmed)", got)
	}
	if cells[0].jurisdiction != "eu" {
		t.Fatalf("eu jurisdiction = %q, want normalised eu", cells[0].jurisdiction)
	}
	if cells[1].baseURL != "" {
		t.Fatalf("ap baseURL = %q, want empty (jurisdiction fallback)", cells[1].baseURL)
	}
}

// TestResolveCellBaseURLs_JurisdictionFallbackForPlacements verifies that
// groups without a cluster slug (from placement-derived groups) resolve their
// baseURL via jurisdiction matching against the cluster catalog.
func TestResolveCellBaseURLs_JurisdictionFallbackForPlacements(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{
		// Home group with slug — resolved via slug join.
		{cell: usEastCell, clusterSlug: testClusterSlugUS, jurisdiction: "us"},
		// Mirror group without slug — must fall back to jurisdiction join.
		{cell: euCentralCell, clusterSlug: "", jurisdiction: "eu"},
	}
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		{Slug: testClusterSlugUS, Jurisdiction: "us", ApiUrl: coreapi.NewOptString("https://aws-us-east-2.api.entire.io")},
		{Slug: testClusterSlugEU, Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://aws-eu-central-1.api.entire.io")},
	}}
	resolveCellBaseURLs(context.Background(), fake, cells)
	if cells[0].baseURL != "https://aws-us-east-2.api.entire.io" {
		t.Fatalf("us baseURL = %q, want resolved via slug", cells[0].baseURL)
	}
	if cells[1].baseURL != "https://aws-eu-central-1.api.entire.io" {
		t.Fatalf("eu baseURL = %q, want resolved via jurisdiction fallback", cells[1].baseURL)
	}
}

// TestResolveCellBaseURLs_CellURLMatchOverJurisdiction verifies that when a
// jurisdiction has multiple clusters, the resolver matches the group's cell
// name against cluster ApiUrl hosts rather than picking an arbitrary one.
// This prevents binding a mirror group to the wrong cell's baseURL.
func TestResolveCellBaseURLs_CellURLMatchOverJurisdiction(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{
		// Mirror group whose cell name appears in the second cluster's URL.
		{cell: euCentralCell, clusterSlug: "", jurisdiction: "eu"},
	}
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		// Different EU cell — must NOT be picked even though it's first and default.
		{Slug: "eu-west-prod", Jurisdiction: "eu", IsDefault: true, ApiUrl: coreapi.NewOptString("https://aws-eu-west-1.api.entire.io")},
		// Matching cell — should be picked by cell-URL matching.
		{Slug: "eu-central-prod", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://aws-eu-central-1.api.entire.io")},
	}}
	resolveCellBaseURLs(context.Background(), fake, cells)
	if cells[0].baseURL != "https://aws-eu-central-1.api.entire.io" {
		t.Fatalf("eu baseURL = %q, want cell-matched URL, not default cluster", cells[0].baseURL)
	}
}

// TestResolveCellBaseURLs_JurisdictionFallbackPrefersDefault verifies that
// when cell-URL matching doesn't find a match, the jurisdiction fallback
// picks the cluster with IsDefault=true.
func TestResolveCellBaseURLs_JurisdictionFallbackPrefersDefault(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{
		// Cell name doesn't appear in any cluster URL — falls through to jurisdiction.
		{cell: "aws-eu-unknown-1", clusterSlug: "", jurisdiction: "eu"},
	}
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		// Non-default listed first — must not win.
		{Slug: "eu-staging", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://eu-staging.api.entire.io")},
		// Default cluster — should be preferred.
		{Slug: testClusterSlugEU, Jurisdiction: "eu", IsDefault: true, ApiUrl: coreapi.NewOptString("https://eu-default.api.entire.io")},
	}}
	resolveCellBaseURLs(context.Background(), fake, cells)
	if cells[0].baseURL != "https://eu-default.api.entire.io" {
		t.Fatalf("eu baseURL = %q, want default cluster's URL", cells[0].baseURL)
	}
}

func TestResolveCellBaseURLs_CatalogErrorLeavesJurisdictionRouting(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{{cell: euWestCell, clusterSlug: testClusterSlugEU, jurisdiction: "eu"}}
	resolveCellBaseURLs(context.Background(), &fakeCellCore{clustersErr: errors.New("boom")}, cells)
	if cells[0].baseURL != "" {
		t.Fatalf("baseURL = %q, want empty after catalog error", cells[0].baseURL)
	}
}

func TestCellGroupTargetAndLabel(t *testing.T) {
	t.Parallel()
	full := cellGroup{cell: euWestCell, jurisdiction: "eu", baseURL: "https://aws-eu-west-1.api.entire.io"}
	if tgt := full.cellTarget(); tgt == nil || tgt.BaseURL != full.baseURL || tgt.Jurisdiction != "eu" {
		t.Fatalf("full target = %+v", tgt)
	}
	jur := cellGroup{jurisdiction: "eu"}
	if tgt := jur.cellTarget(); tgt == nil || tgt.BaseURL != "" || tgt.Jurisdiction != "eu" {
		t.Fatalf("jurisdiction-only target = %+v", tgt)
	}
	if tgt := (cellGroup{}).cellTarget(); tgt != nil {
		t.Fatalf("empty group target = %+v, want nil (home routing)", tgt)
	}
	if got := full.label(); got != euWestCell {
		t.Fatalf("label = %q", got)
	}
	if got := jur.label(); got != "eu" {
		t.Fatalf("label = %q", got)
	}
	if got := (cellGroup{}).label(); got != "home" {
		t.Fatalf("label = %q, want home", got)
	}
}

// fakeCellClientBuilder hands out unauthenticated clients keyed by target and
// records what it was asked for.
type fakeCellClientBuilder struct {
	mu      sync.Mutex // fanOutCells calls ClientFor from one goroutine per cell
	targets []*auth.CellTarget
	err     error
}

func (f *fakeCellClientBuilder) ClientFor(_ context.Context, target *auth.CellTarget) (*api.Client, error) {
	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	base := "https://home.api.example"
	if target != nil && target.BaseURL != "" {
		base = target.BaseURL
	}
	return api.NewClientWithBaseURL("test-token", base), nil
}

func withFakeCellClientBuilder(t *testing.T, f *fakeCellClientBuilder) {
	t.Helper()
	prev := newCellClientBuilder
	newCellClientBuilder = func(context.Context, bool) (cellClientBuilder, error) { return f, nil }
	t.Cleanup(func() { newCellClientBuilder = prev })
}

func TestFanOutCells_PartialFailureIsPerCell(t *testing.T) {
	// Not parallel: swaps the package-level newCellClientBuilder seam.
	withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
	cells := []cellGroup{
		{cell: euWestCell, jurisdiction: "eu", baseURL: "https://eu.api.example", repoIDs: []string{testPlacementA}},
		{cell: usEastCell, jurisdiction: "us", baseURL: "https://us.api.example", repoIDs: []string{"01B"}},
	}
	boom := errors.New("cell down")
	results, err := fanOutCells(context.Background(), false, time.Second, cells,
		func(ctx context.Context, g cellGroup, _ *api.Client) (string, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("per-cell ctx has no deadline")
			}
			if g.cell == euWestCell {
				return "", boom
			}
			return "hits:" + strings.Join(g.repoIDs, ","), nil
		})
	if err != nil {
		t.Fatalf("fanOutCells: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	// Input order preserved; the eu failure is isolated in its slot.
	if !errors.Is(results[0].err, boom) || results[0].group.cell != euWestCell {
		t.Fatalf("results[0] = %+v, want eu failure", results[0])
	}
	if results[1].err != nil || results[1].value != "hits:01B" {
		t.Fatalf("results[1] = %+v, want us success", results[1])
	}
}

func TestFanOutCells_SingleCellRunsSerially(t *testing.T) {
	// Not parallel: swaps the package-level newCellClientBuilder seam.
	builder := &fakeCellClientBuilder{}
	withFakeCellClientBuilder(t, builder)
	cells := []cellGroup{{jurisdiction: "eu", baseURL: "https://eu.api.example"}}
	results, err := fanOutCells(context.Background(), false, time.Second, cells,
		func(_ context.Context, _ cellGroup, _ *api.Client) (string, error) {
			return "ok", nil
		})
	if err != nil || len(results) != 1 || results[0].err != nil || results[0].value != "ok" {
		t.Fatalf("results = %+v, err = %v", results, err)
	}
	if len(builder.targets) != 1 || builder.targets[0].BaseURL != "https://eu.api.example" {
		t.Fatalf("builder targets = %+v", builder.targets)
	}
}

func TestFanOutCells_EmptyAndFactoryError(t *testing.T) {
	// Not parallel: swaps the package-level newCellClientBuilder seam.
	results, err := fanOutCells(context.Background(), false, time.Second, nil,
		func(context.Context, cellGroup, *api.Client) (int, error) { return 0, nil })
	if results != nil || err != nil {
		t.Fatalf("empty fan-out = (%v, %v), want (nil, nil)", results, err)
	}

	factoryErr := errors.New("not logged in")
	prev := newCellClientBuilder
	newCellClientBuilder = func(context.Context, bool) (cellClientBuilder, error) { return nil, factoryErr }
	t.Cleanup(func() { newCellClientBuilder = prev })
	if _, err := fanOutCells(context.Background(), false, time.Second, []cellGroup{{jurisdiction: "eu"}},
		func(context.Context, cellGroup, *api.Client) (int, error) { return 0, nil }); !errors.Is(err, factoryErr) {
		t.Fatalf("err = %v, want factory error", err)
	}
}

// TestFanOutCells_ClientPerCellFromOneBuilder asserts every cell's client
// comes from the single shared builder (one subject, per-jurisdiction token
// reuse lives behind it in auth.CellClientFactory).
func TestFanOutCells_ClientPerCellFromOneBuilder(t *testing.T) {
	// Not parallel: swaps the package-level newCellClientBuilder seam.
	builder := &fakeCellClientBuilder{}
	withFakeCellClientBuilder(t, builder)
	var cells []cellGroup
	for i := range 3 {
		cells = append(cells, cellGroup{
			cell:         fmt.Sprintf("cell-%d", i),
			jurisdiction: "eu",
			baseURL:      fmt.Sprintf("https://cell-%d.api.example", i),
		})
	}
	results, err := fanOutCells(context.Background(), false, time.Second, cells,
		func(_ context.Context, g cellGroup, _ *api.Client) (string, error) { return g.cell, nil })
	if err != nil {
		t.Fatalf("fanOutCells: %v", err)
	}
	for i, r := range results {
		if r.err != nil || r.value != fmt.Sprintf("cell-%d", i) {
			t.Fatalf("results[%d] = %+v", i, r)
		}
	}
	if len(builder.targets) != 3 {
		t.Fatalf("builder asked for %d targets, want 3", len(builder.targets))
	}
}
