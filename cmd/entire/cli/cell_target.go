package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

// cellResolveTimeout bounds the best-effort control-plane lookups that pick a
// repo's cell for a multi-repo fan-out (cell_fanout.go's resolveCellBaseURLs).
// Without it, a reachable-but-hung control plane would block the calling
// command instead of degrading to jurisdiction routing — the "any failure
// falls back" contract must hold for slow cores, not just erroring ones.
const cellResolveTimeout = 5 * time.Second

// requiredCellResolveTimeout bounds the control-plane lookups a repo-scoped
// cell read cannot proceed without: resolveRepoCellTarget (trails, experts)
// and resolveRepoCellPlacement (checkpoint/explain --repo).
//
// Deliberately not cellResolveTimeout: that budget bounds *best-effort*
// lookups whose failure silently degrades to jurisdiction routing, so it can
// be aggressive. Here a timeout is a visible command failure, and the budget
// has to cover two sequential control-plane round trips (a repo lookup, then
// the cluster catalog), so it is more patient.
//
// Some deadline is required: the coreapi HTTP client sets only a dial timeout,
// so a reachable-but-slow core is otherwise unbounded, and this runs before the
// command's spinner starts — the user would sit in front of a silent terminal.
const requiredCellResolveTimeout = 15 * time.Second

// cellCoreClient is the control-plane surface the cell-target resolver needs.
// An interface (with a swappable constructor) so the resolver is unit-testable
// against a fake control plane; *coreapi.Client satisfies it.
type cellCoreClient interface {
	GetRepo(ctx context.Context, params coreapi.GetRepoParams) (*coreapi.Repo, error)
	ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error)
	ListRepos(ctx context.Context, params coreapi.ListReposParams) (*coreapi.ListReposOutputBody, error)
}

type nativeRepoCellCoreClient interface {
	nativeRepoResolverClient
	ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error)
	ListRepos(ctx context.Context, params coreapi.ListReposParams) (*coreapi.ListReposOutputBody, error)
}

// newCellCoreClient builds the control-plane client used for cell resolution.
// Swapped in tests.
var newCellCoreClient = func() (cellCoreClient, error) { return coreapi.New() }

// newNativeRepoCellCoreClient adds the project-scoped name lookup surface
// needed for /et/ repository identities. Kept separate from cellCoreClient so
// cached GitHub index clients do not need unrelated native lookup methods.
var newNativeRepoCellCoreClient = func() (nativeRepoCellCoreClient, error) { return coreapi.New() }

// resolveRepoCellTarget resolves the entire-api cell that HOSTS the given
// repo, plus that cell's jurisdiction, so a repo-scoped call (trails, experts)
// reaches the region that owns the repo — mirroring how the entire.io BFF
// selects a cell per repo (list-repos-index.ts' processingPlacement()) rather
// than using the caller's home cell.
//
// This is NOT best-effort: every failure (control-plane error or timeout,
// repo not onboarded, no/failed/suspended processing placement, missing
// apiUrl) returns a descriptive error instead of falling back to the
// caller's home cell. A silent wrong-region "success" is worse than a command
// failure for repo-scoped data — that fallback is exactly how a mirrored repo
// like entirehq/entire.io used to read (or fail to read) the wrong region's
// trails.
//
// Placement source:
//   - ulid form: coreapi.GetRepo(ulid) -> Repo.ClusterHost, mapped by host via
//     cellTargetForClusterHost. A ULID already names one placement
//     unambiguously, so there is no processing-placement lookup to do.
//   - owner/repo form: delegates to resolveRepoCellPlacement (see its doc
//     comment for why the two functions stay separate) and discards the
//     placement id.
func resolveRepoCellTarget(ctx context.Context, fullName, ulid string) (*auth.CellTarget, error) {
	if strings.TrimSpace(ulid) != "" {
		ctx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
		defer cancel()

		c, err := newCellCoreClient()
		if err != nil {
			return nil, fmt.Errorf("control plane unavailable: %w", err)
		}
		repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: ulid})
		if err != nil {
			return nil, cellPlacementError(ctx, ulid, fmt.Errorf("resolve the Entire cell for %s: %w", ulid, err))
		}
		clusterHost := strings.TrimSpace(repo.ClusterHost.Or(""))
		if clusterHost == "" {
			return nil, fmt.Errorf("resolve the Entire cell for %s: repo has no cluster host", ulid)
		}
		target, err := cellTargetForClusterHost(ctx, c, clusterHost)
		if err != nil {
			return nil, cellPlacementError(ctx, ulid, fmt.Errorf("resolve the Entire cell for %s: %w", ulid, err))
		}
		return target, nil
	}

	placement, err := resolveRepoCellPlacementByFullName(ctx, fullName)
	if err != nil {
		return nil, err
	}
	return placement.Target, nil
}

