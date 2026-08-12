package cli

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/internal/coreapi"
)

const euCellAPIURL = "https://eu.api.entire.io"

const euWestCell = "aws-eu-west-1"

func TestDistinctActiveClusterHosts(t *testing.T) {
	t.Parallel()
	mirrors := []coreapi.Mirror{
		{ClusterHost: "aws-us-east-2.entire.io"},
		{ClusterHost: "AWS-US-EAST-2.entire.io"}, // dup (case-insensitive) → collapses
		{ClusterHost: "aws-eu-west-1.entire.io"}, // distinct active
		// Unique host that is archived → must be excluded (observably absent).
		{ClusterHost: "aws-ap-south-1.entire.io", IsArchived: coreapi.NewOptBool(true)},
		// Unique host with a failed clone → excluded (can't serve experts).
		{ClusterHost: "aws-sa-east-1.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusFailed)},
		// Unique host suspended → excluded.
		{ClusterHost: "aws-ca-central-1.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusSuspended)},
		{ClusterHost: ""}, // empty → excluded
	}
	got := distinctActiveClusterHosts(mirrors)
	sort.Strings(got)
	want := []string{"aws-eu-west-1.entire.io", "aws-us-east-2.entire.io"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("distinctActiveClusterHosts = %v, want %v", got, want)
	}
}

func TestDistinctActiveClusterHosts_AllInactive(t *testing.T) {
	t.Parallel()
	mirrors := []coreapi.Mirror{
		{ClusterHost: "aws-us-east-2.entire.io", IsArchived: coreapi.NewOptBool(true)},
		{ClusterHost: "aws-eu-west-1.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusFailed)},
	}
	if got := distinctActiveClusterHosts(mirrors); len(got) != 0 {
		t.Fatalf("distinctActiveClusterHosts = %v, want empty", got)
	}
}

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

// TestClusterHostJoin exercises the realistic invariant that a mirror's
// ClusterHost joins to a cluster whose PublicUrl host equals it — the actual
// key the resolver relies on.
func TestClusterHostJoin(t *testing.T) {
	t.Parallel()
	mirrors := []coreapi.Mirror{{ClusterHost: "eu.entire.io", Repo: "widget"}}
	clusters := []coreapi.Cluster{
		{PublicUrl: "https://us.entire.io", Jurisdiction: "us", ApiUrl: coreapi.NewOptString("https://us.api.entire.io")},
		{PublicUrl: "https://eu.entire.io", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
	}
	hosts := distinctActiveClusterHosts(mirrors)
	if len(hosts) != 1 {
		t.Fatalf("hosts = %v, want 1", hosts)
	}
	cl, ok := matchClusterByHost(clusters, hosts[0])
	if !ok || cl.Jurisdiction != "eu" || cl.ApiUrl.Or("") != euCellAPIURL {
		t.Fatalf("join failed: ok=%v cluster=%+v", ok, cl)
	}
}

// fakeCellCore is a stub control plane for resolveRepoCellTarget tests.
type fakeCellCore struct {
	repo        *coreapi.Repo
	repoErr     error
	mirrors     []coreapi.Mirror
	mirrorsErr  error
	clusters    []coreapi.Cluster
	clustersErr error
	// blockUntilCtxDone makes the lookups hang until the caller's deadline
	// fires, standing in for a reachable-but-slow control plane. Off by
	// default, so existing tests are unaffected.
	blockUntilCtxDone bool
}

// waitIfBlocking simulates a core that accepts the connection and then stalls.
func (f *fakeCellCore) waitIfBlocking(ctx context.Context) error {
	if !f.blockUntilCtxDone {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeCellCore) GetRepo(context.Context, coreapi.GetRepoParams) (*coreapi.Repo, error) {
	return f.repo, f.repoErr
}

func (f *fakeCellCore) ListClusters(context.Context) (*coreapi.ListClustersOutputBody, error) {
	if f.clustersErr != nil {
		return nil, f.clustersErr
	}
	return &coreapi.ListClustersOutputBody{Clusters: f.clusters}, nil
}

func (f *fakeCellCore) ListMirrors(ctx context.Context, _ coreapi.ListMirrorsParams) (*coreapi.ListMirrorsOutputBody, error) {
	if err := f.waitIfBlocking(ctx); err != nil {
		return nil, err
	}
	if f.mirrorsErr != nil {
		return nil, f.mirrorsErr
	}
	return &coreapi.ListMirrorsOutputBody{Mirrors: f.mirrors}, nil
}

func withFakeCellCore(t *testing.T, f *fakeCellCore) {
	t.Helper()
	prev := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return f, nil }
	t.Cleanup(func() { newCellCoreClient = prev })
}

func euClusters() []coreapi.Cluster {
	return []coreapi.Cluster{
		{PublicUrl: "https://us.entire.io", Jurisdiction: "us", ApiUrl: coreapi.NewOptString("https://us.api.entire.io")},
		{PublicUrl: "https://eu.entire.io", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
	}
}

func TestResolveRepoCellTarget_ULID(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repo:     &coreapi.Repo{ID: "ULID", ClusterHost: coreapi.NewOptString("eu.entire.io")},
		clusters: euClusters(),
	})
	target := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if target == nil {
		t.Fatal("expected a target for a resolvable ULID")
	}
	if target.BaseURL != euCellAPIURL || target.Jurisdiction != "eu" {
		t.Fatalf("target = %+v, want eu cell", target)
	}
}

func TestResolveRepoCellTarget_ULIDError_FallsBack(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{repoErr: errors.New("boom"), clusters: euClusters()})
	if target := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV"); target != nil {
		t.Fatalf("expected nil (fallback) on GetRepo error, got %+v", target)
	}
}

func TestResolveRepoCellTarget_OwnerRepoSingleRegion(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		mirrors: []coreapi.Mirror{
			{Repo: "widget", ClusterHost: "eu.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)},
			// A failed placement in another region must be ignored, not create ambiguity.
			{Repo: "widget", ClusterHost: "us.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusFailed)},
			// A different repo must be filtered out by listMirrorsForRepo.
			{Repo: "other", ClusterHost: "us.entire.io"},
		},
		clusters: euClusters(),
	})
	target := resolveRepoCellTarget(context.Background(), "acme/widget", "")
	if target == nil || target.Jurisdiction != "eu" || target.BaseURL != euCellAPIURL {
		t.Fatalf("target = %+v, want eu cell", target)
	}
}

func TestResolveRepoCellTarget_MultiRegion_FallsBack(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		mirrors: []coreapi.Mirror{
			{Repo: "widget", ClusterHost: "eu.entire.io"},
			{Repo: "widget", ClusterHost: "us.entire.io"},
		},
		clusters: euClusters(),
	})
	if target := resolveRepoCellTarget(context.Background(), "acme/widget", ""); target != nil {
		t.Fatalf("expected nil (fallback) for ambiguous multi-region repo, got %+v", target)
	}
}

