package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/internal/coreapi"
)

const euCellAPIURL = "https://eu.api.entire.io"

const euWestCell = "aws-eu-west-1"

// usCellAPIURL and usClusterSlug mirror the real aws-us-east-2 cell that
// entirehq/entire.io's processing placement lives on in prod, so the
// multi-homed regression tests below reproduce the actual bug rather than an
// invented topology.
const (
	usCellAPIURL  = "https://aws-us-east-2.api.entire.io"
	usClusterSlug = "aws-us-east-2"

	euClusterSlug          = euWestCell
	apSoutheastClusterSlug = "aws-ap-southeast-1"
	apSouthClusterSlug     = "aws-ap-south-1"
)

func TestMatchClusterByHost(t *testing.T) {
	t.Parallel()
	clusters := []coreapi.Cluster{
		{PublicUrl: "https://us.entire.io", Jurisdiction: "us", ApiUrl: coreapi.NewOptString("https://aws-us-east-2.api.entire.io")},
		{PublicUrl: "https://eu.entire.io", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://aws-eu-west-1.api.entire.io")},
	}

	// Match is on the public host, case-insensitive.
	cl, ok := matchClusterByHost(clusters, "EU.entire.io")
	if !ok {
		t.Fatal("expected a match for eu.entire.io")
	}
	if cl.Jurisdiction != "eu" || cl.ApiUrl.Or("") != "https://aws-eu-west-1.api.entire.io" {
		t.Fatalf("matched wrong cluster: %+v", cl)
	}

	if _, ok := matchClusterByHost(clusters, "ap.entire.io"); ok {
		t.Fatal("expected no match for unknown host")
	}
	if _, ok := matchClusterByHost(clusters, ""); ok {
		t.Fatal("expected no match for empty host")
	}
}

// TestMatchClusterBySlug mirrors TestMatchClusterByHost for the slug-keyed
// join used by the processing-placement path: the catalog's Slug, not its
// PublicUrl host, is the reliable join key for a RepoPlacement.
func TestMatchClusterBySlug(t *testing.T) {
	t.Parallel()
	clusters := clustersWithSlugs()

	cl, ok := matchClusterBySlug(clusters, usClusterSlug)
	if !ok {
		t.Fatalf("expected a match for %s", usClusterSlug)
	}
	if cl.Jurisdiction != "us" || cl.ApiUrl.Or("") != usCellAPIURL {
		t.Fatalf("matched wrong cluster: %+v", cl)
	}

	if _, ok := matchClusterBySlug(clusters, "unknown-slug"); ok {
		t.Fatal("expected no match for unknown slug")
	}
	if _, ok := matchClusterBySlug(clusters, ""); ok {
		t.Fatal("expected no match for empty slug")
	}
}

// fakeCellCore is a stub control plane for resolveRepoCellTarget /
// resolveRepoCellPlacement tests.
type fakeCellCore struct {
	repo         *coreapi.Repo
	repoErr      error
	projects     *coreapi.ListProjectsOutputBody
	projectsErr  error
	projectRepos *coreapi.ListProjectReposOutputBody
	projectErr   error
	clusters     []coreapi.Cluster
	clustersErr  error
	repos        *coreapi.ListReposOutputBody
	reposErr     error
	// blockUntilCtxDone makes ListRepos and GetRepo hang until the caller's
	// deadline fires, standing in for a reachable-but-slow control plane —
	// both, so the owner/repo and ULID paths can each be tested. Off by
	// default, so existing tests are unaffected.
	blockUntilCtxDone bool
	// lastListReposParams records the params passed to the most recent
	// ListRepos call, so a test can assert the resolver actually sets Filter
	// to the requested repo rather than merely returning whatever fixture was
	// configured regardless of what was asked.
	lastListReposParams coreapi.ListReposParams
	mu                  sync.Mutex
}

// waitIfBlocking simulates a core that accepts the connection and then stalls.
func (f *fakeCellCore) waitIfBlocking(ctx context.Context) error {
	if !f.blockUntilCtxDone {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeCellCore) GetRepo(ctx context.Context, _ coreapi.GetRepoParams) (*coreapi.Repo, error) {
	if err := f.waitIfBlocking(ctx); err != nil {
		return nil, err
	}
	return f.repo, f.repoErr
}

func (f *fakeCellCore) ListProjects(ctx context.Context, _ coreapi.ListProjectsParams) (*coreapi.ListProjectsOutputBody, error) {
	if err := f.waitIfBlocking(ctx); err != nil {
		return nil, err
	}
	if f.projectsErr != nil {
		return nil, f.projectsErr
	}
	if f.projects != nil {
		return f.projects, nil
	}
	return &coreapi.ListProjectsOutputBody{}, nil
}

func (f *fakeCellCore) ListProjectRepos(ctx context.Context, _ coreapi.ListProjectReposParams) (*coreapi.ListProjectReposOutputBody, error) {
	if err := f.waitIfBlocking(ctx); err != nil {
		return nil, err
	}
	if f.projectErr != nil {
		return nil, f.projectErr
	}
	if f.projectRepos != nil {
		return f.projectRepos, nil
	}
	return &coreapi.ListProjectReposOutputBody{}, nil
}

func (f *fakeCellCore) ListClusters(context.Context) (*coreapi.ListClustersOutputBody, error) {
	if f.clustersErr != nil {
		return nil, f.clustersErr
	}
	return &coreapi.ListClustersOutputBody{Clusters: f.clusters}, nil
}

func (f *fakeCellCore) ListRepos(ctx context.Context, params coreapi.ListReposParams) (*coreapi.ListReposOutputBody, error) {
	f.mu.Lock()
	f.lastListReposParams = params
	f.mu.Unlock()
	if err := f.waitIfBlocking(ctx); err != nil {
		return nil, err
	}
	if f.reposErr != nil {
		return nil, f.reposErr
	}
	if f.repos != nil {
		return f.repos, nil
	}
	return &coreapi.ListReposOutputBody{}, nil
}

func withFakeCellCore(t *testing.T, f *fakeCellCore) {
	t.Helper()
	prev := newCellCoreClient
	prevNative := newNativeRepoCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return f, nil }
	newNativeRepoCellCoreClient = func() (nativeRepoCellCoreClient, error) { return f, nil }
	t.Cleanup(func() {
		newCellCoreClient = prev
		newNativeRepoCellCoreClient = prevNative
	})
}

func TestResolveForgeRepoCellPlacement_NativeDoesNotSelectSameNamedGitHubMirror(t *testing.T) {
	const (
		projectID  = "01NATIVEPROJECT00000000000"
		nativeID   = "01NATIVEREPOSITORY00000000"
		legacyGHID = "01LEGACYGHMIRROR000000000"
	)
	withFakeCellCore(t, &fakeCellCore{
		projects: &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
			ID: projectID, Name: "entirehq",
		})},
		projectRepos: &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
			ID: nativeID, Name: "marvin", OwningProjectId: projectID,
		})},
		repo: &coreapi.Repo{
			ID: nativeID, Name: "marvin", OwningProjectId: projectID,
			ClusterHost: coreapi.NewOptString("eu.entire.io"),
		},
		clusters: euClusters(),
		// This is the forge-blind match the old implementation selected. The
		// native resolver must never consult it.
		repos: reposOutput(repoIndexFixture("entirehq/marvin", legacyGHID,
			placementFixture{id: legacyGHID, slug: usClusterSlug})),
	})

	got, err := resolveForgeRepoCellPlacement(t.Context(), nativeCloneForge, "entirehq", "marvin")
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoID != nativeID {
		t.Fatalf("RepoID = %q, want native repo ID %q (not legacy mirror %q)", got.RepoID, nativeID, legacyGHID)
	}
	if got.Target.BaseURL != euCellAPIURL || got.Target.Jurisdiction != "eu" {
		t.Fatalf("Target = %+v, want EU native repo cell", got.Target)
	}
}