// resolveRepoCellPlacementByFullName parses an owner/repo name and resolves
// the processing placement. Keeping this boundary shared ensures callers that
// need both the cell and repo ID make exactly the same placement choice as
// callers that need only the cell.
func resolveRepoCellPlacementByFullName(ctx context.Context, fullName string) (repoCellPlacement, error) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(fullName), "/")
	if !ok || owner == "" || repo == "" {
		return repoCellPlacement{}, fmt.Errorf("invalid repo %q: expected owner/repo", fullName)
	}
	return resolveRepoCellPlacement(ctx, owner, repo)
}

// cellTargetForClusterHost maps a known cluster host to a cell apiUrl +
// jurisdiction via the coreapi cluster catalog, the authoritative source for a
// jurisdiction's cell URL. Used by the ULID path only: a GetRepo response
// names a cluster host, not a slug.
func cellTargetForClusterHost(ctx context.Context, c cellCoreClient, clusterHost string) (*auth.CellTarget, error) {
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	cluster, ok := matchClusterByHost(clusters.Clusters, clusterHost)
	if !ok {
		return nil, fmt.Errorf("no cluster matches host %q", clusterHost)
	}
	return cellTargetFromCluster(cluster)
}

// cellTargetForPlacement maps a RepoPlacement to its cell's apiUrl +
// jurisdiction via the coreapi cluster catalog, trying the same two joins the
// multi-repo fan-out uses: ClusterSlug against Cluster.Slug first (the
// reliable key for a placement — see cell_fanout.go's groupReposByCell), then
// the placement's Cell name against the catalog's URLs.
//
// Both tiers exist because resolveCellBaseURLs — the sibling path resolving
// the same data for a repo SET — needs both in practice; its own comments name
// the misses it survives ("cluster not in catalog", "cell name does not appear
// in any catalog URL"). Joining on slug alone here made the single-repo path
// hard-fail on exactly the catalog gap the fan-out was written to tolerate,
// leaving `entire trail`, `entire experts` and `explain --repo` strictly
// narrower than `entire search`.
//
// Unlike the fan-out there is deliberately no jurisdiction-default tier: that
// is home-cell routing, i.e. the wrong-region "success" this resolver exists
// to refuse. Two joins, then a loud failure.
func cellTargetForPlacement(ctx context.Context, c cellCoreClient, placement coreapi.RepoPlacement) (*auth.CellTarget, error) {
	clusters, err := c.ListClusters(ctx)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	cluster, ok := matchClusterBySlug(clusters.Clusters, placement.ClusterSlug)
	if !ok {
		cluster, ok = matchClusterByCellInURL(clusters.Clusters, placement.Cell)
	}
	if !ok {
		return nil, fmt.Errorf("no cluster matches slug %q or cell %q", placement.ClusterSlug, placement.Cell)
	}
	return cellTargetFromCluster(cluster)
}

// cellTargetFromCluster extracts the apiUrl + jurisdiction a matched catalog
// cluster resolves to, shared by the host- and slug-keyed joins above.
func cellTargetFromCluster(cluster coreapi.Cluster) (*auth.CellTarget, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(cluster.ApiUrl.Or("")), "/")
	// DNS is case-insensitive; normalise the catalog jurisdiction so a
	// non-lowercase value still passes the auth layer's strict label check
	// instead of hard-failing the target path.
	jurisdiction := strings.ToLower(strings.TrimSpace(cluster.Jurisdiction))
	if apiURL == "" || jurisdiction == "" {
		return nil, fmt.Errorf("matched cluster %q is missing apiUrl/jurisdiction", cluster.Slug)
	}
	return &auth.CellTarget{BaseURL: apiURL, Jurisdiction: jurisdiction}, nil
}