func TestResolveRepoCellTarget_NoClusterMatch_FallsBack(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repo:     &coreapi.Repo{ClusterHost: coreapi.NewOptString("ap.entire.io")}, // not in catalog
		clusters: euClusters(),
	})
	if target := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV"); target != nil {
		t.Fatalf("expected nil (fallback) when no cluster matches, got %+v", target)
	}
}

func TestResolveRepoCellTarget_JurisdictionLowercased(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		repo: &coreapi.Repo{ClusterHost: coreapi.NewOptString("eu.entire.io")},
		clusters: []coreapi.Cluster{
			{PublicUrl: "https://eu.entire.io", Jurisdiction: "EU", ApiUrl: coreapi.NewOptString(euCellAPIURL)},
		},
	})
	target := resolveRepoCellTarget(context.Background(), "", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if target == nil || target.Jurisdiction != "eu" {
		t.Fatalf("target = %+v, want lowercased jurisdiction eu", target)
	}
}

// Bugbot (PR #1942): cross-repo reads must take the repo_id and the cell from
// the SAME placement. A mirror id is only resolvable by the cell holding that
// placement, so pairing one placement's id with a cell picked another way (the
// caller's home cell, which is what resolveRepoCellTarget falls back to for a
// multi-region repo) asks a cell about an id it has never seen.
func TestResolveRepoCellPlacement_PairsIDWithItsOwnCell(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		mirrors: []coreapi.Mirror{
			{Repo: "widget", MirrorId: "mirror-eu", ClusterHost: "eu.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)},
		},
		clusters: euClusters(),
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

// A multi-region repo must still resolve to a real placement's cell. This is the
// case resolveRepoCellTarget deliberately refuses (returning nil so the auth
// layer routes to the caller's home cell) — for a repo_id-keyed read that
// fallback is a wrong-cell 404, so this path picks a placement instead.
func TestResolveRepoCellPlacement_MultiRegionPicksAPlacement(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		mirrors: []coreapi.Mirror{
			{Repo: "widget", MirrorId: "mirror-eu", ClusterHost: "eu.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)},
			{Repo: "widget", MirrorId: "mirror-us", ClusterHost: "us.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)},
		},
		clusters: euClusters(),
	})
	got, err := resolveRepoCellPlacement(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("resolveRepoCellPlacement: %v", err)
	}
	// Whichever placement is chosen, its id and its cell must agree.
	wantJurisdiction := map[string]string{"mirror-eu": "eu", "mirror-us": "us"}[got.RepoID]
	if wantJurisdiction == "" {
		t.Fatalf("RepoID = %q, want one of the two placements", got.RepoID)
	}
	if got.Target == nil || got.Target.Jurisdiction != wantJurisdiction {
		t.Fatalf("Target = %+v, want the %s cell to match %s", got.Target, wantJurisdiction, got.RepoID)
	}
}

// An inactive placement can't serve the repo, so it must not be chosen.
func TestResolveRepoCellPlacement_SkipsInactiveAndUnplaceable(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{
		mirrors: []coreapi.Mirror{
			{Repo: "widget", MirrorId: "mirror-failed", ClusterHost: "eu.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusFailed)},
			// Not in the cluster catalog: unplaceable, so skip rather than fall
			// back to a cell that doesn't know this id.
			{Repo: "widget", MirrorId: "mirror-unknown", ClusterHost: "nowhere.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)},
			{Repo: "widget", MirrorId: "mirror-eu", ClusterHost: "eu.entire.io", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)},
		},
		clusters: euClusters(),
	})
	got, err := resolveRepoCellPlacement(context.Background(), "acme", "widget")
	if err != nil {
		t.Fatalf("resolveRepoCellPlacement: %v", err)
	}
	if got.RepoID != "mirror-eu" {
		t.Errorf("RepoID = %q, want mirror-eu (the only active, placeable mirror)", got.RepoID)
	}
}

func TestResolveRepoCellPlacement_NoMirror(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{clusters: euClusters()})
	if _, err := resolveRepoCellPlacement(context.Background(), "acme", "widget"); err == nil {
		t.Fatal("expected an error when the repo has no reachable mirror")
	}
}

// Trail #1003 finding: resolveRepoCellPlacement was the one cell-resolution
// entry point with no deadline, and the coreapi HTTP client sets only a dial
// timeout — so a reachable-but-slow control plane stalled `explain --repo`
// indefinitely, before its spinner had even started. The parent deadline here
// is shorter than requiredCellResolveTimeout, which is what keeps this test
// fast; the point is that the wait is bounded and reported as a timeout.
func TestResolveRepoCellPlacement_BoundedByDeadline(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{blockUntilCtxDone: true, clusters: euClusters()})

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

// The command cannot proceed without this lookup, so its budget is separate
// from the best-effort one that silently degrades to home-jurisdiction routing.
func TestRequiredCellResolveTimeoutIsItsOwnBudget(t *testing.T) {
	t.Parallel()

	if requiredCellResolveTimeout <= cellResolveTimeout {
		t.Errorf("requiredCellResolveTimeout (%s) should be more patient than the best-effort cellResolveTimeout (%s): a timeout here fails the command instead of degrading",
			requiredCellResolveTimeout, cellResolveTimeout)
	}
}