func TestResolveForgeRepoCellPlacement_RequiresSupportedForge(t *testing.T) {
	t.Parallel()

	for _, forge := range []string{"", "github", "entire", "gl"} {
		t.Run(forge, func(t *testing.T) {
			t.Parallel()

			_, err := resolveForgeRepoCellPlacement(t.Context(), forge, "acme", "widgets")
			if err == nil || !strings.Contains(err.Error(), "unsupported forge") {
				t.Fatalf("resolveForgeRepoCellPlacement(%q) error = %v, want unsupported forge", forge, err)
			}
		})
	}
}

func TestResolveNativeRepoCellPlacement_ClassifiesDefinitiveMisses(t *testing.T) {
	const (
		projectID = "01NATIVEPROJECT00000000000"
		repoID    = "01NATIVEREPOSITORY00000000"
	)
	project := &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{ID: projectID, Name: "entirehq"})}
	projectRepo := &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{ID: repoID, Name: "marvin", OwningProjectId: projectID})}

	tests := []struct {
		name               string
		core               *fakeCellCore
		wantNotOnboarded   bool
		wantMessageSnippet string
	}{
		{
			name:               "project does not exist",
			core:               &fakeCellCore{},
			wantNotOnboarded:   true,
			wantMessageSnippet: "no project named",
		},
		{
			name:               "repo does not exist in project",
			core:               &fakeCellCore{projects: project},
			wantNotOnboarded:   true,
			wantMessageSnippet: "no repo named",
		},
		{
			name:               "repo has no cluster host",
			core:               &fakeCellCore{projects: project, projectRepos: projectRepo, repo: &coreapi.Repo{ID: repoID, Name: "marvin", OwningProjectId: projectID}},
			wantNotOnboarded:   true,
			wantMessageSnippet: "repo has no cluster host",
		},
		{
			name:               "control plane failure remains retryable",
			core:               &fakeCellCore{projectsErr: errors.New("core unavailable")},
			wantNotOnboarded:   false,
			wantMessageSnippet: "core unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeCellCore(t, tt.core)
			_, err := resolveNativeRepoCellPlacement(t.Context(), "entirehq", "marvin")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, errRepoNotOnboarded); got != tt.wantNotOnboarded {
				t.Fatalf("errors.Is(err, errRepoNotOnboarded) = %v, want %v (err: %v)", got, tt.wantNotOnboarded, err)
			}
			if !strings.Contains(err.Error(), tt.wantMessageSnippet) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantMessageSnippet)
			}
		})
	}
}