// errRepoNotOnboarded signals that fullName has no processing placement to
// resolve, in any of its three shapes: no entry at all in the control plane's
// repos index, a Candidate row (on the forge, never onboarded), or a row whose
// primaries name no processing placement. Callers that persist a per-repo
// enabled/disabled decision (trails' background enablement cache) should treat
// this the same way they'd treat an explicit "not found" from the data API,
// rather than leaving the decision unresolved forever.
//
// The third shape can be transient (a repo mid-onboarding has placements
// before its primaries are set) and is still grouped with the permanent two
// deliberately: repo-scoped data is unreachable either way, and the one
// consumer caches under a TTL, so a completed onboarding self-heals.
var errRepoNotOnboarded = errors.New("repo is not onboarded to Entire")

// resolveProcessingPlacement resolves fullName's PROCESSING placement — the
// placement id and cell entire-api and the control plane treat as
// authoritative for repo-scoped data (trails, experts, checkpoint/explain),
// mirroring the entire.io BFF's processingPlacement() selection
// (list-repos-index.ts). A repo can be mirrored in several regions; only this
// one placement is guaranteed to hold the repo's actual data.
//
// Its errors deliberately omit fullName: every caller already wraps them with
// the repo name, and naming it here too is what produced the doubled
// "resolve processing placement for X: X: repo is not onboarded" output.
func resolveProcessingPlacement(ctx context.Context, c cellCoreClient, fullName string) (coreapi.RepoPlacement, error) {
	entry, err := lookupRepoIndexEntry(ctx, c, fullName)
	if err != nil {
		return coreapi.RepoPlacement{}, err
	}
	// A Candidate row is a repo that exists on the forge and is visible to the
	// caller but was never onboarded: no placements or primaries to resolve.
	//
	// Kept as its own branch, but NOT because it is the common case — the
	// opposite is true today. The OpenAPI text on ListReposParams.Filter says
	// scope is ignored for a Filter lookup, which would make Candidate rows
	// reachable; the live control plane does not behave that way. Verified
	// against prod 2026-08-19: filter=torvalds/linux (public, plainly not
	// onboarded) returns ZERO rows, not a Candidate row, and
	// repo_mirror.go's runRepoMirrorGetByName documents the same server
	// behaviour. So the zero-rows branch above is the common real-world "not
	// onboarded" shape and this branch is currently unreachable.
	//
	// Both return errRepoNotOnboarded, so which one fires is not
	// behaviour-visible. Do not re-document this as the common case without
	// re-checking the server.
	if _, isCandidate := entry.Candidate.Get(); isCandidate {
		return coreapi.RepoPlacement{}, errRepoNotOnboarded
	}
	var processingID string
	if primaries, ok := entry.Primaries.Get(); ok {
		processingID = strings.TrimSpace(primaries.Processing)
	}
	if processingID == "" {
		return coreapi.RepoPlacement{}, errRepoNotOnboarded
	}
	// Shares the fan-out's matcher (cell_fanout.go) so the two resolvers can
	// never disagree on whether an ID names a placement — only on what they
	// do with the answer, which is the deliberate part (see below).
	p, ok := placementByID(entry.Placements, processingID)
	if !ok {
		// Defensive: primaries.processing should always name a placement
		// present in the same response.
		return coreapi.RepoPlacement{}, fmt.Errorf("processing placement %q is not in the repo's placement list", processingID)
	}
	if p.Status == coreapi.RepoPlacementStatusFailed || p.Status == coreapi.RepoPlacementStatusSuspended {
		return coreapi.RepoPlacement{}, fmt.Errorf("processing placement is %s", p.Status)
	}
	return p, nil
}

