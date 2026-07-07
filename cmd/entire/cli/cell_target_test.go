package cli

import (
	"context"
	"errors"
	"sort"
	"testing"

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

func (f *fakeCellCore) ListMirrors(context.Context, coreapi.ListMirrorsParams) (*coreapi.ListMirrorsOutputBody, error) {
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