// euClusters is keyed by PublicUrl host, the join the ULID path uses
// (cellTargetForClusterHost / matchClusterByHost).
func euClusters() []coreapi.Cluster {
	return []coreapi.Cluster{
		{PublicUrl: "https://us.entire.io", Jurisdiction: "us", ApiUrl: coreapi.NewOptString("https://us.api.entire.io")},
		{PublicUrl: "https://eu.entire.io", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
	}
}

// clustersWithSlugs is keyed by Slug, the first join the owner/repo path uses
// (cellTargetForPlacement / matchClusterBySlug) against a RepoPlacement's
// ClusterSlug.
func clustersWithSlugs() []coreapi.Cluster {
	return []coreapi.Cluster{
		{Slug: usClusterSlug, Jurisdiction: "us", PublicUrl: "https://us.entire.io", ApiUrl: coreapi.NewOptString(usCellAPIURL)},
		{Slug: euClusterSlug, Jurisdiction: "eu", PublicUrl: "https://eu.entire.io", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
		{Slug: apSoutheastClusterSlug, Jurisdiction: "ap-southeast", PublicUrl: "https://ap-southeast.entire.io", ApiUrl: coreapi.NewOptString("https://aws-ap-southeast-1.api.entire.io")},
		{Slug: apSouthClusterSlug, Jurisdiction: "ap-south", PublicUrl: "https://ap-south.entire.io", ApiUrl: coreapi.NewOptString("https://aws-ap-south-1.api.entire.io")},
	}
}

// placementFixture is one entry of a RepoIndexEntry.Placements list.
type placementFixture struct {
	id   string
	slug string
	// cell is the placement's Cell name, the second join key
	// cellTargetForPlacement falls back to. Defaults to slug, which is the
	// common case; set it explicitly to exercise a slug that is missing from
	// the catalog.
	cell   string
	status coreapi.RepoPlacementStatus
}

// repoIndexFixture builds a RepoIndexEntry for fullName from an ordered list
// of placements, with processingID naming the placement that is
// primaries.processing. Order in placements is preserved, so a fixture can
// deliberately put the processing placement somewhere other than index 0 -
// proving a resolver picks it by id, not by position.
func repoIndexFixture(fullName, processingID string, placements ...placementFixture) coreapi.RepoIndexEntry {
	out := make([]coreapi.RepoPlacement, 0, len(placements))
	for _, p := range placements {
		status := p.status
		if status == "" {
			status = coreapi.RepoPlacementStatusReady
		}
		cell := p.cell
		if cell == "" {
			cell = p.slug
		}
		out = append(out, coreapi.RepoPlacement{
			ID:          p.id,
			ClusterSlug: p.slug,
			Cell:        cell,
			Status:      status,
		})
	}
	return coreapi.RepoIndexEntry{
		FullName:   fullName,
		ID:         "repo-" + fullName,
		Primaries:  coreapi.NewOptRepoPrimaries(coreapi.RepoPrimaries{Processing: processingID}),
		Placements: out,
	}
}

// fourRegionFixture reproduces entirehq/entire.io's real prod topology: four
// active placements, with the US one (deliberately not first in the list)
// naming primaries.processing. This is the shape that used to make
// resolveRepoClusterHost see >1 distinct active cluster host and refuse.
func fourRegionFixture() coreapi.RepoIndexEntry {
	return repoIndexFixture("entirehq/entire.io", "mirror-us",
		placementFixture{id: "mirror-eu", slug: euClusterSlug},
		placementFixture{id: "mirror-ap-southeast", slug: apSoutheastClusterSlug},
		placementFixture{id: "mirror-ap-south", slug: apSouthClusterSlug},
		placementFixture{id: "mirror-us", slug: usClusterSlug},
	)
}

// fourRegionFixtureProcessingInMiddle is the same real topology as
// fourRegionFixture but with the processing placement in the middle of the
// list, so neither a "first" nor a "last" positional heuristic could
// accidentally satisfy this test — only selection by primaries.processing id
// can.
func fourRegionFixtureProcessingInMiddle() coreapi.RepoIndexEntry {
	return repoIndexFixture("entirehq/entire.io", "mirror-us",
		placementFixture{id: "mirror-eu", slug: euClusterSlug},
		placementFixture{id: "mirror-us", slug: usClusterSlug},
		placementFixture{id: "mirror-ap-southeast", slug: apSoutheastClusterSlug},
		placementFixture{id: "mirror-ap-south", slug: apSouthClusterSlug},
	)
}

func reposOutput(entries ...coreapi.RepoIndexEntry) *coreapi.ListReposOutputBody {
	return &coreapi.ListReposOutputBody{Repos: entries}
}

// candidateRepoIndexFixture builds a RepoIndexEntry for a repo that ListRepos'
// Filter can find (it exists on the forge and the caller can see it) but that
// has never been onboarded to Entire: a Candidate is set, and there are no
// placements or primaries to resolve. This is the common "random repo a
// developer works in" shape, as opposed to the zero-rows "not found at all"
// shape.
func candidateRepoIndexFixture(fullName string) coreapi.RepoIndexEntry {
	return coreapi.RepoIndexEntry{
		FullName: fullName,
		ID:       "repo-" + fullName,
		Candidate: coreapi.NewOptRepoCandidate(coreapi.RepoCandidate{
			Access:      coreapi.RepoCandidateAccessRead,
			Onboardable: true,
		}),
	}
}

func TestResolveRepoCellTarget_ULID(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repo:     &coreapi.Repo{ID: "ULID", ClusterHost: coreapi.NewOptString("eu.entire.io")},
		clusters: euClusters(),
	})
	target, err := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("resolveRepoCellTarget: %v", err)
	}
	if target == nil {
		t.Fatal("expected a target for a resolvable ULID")
	}
	if target.BaseURL != euCellAPIURL || target.Jurisdiction != "eu" {
		t.Fatalf("target = %+v, want eu cell", target)
	}
}

