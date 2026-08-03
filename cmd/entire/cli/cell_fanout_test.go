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

func TestGroupReposByCell(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{ID: "01B", Cell: "aws-us-east-2", ClusterSlug: "us-prod", Jurisdiction: "us"},
		{ID: "01C", Cell: "AWS-US-EAST-2", ClusterSlug: "us-prod", Jurisdiction: "US"}, // case-folds into same group
		{ID: "01A", Cell: euWestCell, ClusterSlug: "eu-prod", Jurisdiction: "eu"},
		{ID: "", Cell: euWestCell}, // no ID → skipped
		// Blank cell in different jurisdictions must NOT collapse into one
		// group — each routes via its own jurisdiction fallback.
		{ID: "01D", Jurisdiction: "eu"},
		{ID: "01E", Jurisdiction: "us"},
	}
	cells := groupReposByCell(repos)
	if len(cells) != 4 {
		t.Fatalf("groups = %d, want 4: %+v", len(cells), cells)
	}
	// Deterministic order by cell name, jurisdiction tiebreak:
	// ""/eu < ""/us < aws-eu-west-1 < aws-us-east-2.
	order := []struct{ cell, jurisdiction string }{
		{"", "eu"}, {"", "us"}, {euWestCell, "eu"}, {"aws-us-east-2", "us"},
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

// TestGroupReposByCell_Placements verifies that when a repo has Placements,
// each placement is grouped into its own cell group with the placement-specific
// repo ID. This is the fix for cross-region fan-out: a US-homed repo with an
// EU mirror produces two cell groups so both cells are searched.
func TestGroupReposByCell_Placements(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			// US-homed repo with an EU mirror — the real-world scenario. Each
			// placement carries its OWN cluster slug (the /repos spec bump).
			ID: "01US", Cell: "aws-us-east-2", ClusterSlug: "us-prod", Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				{ID: "01US", Cell: "aws-us-east-2", ClusterSlug: "us-prod", Jurisdiction: "us"},
				{ID: "01EU", Cell: "aws-eu-central-1", ClusterSlug: "eu-prod", Jurisdiction: "eu", Mirror: true},
			},
		},
		{
			// Repo without placements (legacy index) — top-level fields used.
			ID: "01LEGACY", Cell: "aws-us-east-2", ClusterSlug: "us-prod", Jurisdiction: "us",
		},
	}
	cells := groupReposByCell(repos)
	if len(cells) != 2 {
		t.Fatalf("groups = %d, want 2 (one per cell): %+v", len(cells), cells)
	}
	// Sorted: aws-eu-central-1 < aws-us-east-2.
	eu := cells[0]
	us := cells[1]
	if eu.cell != "aws-eu-central-1" || eu.jurisdiction != "eu" {
		t.Fatalf("eu group = %+v", eu)
	}
	if got := strings.Join(eu.repoIDs, ","); got != "01EU" {
		t.Fatalf("eu repoIDs = %q, want 01EU", got)
	}
	// The EU mirror carries its own slug, so the group can do the exact
	// catalog join instead of falling back to jurisdiction-default routing.
	if eu.clusterSlug != "eu-prod" {
		t.Fatalf("eu clusterSlug = %q, want eu-prod", eu.clusterSlug)
	}
	if us.cell != "aws-us-east-2" || us.jurisdiction != "us" {
		t.Fatalf("us group = %+v", us)
	}
	// US group has both the placement ID and the legacy entry.
	if got := strings.Join(us.repoIDs, ","); got != "01US,01LEGACY" {
		t.Fatalf("us repoIDs = %q, want 01US,01LEGACY", got)
	}
	// US group gets its slug from the placement (and the legacy entry agrees).
	if us.clusterSlug != testClusterSlugUS {
		t.Fatalf("us clusterSlug = %q, want us-prod", us.clusterSlug)
	}
}

// TestGroupReposByCell_PlacementEmptyID verifies that placements with empty IDs
// are skipped, matching the top-level behavior.
func TestGroupReposByCell_PlacementEmptyID(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			ID: "01A", Cell: "aws-us-east-2", Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				{ID: "", Cell: "aws-us-east-2", Jurisdiction: "us"}, // empty ID → skipped
			},
		},
	}
	cells := groupReposByCell(repos)
	if len(cells) != 0 {
		t.Fatalf("groups = %d, want 0 (all placement IDs empty): %+v", len(cells), cells)
	}
}

