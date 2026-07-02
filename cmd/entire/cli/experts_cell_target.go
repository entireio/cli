package cli

import (
	"context"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// expertsCellResolveTimeout bounds the best-effort control-plane lookups that
// pick a repo's cell. Without it, a reachable-but-hung control plane would block
// the whole experts command instead of degrading to home-jurisdiction routing —
// the "any failure falls back" contract must hold for slow cores, not just
// erroring ones.
const expertsCellResolveTimeout = 5 * time.Second

// expertsCoreClient is the control-plane surface the cell-target resolver needs.
// An interface (with a swappable constructor) so the resolver is unit-testable
// against a fake control plane; *coreapi.Client satisfies it.
type expertsCoreClient interface {
	GetRepo(ctx context.Context, params coreapi.GetRepoParams) (*coreapi.Repo, error)
	ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error)
	ListMirrors(ctx context.Context, params coreapi.ListMirrorsParams) (*coreapi.ListMirrorsOutputBody, error)
}

// newExpertsCoreClient builds the control-plane client used for cell resolution.
// Swapped in tests.
var newExpertsCoreClient = func() (expertsCoreClient, error) { return coreapi.New() }

// resolveExpertsCellTarget resolves the entire-api cell that HOSTS the given
// repo, plus that cell's jurisdiction, so a repo-scoped experts call reaches the
// region that owns the repo — mirroring how the entire.io BFF selects a cell per
// repo (resolve-cluster-host.ts / repos-stream.ts) rather than using the
// caller's home cell.
//
// It is deliberately best-effort: ANY failure (not logged in, control-plane
// error or timeout, unknown/ambiguous placement, missing apiUrl) returns nil,
// and the auth-layer client falls back to home-jurisdiction routing. That
// fallback is exactly the previous behaviour, so this can never regress the
// common same-region case (where the repo's jurisdiction equals the caller's
// home). A short deadline keeps a slow control plane from stalling the command.
//
// Placement source:
//   - ulid form: coreapi.GetRepo(ulid) -> Repo.ClusterHost;
//   - owner/repo form: coreapi mirrors filtered to this repo -> ClusterHost.
//
// The cluster host is then mapped to a cell apiUrl + jurisdiction via the
// coreapi cluster catalog (ListClusters), the authoritative source for a
// jurisdiction's cell URL.
func resolveExpertsCellTarget(ctx context.Context, fullName, ulid string) *auth.CellTarget {
	ctx, cancel := context.WithTimeout(ctx, expertsCellResolveTimeout)
	defer cancel()

	c, err := newExpertsCoreClient()
	if err != nil {
		logging.Debug(ctx, "experts cell target: core client unavailable, using home-jurisdiction routing", "error", err.Error())
		return nil
	}

	clusterHost, ok := resolveRepoClusterHost(ctx, c, fullName, ulid)
	if !ok || clusterHost == "" {
		return nil
	}

	clusters, err := c.ListClusters(ctx)
	if err != nil {
		logging.Debug(ctx, "experts cell target: list clusters failed, using home-jurisdiction routing", "error", err.Error())
		return nil
	}
	cluster, ok := matchClusterByHost(clusters.Clusters, clusterHost)
	if !ok {
		logging.Debug(ctx, "experts cell target: no cluster matched repo host, using home-jurisdiction routing", "cluster_host", clusterHost)
		return nil
	}
	apiURL := strings.TrimRight(strings.TrimSpace(cluster.ApiUrl.Or("")), "/")
	// DNS is case-insensitive; normalise the catalog jurisdiction so a
	// non-lowercase value still passes the auth layer's strict label check
	// instead of hard-failing the target path.
	jurisdiction := strings.ToLower(strings.TrimSpace(cluster.Jurisdiction))
	if apiURL == "" || jurisdiction == "" {
		logging.Debug(ctx, "experts cell target: matched cluster missing apiUrl/jurisdiction, using home-jurisdiction routing", "cluster_host", clusterHost)
		return nil
	}
	return &auth.CellTarget{BaseURL: apiURL, Jurisdiction: jurisdiction}
}

// resolveRepoClusterHost finds the public cluster host that owns the repo. It
// returns ok=false to signal "fall back to home-jurisdiction routing" for every
// unresolved or ambiguous case, never an error.
func resolveRepoClusterHost(ctx context.Context, c expertsCoreClient, fullName, ulid string) (string, bool) {
	if strings.TrimSpace(ulid) != "" {
		repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: ulid})
		if err != nil {
			logging.Debug(ctx, "experts cell target: GetRepo failed, using home-jurisdiction routing", "error", err.Error())
			return "", false
		}
		return strings.TrimSpace(repo.ClusterHost.Or("")), true
	}

	owner, repo, ok := strings.Cut(strings.TrimSpace(fullName), "/")
	if !ok || owner == "" || repo == "" {
		return "", false
	}
	mirrors, err := listMirrorsForRepo(ctx, c, mirrorCloneProviderGitHub, strings.ToLower(owner), strings.ToLower(repo))
	if err != nil {
		logging.Debug(ctx, "experts cell target: list mirrors failed, using home-jurisdiction routing", "error", err.Error())
		return "", false
	}
	hosts := distinctActiveClusterHosts(mirrors)
	if len(hosts) != 1 {
		// Zero placements (not mirrored / unknown) or multiple regions
		// (ambiguous which cell holds the experts data): fall back rather than
		// guess a region.
		if len(hosts) > 1 {
			logging.Debug(ctx, "experts cell target: repo mirrored in multiple regions, using home-jurisdiction routing", "count", len(hosts))
		}
		return "", false
	}
	return hosts[0], true
}

// distinctActiveClusterHosts returns the set of cluster hosts a repo is actively
// serviced on: excluding archived placements and unhealthy ones (failed /
// suspended clone status), since those cells can't answer experts for the repo.
// A failed/suspended placement must neither manufacture false cross-region
// ambiguity nor become a sole "active" host. Case-folded to match DNS
// semantics; order is unimportant (callers only use a single-member set).
func distinctActiveClusterHosts(mirrors []coreapi.Mirror) []string {
	seen := make(map[string]string, len(mirrors))
	for _, m := range mirrors {
		if m.IsArchived.Or(false) {
			continue
		}
		// Treat unset status as active (older data); only failed/suspended
		// placements can't serve experts for the repo.
		if st := m.Status.Or(coreapi.MirrorStatusReady); st == coreapi.MirrorStatusFailed || st == coreapi.MirrorStatusSuspended {
			continue
		}
		host := strings.TrimSpace(m.ClusterHost)
		if host == "" {
			continue
		}
		key := strings.ToLower(host)
		if _, ok := seen[key]; !ok {
			seen[key] = host
		}
	}
	out := make([]string, 0, len(seen))
	for _, host := range seen {
		out = append(out, host)
	}
	return out
}

// matchClusterByHost finds the catalog cluster whose public host equals
// clusterHost (case-insensitive). The cluster's apiUrl + jurisdiction are the
// authoritative cell coordinates.
func matchClusterByHost(clusters []coreapi.Cluster, clusterHost string) (coreapi.Cluster, bool) {
	want := strings.ToLower(strings.TrimSpace(clusterHost))
	if want == "" {
		return coreapi.Cluster{}, false
	}
	for _, cl := range clusters {
		host, err := hostFromPublicURL(cl.PublicUrl)
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(host), want) {
			return cl, true
		}
	}
	return coreapi.Cluster{}, false
}