func TestResolveRepoCellTarget_ULIDError_ReturnsError(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{repoErr: errors.New("boom"), clusters: euClusters()})
	target, err := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err == nil {
		t.Fatalf("expected an error on GetRepo failure, got target=%+v", target)
	}
	if target != nil {
		t.Fatalf("expected a nil target alongside the error, got %+v", target)
	}
}

func TestResolveRepoCellTarget_NoClusterMatch_ReturnsError(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repo:     &coreapi.Repo{ClusterHost: coreapi.NewOptString("ap.entire.io")}, // not in catalog
		clusters: euClusters(),
	})
	target, err := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err == nil {
		t.Fatalf("expected an error when no cluster matches, got target=%+v", target)
	}
	if target != nil {
		t.Fatalf("expected a nil target alongside the error, got %+v", target)
	}
}

func TestResolveRepoCellTarget_JurisdictionLowercased(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repo: &coreapi.Repo{ClusterHost: coreapi.NewOptString("eu.entire.io")},
		clusters: []coreapi.Cluster{
			{PublicUrl: "https://eu.entire.io", Jurisdiction: "EU", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
		},
	})
	target, err := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("resolveRepoCellTarget: %v", err)
	}
	if target == nil || target.Jurisdiction != "eu" {
		t.Fatalf("target = %+v, want lowercased jurisdiction eu", target)
	}
}

func TestResolveRepoCellTarget_OwnerRepo_SingleHomedRepo(t *testing.T) {
	fake := &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu",
			placementFixture{id: "mirror-eu", slug: euClusterSlug},
		)),
		clusters: clustersWithSlugs(),
	}
	withFakeCellCore(t, fake)
	target, err := resolveRepoCellTarget(context.Background(), "acme/widget", "")
	if err != nil {
		t.Fatalf("resolveRepoCellTarget: %v", err)
	}
	if target == nil || target.Jurisdiction != "eu" || target.BaseURL != euCellAPIURL {
		t.Fatalf("target = %+v, want eu cell", target)
	}
	// Proves the resolver actually sets Filter to the requested repo, not
	// just that the fake happens to return the right fixture regardless of
	// what was asked.
	if got := fake.lastListReposParams.Filter.Or(""); got != "acme/widget" {
		t.Errorf("ListRepos Filter = %q, want %q", got, "acme/widget")
	}
}