// TestGroupReposByCell_PlacementUsesOwnSlug verifies each placement routes by
// its OWN cluster slug (home and mirror alike), independent of the Mirror flag
// and of the deprecated top-level Cell/ClusterSlug. This is the fix for the
// mirror-miss: a mirror placement that carries its slug gets the exact catalog
// join instead of the jurisdiction-default fallback. A placement that genuinely
// omits its slug still falls back (empty slug on its group).
func TestGroupReposByCell_PlacementUsesOwnSlug(t *testing.T) {
	t.Parallel()
	repos := []coreapi.RepoIndexEntry{
		{
			// Top-level Cell/ClusterSlug intentionally empty; routing must come
			// entirely from the per-placement fields.
			ID: "01US", Jurisdiction: "us",
			Placements: []coreapi.RepoPlacement{
				{ID: "01US", Cell: "aws-us-east-2", ClusterSlug: "us-prod", Jurisdiction: "us", Mirror: false},
				{ID: "01EU", Cell: "aws-eu-central-1", ClusterSlug: "eu-prod", Jurisdiction: "eu", Mirror: true},
				{ID: "01AP", Cell: "aws-ap-south-1", Jurisdiction: "ap", Mirror: true}, // no slug → fallback
			},
		},
	}
	cells := groupReposByCell(repos)
	if len(cells) != 3 {
		t.Fatalf("groups = %d, want 3: %+v", len(cells), cells)
	}
	// Sorted by cell: aws-ap-south-1 < aws-eu-central-1 < aws-us-east-2.
	ap, eu, us := cells[0], cells[1], cells[2]
	if us.cell != "aws-us-east-2" || us.clusterSlug != testClusterSlugUS {
		t.Fatalf("home group = %+v, want cell aws-us-east-2 with slug us-prod", us)
	}
	if eu.cell != "aws-eu-central-1" || eu.clusterSlug != "eu-prod" {
		t.Fatalf("mirror group = %+v, want cell aws-eu-central-1 with slug eu-prod", eu)
	}
	if ap.cell != "aws-ap-south-1" || ap.clusterSlug != "" {
		t.Fatalf("slugless mirror group = %+v, want cell aws-ap-south-1 with empty slug", ap)
	}
}

// TestResolveCellBaseURLs_RefusesBaseURLWithoutJurisdiction pins the guard: a
// concrete baseURL is only usable together with the jurisdiction its token
// must be minted for; a catalog row with no jurisdiction leaves the group on
// home routing instead of dialing a foreign cell with a home token.
func TestResolveCellBaseURLs_RefusesBaseURLWithoutJurisdiction(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{{cell: "aws-eu-west-1", clusterSlug: "eu-prod"}} // no jurisdiction anywhere
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		{Slug: "eu-prod", ApiUrl: coreapi.NewOptString(euCellAPIURL)}, // row has no jurisdiction either
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
		// Slug ("eu-prod") differs from the cell name (euWestCell).
		{cell: euWestCell, clusterSlug: "eu-prod", jurisdiction: "eu"},
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
		{cell: "aws-us-east-2", clusterSlug: "us-prod", jurisdiction: "us"},
		// Mirror group without slug — must fall back to jurisdiction join.
		{cell: "aws-eu-central-1", clusterSlug: "", jurisdiction: "eu"},
	}
	fake := &fakeCellCore{clusters: []coreapi.Cluster{
		{Slug: "us-prod", Jurisdiction: "us", ApiUrl: coreapi.NewOptString("https://aws-us-east-2.api.entire.io")},
		{Slug: "eu-prod", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://aws-eu-central-1.api.entire.io")},
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
		{cell: "aws-eu-central-1", clusterSlug: "", jurisdiction: "eu"},
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
		{Slug: "eu-prod", Jurisdiction: "eu", IsDefault: true, ApiUrl: coreapi.NewOptString("https://eu-default.api.entire.io")},
	}}
	resolveCellBaseURLs(context.Background(), fake, cells)
	if cells[0].baseURL != "https://eu-default.api.entire.io" {
		t.Fatalf("eu baseURL = %q, want default cluster's URL", cells[0].baseURL)
	}
}

func TestResolveCellBaseURLs_CatalogErrorLeavesJurisdictionRouting(t *testing.T) {
	t.Parallel()
	cells := []cellGroup{{cell: euWestCell, clusterSlug: "eu-prod", jurisdiction: "eu"}}
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
		{cell: euWestCell, jurisdiction: "eu", baseURL: "https://eu.api.example", repoIDs: []string{"01A"}},
		{cell: "aws-us-east-2", jurisdiction: "us", baseURL: "https://us.api.example", repoIDs: []string{"01B"}},
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