// lookupRepoIndexEntry is the exact-match repo-index lookup shared by every
// resolver that starts from an owner/repo name. Zero rows is
// errRepoNotOnboarded (see resolveProcessingPlacement for why that, not a
// Candidate row, is the real-world "not onboarded" shape).
//
// Filter is documented as an exact-match lookup restricted to fullName, so
// beyond the identity check and the zero-length check, Repos[0] is the only
// possible row. The check exists because that promise lives only in the
// OpenAPI doc, not in this code: if the control plane ever ignored or dropped
// Filter, the entry could silently be an unrelated repo — exactly the
// wrong-region-success bug class these resolvers exist to kill.
func lookupRepoIndexEntry(ctx context.Context, c cellCoreClient, fullName string) (coreapi.RepoIndexEntry, error) {
	out, err := c.ListRepos(ctx, coreapi.ListReposParams{Filter: coreapi.NewOptString(fullName)})
	if err != nil {
		return coreapi.RepoIndexEntry{}, fmt.Errorf("list repos: %w", err)
	}
	if len(out.Repos) == 0 {
		return coreapi.RepoIndexEntry{}, errRepoNotOnboarded
	}
	entry := out.Repos[0]
	if !strings.EqualFold(strings.TrimSpace(entry.FullName), strings.TrimSpace(fullName)) {
		return coreapi.RepoIndexEntry{}, fmt.Errorf("control plane returned %q for %s", entry.FullName, fullName)
	}
	return entry, nil
}

// repoCellPlacement is the identity entire-api keys repo-scoped routes on,
// paired with the cell that actually holds it. For a GitHub mirror RepoID is
// the processing placement ID; for an Entire-native repo it is core Repo.ID.
type repoCellPlacement struct {
	// RepoID is the value entire-api uses as repo_id. For GitHub mirrors this is
	// coreapi.RepoPlacement.ID (verified identical to the legacy
	// coreapi.Mirror.MirrorId); for native repos it is coreapi.Repo.ID.
	RepoID string
	// Target is the cell hosting this repo identity.
	Target *auth.CellTarget
}

// resolveForgeRepoCellPlacement keeps the forge namespace in repository
// identity resolution. A native /et/<project>/<repo> and a legacy
// /gh/<owner>/<repo> can have the same two trailing path segments but are
// different repositories with different repo IDs and, potentially, cells.
func resolveForgeRepoCellPlacement(ctx context.Context, forge, owner, repo string) (repoCellPlacement, error) {
	if forge == nativeCloneForge {
		return resolveNativeRepoCellPlacement(ctx, owner, repo)
	}
	return resolveRepoCellPlacement(ctx, owner, repo)
}

// resolveNativeRepoCellPlacement resolves /et/<project>/<repo> through the
// native project-scoped repo lookup, then maps the repo's home cluster to its
// entire-api cell. It deliberately never consults the forge-blind repos index:
// that index can select a same-named /gh/ mirror instead.
func resolveNativeRepoCellPlacement(ctx context.Context, project, repoName string) (repoCellPlacement, error) {
	ctx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
	defer cancel()

	c, err := newNativeRepoCellCoreClient()
	if err != nil {
		return repoCellPlacement{}, fmt.Errorf("control plane unavailable: %w", err)
	}
	repo, err := resolveNativeRepo(ctx, c, project, repoName)
	if err != nil {
		return repoCellPlacement{}, cellPlacementError(ctx, project+"/"+repoName, fmt.Errorf("resolve native repo %s/%s: %w", project, repoName, err))
	}
	repoID := strings.TrimSpace(repo.ID)
	if repoID == "" {
		return repoCellPlacement{}, fmt.Errorf("resolve native repo %s/%s: repo has no id", project, repoName)
	}
	clusterHost := strings.TrimSpace(repo.ClusterHost.Or(""))
	if clusterHost == "" {
		return repoCellPlacement{}, fmt.Errorf("resolve the Entire cell for %s/%s: repo has no cluster host", project, repoName)
	}
	target, err := cellTargetForClusterHost(ctx, c, clusterHost)
	if err != nil {
		return repoCellPlacement{}, cellPlacementError(ctx, repoID, fmt.Errorf("resolve the Entire cell for %s/%s: %w", project, repoName, err))
	}
	return repoCellPlacement{RepoID: repoID, Target: target}, nil
}