// This is the regression test for the actual production bug: a repo mirrored
// in 4 regions (entirehq/entire.io's real topology) used to make
// resolveRepoClusterHost see >1 distinct cluster host and refuse, falling
// back to home-jurisdiction routing (wrong-region silent failure for `entire
// trail`/`entire experts`). It must resolve to the PROCESSING placement's
// cell (us) regardless of how many other regions the repo is mirrored in.
func TestResolveRepoCellTarget_OwnerRepo_MultiHomedRepo_ResolvesProcessingCell(t *testing.T) {
	fixtures := map[string]coreapi.RepoIndexEntry{
		"processing last":   fourRegionFixture(),
		"processing middle": fourRegionFixtureProcessingInMiddle(),
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			withFakeCellCore(t, &fakeCellCore{
				repos:    reposOutput(fixture),
				clusters: clustersWithSlugs(),
			})
			target, err := resolveRepoCellTarget(context.Background(), "entirehq/entire.io", "")
			if err != nil {
				t.Fatalf("resolveRepoCellTarget: %v", err)
			}
			if target == nil {
				t.Fatal("expected a target for a multi-homed repo's processing cell")
			}
			if target.Jurisdiction != "us" || target.BaseURL != usCellAPIURL {
				t.Fatalf("target = %+v, want the us processing cell, not any of the other 3 mirrored regions", target)
			}
		})
	}
}

func TestResolveRepoCellTarget_OwnerRepo_JurisdictionLowercased(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu",
			placementFixture{id: "mirror-eu", slug: euClusterSlug},
		)),
		clusters: []coreapi.Cluster{
			{Slug: euClusterSlug, Jurisdiction: "EU", PublicUrl: "https://eu.entire.io", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
		},
	})
	target, err := resolveRepoCellTarget(context.Background(), "acme/widget", "")
	if err != nil {
		t.Fatalf("resolveRepoCellTarget: %v", err)
	}
	if target == nil || target.Jurisdiction != "eu" {
		t.Fatalf("target = %+v, want lowercased jurisdiction eu", target)
	}
}

// processingResolutionErrorCase names one way processing-placement
// resolution can fail. Shared between resolveRepoCellTarget's owner/repo path
// and resolveRepoCellPlacement, since both now go through
// resolveProcessingPlacement + cellTargetForPlacement.
type processingResolutionErrorCase struct {
	name        string
	repos       *coreapi.ListReposOutputBody
	reposErr    error
	clusters    []coreapi.Cluster
	clustersErr error
	// wantNotOnboarded marks the cases that must surface errRepoNotOnboarded
	// specifically (not just any error), so callers like the trail
	// enablement cache can tell "not onboarded" apart from a transient
	// placement failure.
	wantNotOnboarded bool
}

func processingResolutionErrorCases() []processingResolutionErrorCase {
	activePlacement := placementFixture{id: "mirror-eu", slug: euClusterSlug}

	return []processingResolutionErrorCase{
		{
			name:             "repo not found",
			repos:            reposOutput(), // zero rows
			clusters:         clustersWithSlugs(),
			wantNotOnboarded: true,
		},
		{
			// The more common real trigger for "not onboarded": a repo that
			// exists on the forge and is visible to the caller, but was never
			// onboarded to Entire. ListRepos' Filter bypasses the default
			// scope=onboarded restriction, so this comes back as a row with a
			// Candidate set instead of a zero-row response.
			name:             "repo discoverable but not onboarded (candidate row)",
			repos:            reposOutput(candidateRepoIndexFixture("acme/widget")),
			clusters:         clustersWithSlugs(),
			wantNotOnboarded: true,
		},
		{
			// Guards the identity check: the control plane's Filter param is
			// documented as an exact-match lookup, but nothing enforces that
			// here. If Filter were ever ignored/dropped server-side, an
			// unchecked Repos[0] would silently resolve an unrelated repo's
			// cell instead of erroring.
			name: "repo index returns a different repo than requested",
			repos: reposOutput(repoIndexFixture("some/other-repo", "mirror-eu",
				placementFixture{id: "mirror-eu", slug: euClusterSlug},
			)),
			clusters: clustersWithSlugs(),
		},
		{
			name: "no processing primary",
			repos: reposOutput(func() coreapi.RepoIndexEntry {
				e := repoIndexFixture("acme/widget", "", activePlacement)
				e.Primaries = coreapi.OptRepoPrimaries{} // unset
				return e
			}()),
			clusters:         clustersWithSlugs(),
			wantNotOnboarded: true,
		},
		{
			// The defensive branch: primaries names a processing placement that
			// is absent from the same response's placement list. Should not be
			// reachable via the control plane, but it is the difference between
			// a clear error and a zero-value placement resolving to no cell.
			name: "processing placement id absent from the placement list",
			repos: reposOutput(repoIndexFixture("acme/widget", "mirror-vanished",
				placementFixture{id: "mirror-eu", slug: euClusterSlug},
			)),
			clusters: clustersWithSlugs(),
		},
		{
			name: "processing placement failed",
			repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu",
				placementFixture{id: "mirror-eu", slug: euClusterSlug, status: coreapi.RepoPlacementStatusFailed},
			)),
			clusters: clustersWithSlugs(),
		},
		{
			name: "processing placement suspended",
			repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu",
				placementFixture{id: "mirror-eu", slug: euClusterSlug, status: coreapi.RepoPlacementStatusSuspended},
			)),
			clusters: clustersWithSlugs(),
		},
		{
			name:     "list repos errors",
			reposErr: errors.New("core unavailable"),
			clusters: clustersWithSlugs(),
		},
		{
			name:        "list clusters errors",
			repos:       reposOutput(repoIndexFixture("acme/widget", "mirror-eu", activePlacement)),
			clustersErr: errors.New("core unavailable"),
		},
		{
			name:  "no cluster matches the placement's slug",
			repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu", activePlacement)),
			clusters: []coreapi.Cluster{
				{Slug: "some-other-slug", Jurisdiction: "us", PublicUrl: "https://us.entire.io", ApiUrl: coreapi.NewOptString("https://us.api.entire.io")},
			},
		},
	}
}