// resolveRepoCellPlacement resolves a GitHub repo to its processing
// placement's (repo_id, cell) pair for repo-scoped cell reads.
//
// The pair must come from the same placement: a placement id is only
// resolvable by the cell hosting that placement, and entire-api's data lives
// on exactly one placement (the processing one) even when the repo is
// mirrored elsewhere too. Taking the id from one placement and routing to a
// cell chosen some other way asks a cell about an id it has never seen, which
// comes back as an indistinguishable 404.
//
// resolveRepoCellTarget's owner/repo path delegates to this function and
// discards RepoID, rather than the two duplicating the same resolution body:
// the two functions exist separately only because their callers need
// different return shapes for the same lookup — this one also needs the
// placement id (repo_id), which resolveRepoCellTarget has no caller for and
// so doesn't return — not because the resolution logic itself differs.
func resolveRepoCellPlacement(ctx context.Context, owner, repo string) (repoCellPlacement, error) {
	ctx, cancel := context.WithTimeout(ctx, requiredCellResolveTimeout)
	defer cancel()

	c, err := newCellCoreClient()
	if err != nil {
		return repoCellPlacement{}, fmt.Errorf("control plane unavailable: %w", err)
	}
	fullName := owner + "/" + repo
	placement, err := resolveProcessingPlacement(ctx, c, fullName)
	if err != nil {
		return repoCellPlacement{}, cellPlacementError(ctx, fullName, fmt.Errorf("resolve processing placement for %s: %w", fullName, err))
	}
	target, err := cellTargetForPlacement(ctx, c, placement)
	if err != nil {
		return repoCellPlacement{}, cellPlacementError(ctx, fullName, fmt.Errorf("resolve cell for %s: %w", fullName, err))
	}
	// Trimmed for the same reason the match is: RepoID becomes a path segment
	// in entire-api URLs, so tolerating a padded id in the lookup without
	// normalizing it here would trade a loud "not in the placement list" for
	// a malformed request.
	return repoCellPlacement{RepoID: strings.TrimSpace(placement.ID), Target: target}, nil
}

// cellPlacementError reports a timeout as a timeout: without this, a fired
// deadline surfaces as whichever "not found"/"no processing placement" error
// the caller's lookup happened to return, and the user goes looking for a
// missing mirror instead of a slow control plane. subject is the repo the
// lookup was for, named however the caller addressed it (owner/repo or ULID).
//
// Only DeadlineExceeded is relabelled. A cancelled context is a Ctrl+C, not a
// timeout, and calling it one sends the user hunting a slow control plane that
// was never the problem; the cancelled in-flight call's own error already
// carries context.Canceled, which is what renderDataAPIAuthError silences on.
func cellPlacementError(ctx context.Context, subject string, fallback error) error {
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("resolving the Entire cell for %s timed out: %w", subject, ctxErr)
	}
	return fallback
}

// isActiveMirror reports whether a mirror placement can currently serve the
// repo: not archived, and not in a failed/suspended clone state. An unset status
// is treated as active (older data). Shared by every caller that must ignore
// placements a cell can't answer for.
func isActiveMirror(m coreapi.Mirror) bool {
	if m.IsArchived.Or(false) {
		return false
	}
	st := m.Status.Or(coreapi.MirrorStatusReady)
	return st != coreapi.MirrorStatusFailed && st != coreapi.MirrorStatusSuspended
}

// matchClusterByHost finds the catalog cluster whose public host equals
// clusterHost (case-insensitive). The cluster's apiUrl + jurisdiction are the
// authoritative cell coordinates. Used by the ULID path (GetRepo returns a
// host, not a slug).
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

// matchClusterBySlug finds the catalog cluster whose Slug equals clusterSlug
// (case-insensitive). Used by the owner/repo path: a RepoPlacement carries a
// ClusterSlug, not a host, so this is the reliable join for it (the catalog
// does not expose a cell field, so slug — not cell name — is the only
// reliable join key; see cell_fanout.go's groupReposByCell for the same rule).
func matchClusterBySlug(clusters []coreapi.Cluster, clusterSlug string) (coreapi.Cluster, bool) {
	want := strings.ToLower(strings.TrimSpace(clusterSlug))
	if want == "" {
		return coreapi.Cluster{}, false
	}
	for _, cl := range clusters {
		if strings.EqualFold(strings.TrimSpace(cl.Slug), want) {
			return cl, true
		}
	}
	return coreapi.Cluster{}, false
}