func TestResolveRepoCellTarget_OwnerRepo_ProcessingResolutionErrors(t *testing.T) {
	for _, tc := range processingResolutionErrorCases() {
		t.Run(tc.name, func(t *testing.T) {
			withFakeCellCore(t, &fakeCellCore{repos: tc.repos, reposErr: tc.reposErr, clusters: tc.clusters, clustersErr: tc.clustersErr})
			target, err := resolveRepoCellTarget(context.Background(), "acme/widget", "")
			if err == nil {
				t.Fatalf("expected an error, got target=%+v", target)
			}
			if target != nil {
				t.Fatalf("expected a nil target alongside the error, got %+v", target)
			}
			if got := errors.Is(err, errRepoNotOnboarded); got != tc.wantNotOnboarded {
				t.Fatalf("errors.Is(err, errRepoNotOnboarded) = %v, want %v (err: %v)", got, tc.wantNotOnboarded, err)
			}
		})
	}
}

func TestResolveRepoCellPlacement_ProcessingResolutionErrors(t *testing.T) {
	for _, tc := range processingResolutionErrorCases() {
		t.Run(tc.name, func(t *testing.T) {
			withFakeCellCore(t, &fakeCellCore{repos: tc.repos, reposErr: tc.reposErr, clusters: tc.clusters, clustersErr: tc.clustersErr})
			_, err := resolveRepoCellPlacement(context.Background(), "acme", "widget")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := errors.Is(err, errRepoNotOnboarded); got != tc.wantNotOnboarded {
				t.Fatalf("errors.Is(err, errRepoNotOnboarded) = %v, want %v (err: %v)", got, tc.wantNotOnboarded, err)
			}
		})
	}
}

// Bugbot (PR #1942): cross-repo reads must take the repo_id and the cell from
// the SAME placement. A mirror id is only resolvable by the cell holding that
// placement, so pairing one placement's id with a cell picked another way
// asks a cell about an id it has never seen. With a single placement this is
// trivially true; TestResolveRepoCellPlacement_MultiHomedRepo_ResolvesProcessingPlacement
// below is the version of this invariant that actually exercises a choice.
func TestResolveRepoCellPlacement_PairsIDWithItsOwnCell(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu",
			placementFixture{id: "mirror-eu", slug: euClusterSlug},
		)),
		clusters: clustersWithSlugs(),
	})
	got, err := resolveRepoCellPlacement(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("resolveRepoCellPlacement: %v", err)
	}
	if got.RepoID != "mirror-eu" {
		t.Errorf("RepoID = %q, want mirror-eu", got.RepoID)
	}
	if got.Target == nil || got.Target.Jurisdiction != "eu" || got.Target.BaseURL != euCellAPIURL {
		t.Fatalf("Target = %+v, want the eu cell that holds mirror-eu", got.Target)
	}
}

// Both placement resolvers share one ID matcher (placementByID), so a
// whitespace-padded placement id resolves the same way here as in the search
// fan-out. Before they shared it, this resolver compared a trimmed
// primaries.processing against an untrimmed p.ID and reported the repo as
// having no processing placement at all. RepoID must come back trimmed: it
// becomes a path segment in entire-api URLs.
func TestResolveRepoCellPlacement_ToleratesPaddedPlacementID(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "mirror-eu",
			placementFixture{id: " mirror-eu ", slug: euClusterSlug},
		)),
		clusters: clustersWithSlugs(),
	})
	got, err := resolveRepoCellPlacement(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("resolveRepoCellPlacement: %v", err)
	}
	if got.RepoID != "mirror-eu" {
		t.Errorf("RepoID = %q, want the trimmed mirror-eu", got.RepoID)
	}
}

// A multi-homed repo must resolve to its PROCESSING placement specifically,
// not merely "some" placement (the pre-fix behavior in resolveRepoCellTarget
// was to refuse; resolveRepoCellPlacement's pre-fix behavior was to take
// whichever active mirror came first). The fixture orders the processing
// placement (mirror-us) last, so a resolver that still picked "first active"
// would fail this test.
func TestResolveRepoCellPlacement_MultiHomedRepo_ResolvesProcessingPlacement(t *testing.T) {
	fixtures := map[string]coreapi.RepoIndexEntry{
		"processing last":   fourRegionFixture(),
		"processing middle": fourRegionFixtureProcessingInMiddle(),
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			withFakeCellCore(t, &fakeCellCore{
				repos:    reposOutput(fixture),
				clusters: clustersWithSlugs(),
			})
			got, err := resolveRepoCellPlacement(context.Background(), "entirehq", "entire.io")
			if err != nil {
				t.Fatalf("resolveRepoCellPlacement: %v", err)
			}
			if got.RepoID != "mirror-us" {
				t.Errorf("RepoID = %q, want mirror-us (the processing placement, regardless of its position in the list)", got.RepoID)
			}
			if got.Target == nil || got.Target.Jurisdiction != "us" || got.Target.BaseURL != usCellAPIURL {
				t.Fatalf("Target = %+v, want the us cell that holds mirror-us", got.Target)
			}
		})
	}
}

// The not-onboarded error is what a user in a non-onboarded repo sees, and the
// fail-loud path made it the most common failure — so it must not read like a
// stack trace. Each resolution layer naming the repo produced
// "resolve processing placement for acme/widget: acme/widget: repo is not
// onboarded to Entire"; the name belongs to exactly one layer.
func TestResolveRepoCellPlacement_NotOnboardedNamesTheRepoOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		repos *coreapi.ListReposOutputBody
	}{
		{name: "no row in the repos index", repos: reposOutput()},
		{name: "candidate row", repos: reposOutput(candidateRepoIndexFixture("acme/widget"))},
		{
			name: "row with no processing primary",
			repos: reposOutput(func() coreapi.RepoIndexEntry {
				e := repoIndexFixture("acme/widget", "", placementFixture{id: "mirror-eu", slug: euClusterSlug})
				e.Primaries = coreapi.OptRepoPrimaries{}
				return e
			}()),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeCellCore(t, &fakeCellCore{repos: tc.repos, clusters: clustersWithSlugs()})

			_, err := resolveRepoCellPlacement(context.Background(), "acme", "widget")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := strings.Count(err.Error(), "acme/widget"); got != 1 {
				t.Errorf("error = %q names the repo %d times, want exactly 1", err, got)
			}
		})
	}
}

// Trail #1003 finding: resolveRepoCellPlacement was the one cell-resolution
// entry point with no deadline, and the coreapi HTTP client sets only a dial
// timeout — so a reachable-but-slow control plane stalled `explain --repo`
// indefinitely, before its spinner had even started. The parent deadline here
// is shorter than requiredCellResolveTimeout, which is what keeps this test
// fast; the point is that the wait is bounded and reported as a timeout. The
// blocking now happens on ListRepos (the processing-placement lookup), not
// ListMirrors.
func TestResolveRepoCellPlacement_BoundedByDeadline(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{blockUntilCtxDone: true, clusters: clustersWithSlugs()})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := resolveRepoCellPlacement(ctx, "acme", "widget")
	if err == nil {
		t.Fatal("expected a timeout error from a stalled control plane")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("resolveRepoCellPlacement waited %s; the lookup is not bounded", elapsed)
	}
	// Reported as a timeout, not as a missing mirror — the whole point is that
	// the user looks at the control plane rather than at their mirrors.
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to name a timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %q, want it to wrap context.DeadlineExceeded", err)
	}
}

// The ULID path resolves a placement the caller already named, so it skips the
// processing-placement lookup entirely — but a stalled control plane must still
// report a timeout as a timeout, in the same words as the owner/repo path. The
// two paths reaching different wording for the same failure is what made the
// shared cellPlacementError worth routing both through.
func TestResolveRepoCellTarget_ULIDPathReportsTimeoutAsTimeout(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{blockUntilCtxDone: true, clusters: euClusters()})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := resolveRepoCellTarget(ctx, "", "01J000000000000000000000")
	if err == nil {
		t.Fatal("expected a timeout error from a stalled control plane")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("resolveRepoCellTarget waited %s; the ULID lookup is not bounded", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to name a timeout like the owner/repo path does", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %q, want it to wrap context.DeadlineExceeded", err)
	}
}

// A cancelled context is a Ctrl+C, not a slow control plane; calling it a
// timeout sends the user debugging the wrong thing.
func TestCellPlacementError_CancellationIsNotReportedAsTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fallback := fmt.Errorf("list repos: %w", context.Canceled)
	err := cellPlacementError(ctx, "acme/widget", fallback)

	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want a cancellation not to be labelled a timeout", err)
	}
	// The Canceled chain has to survive: renderDataAPIAuthError silences on it.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %q, want it to still wrap context.Canceled", err)
	}
}

// The command cannot proceed without this lookup, so its budget is separate
// from the best-effort one that silently degrades to home-jurisdiction routing.
func TestRequiredCellResolveTimeoutIsItsOwnBudget(t *testing.T) {
	t.Parallel()

	if requiredCellResolveTimeout <= cellResolveTimeout {
		t.Errorf("requiredCellResolveTimeout (%s) should be more patient than the best-effort cellResolveTimeout (%s): a timeout here fails the command instead of degrading",
			requiredCellResolveTimeout, cellResolveTimeout)
	}
}

// A placement whose ClusterSlug is absent from the catalog must still resolve
// via its Cell name, the way the repo-SET path (resolveCellBaseURLs ->
// matchClusterByCellInURL) already does. Before this tier existed, the
// single-repo path hard-failed on exactly the catalog gap the fan-out was
// written to tolerate, making `entire trail`/`experts`/`explain --repo`
// strictly narrower than `entire search` on the same data.
func TestResolveRepoCellTarget_OwnerRepo_FallsBackToCellNameWhenSlugMissing(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "placement-1",
			placementFixture{id: "placement-1", slug: "slug-not-in-catalog", cell: "aws-ap-south-1"},
		)),
		clusters: clustersWithSlugs(),
	})

	target, err := resolveRepoCellTarget(context.Background(), "acme/widget", "")
	if err != nil {
		t.Fatalf("resolveRepoCellTarget: %v", err)
	}
	if target == nil {
		t.Fatal("expected a target resolved via the placement's cell name")
	}
	if target.Jurisdiction != "ap-south" {
		t.Errorf("jurisdiction = %q, want ap-south (the cluster whose apiUrl host carries the cell name)", target.Jurisdiction)
	}
}

// The slug join still wins when it matches, so adding the cell tier cannot
// silently reroute a placement whose slug is in the catalog.
func TestResolveRepoCellTarget_OwnerRepo_PrefersSlugOverCellName(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "placement-1",
			// Slug says EU; the cell name would match the ap-south cluster's
			// apiUrl host. Slug must win.
			placementFixture{id: "placement-1", slug: euClusterSlug, cell: "aws-ap-south-1"},
		)),
		clusters: clustersWithSlugs(),
	})

	target, err := resolveRepoCellTarget(context.Background(), "acme/widget", "")
	if err != nil {
		t.Fatalf("resolveRepoCellTarget: %v", err)
	}
	if target.Jurisdiction != "eu" {
		t.Errorf("jurisdiction = %q, want eu (the slug match, not the cell-name fallback)", target.Jurisdiction)
	}
}

// Neither join matching is still a loud failure: the deliberate difference from
// the fan-out, which degrades to jurisdiction routing here. That degradation is
// the wrong-region "success" this resolver exists to refuse.
func TestResolveRepoCellTarget_OwnerRepo_NoJurisdictionFallback(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repos: reposOutput(repoIndexFixture("acme/widget", "placement-1",
			placementFixture{id: "placement-1", slug: "slug-not-in-catalog", cell: "cell-not-in-any-url"},
		)),
		clusters: clustersWithSlugs(),
	})

	target, err := resolveRepoCellTarget(context.Background(), "acme/widget", "")
	if err == nil {
		t.Fatalf("expected an error rather than jurisdiction-default routing, got target %+v", target)
	}
	if target != nil {
		t.Errorf("target = %+v, want nil", target)
	}
	if !strings.Contains(err.Error(), "cell-not-in-any-url") {
		t.Errorf("error = %q, want it to name the cell it could not place", err)
	}
}
