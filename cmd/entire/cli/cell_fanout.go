package cli

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// This file is the multi-cell counterpart to cell_target.go: where
// resolveRepoCellTarget routes ONE repo-scoped call to the cell hosting that
// repo, the helpers here route a query over ALL of the caller's repos — group
// the repo index by hosting cell, then ask each cell about its own repos and
// let the caller merge. That mirrors the entire.io BFF's fan-out
// (code-search.ts: index → group by cell → per-cell call → merge); no
// server-side aggregator exists, cells are strictly local.

// cellGroup is one entire-api cell plus the caller's repos hosted there — the
// unit of a multi-cell fan-out.
type cellGroup struct {
	// cell is the physical cell name (e.g. aws-eu-west-1), the grouping key —
	// each repo placement lives in exactly one cell. Empty when the index did
	// not report one (the group then routes by jurisdiction, or home).
	cell string
	// clusterSlug joins the group to the cluster catalog
	// (RepoIndexEntry.ClusterSlug ↔ Cluster.Slug) to resolve baseURL. The
	// catalog does not expose a cell field, so the slug — not the cell name —
	// is the only reliable join key. Several clusters may share a cell; any of
	// them reports the jurisdiction's apiUrl, so the first seen slug serves.
	clusterSlug string
	// jurisdiction is the lowercased jurisdiction label; it drives the identity
	// token audience and is the routing fallback when baseURL is empty.
	jurisdiction string
	// baseURL is the cell's resolved apiUrl (resolveCellBaseURLs). Empty means
	// "route by jurisdiction" — the auth layer then resolves the jurisdiction's
	// default cell from the catalog.
	baseURL string
	// repoIDs are the caller's repo ULIDs placed in this cell, so the cell is
	// only ever asked about repos it hosts.
	repoIDs []string
}

// groupReposByCell groups a repo index by hosting cell, one group per distinct
// cell, deterministically ordered by cell name (jurisdiction as tiebreak).
// Entries without an ID are skipped (nothing to ask the cell about). The key
// includes the jurisdiction so entries whose index row carries no cell don't
// collapse across jurisdictions into one group routed by whichever repo came
// first — they stay per-jurisdiction and route via the jurisdiction fallback.
func groupReposByCell(repos []coreapi.RepoIndexEntry) []cellGroup {
	byCell := make(map[string]*cellGroup)
	for _, r := range repos {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			continue
		}
		cell := strings.ToLower(strings.TrimSpace(r.Cell))
		jurisdiction := strings.ToLower(strings.TrimSpace(r.Jurisdiction))
		key := cell + "\x00" + jurisdiction
		g, ok := byCell[key]
		if !ok {
			g = &cellGroup{
				cell:         cell,
				clusterSlug:  strings.ToLower(strings.TrimSpace(r.ClusterSlug)),
				jurisdiction: jurisdiction,
			}
			byCell[key] = g
		}
		g.repoIDs = append(g.repoIDs, id)
	}
	cells := make([]cellGroup, 0, len(byCell))
	for _, g := range byCell {
		cells = append(cells, *g)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].cell != cells[j].cell {
			return cells[i].cell < cells[j].cell
		}
		return cells[i].jurisdiction < cells[j].jurisdiction
	})
	return cells
}

// resolveCellBaseURLs fills each group's baseURL from the cluster catalog,
// joining on ClusterSlug ↔ Cluster.Slug. Best-effort: on a catalog error or
// timeout (bounded by cellResolveTimeout, like resolveRepoCellTarget — a hung
// core must not stall the command) or a missing/incomplete cluster row, the
// group keeps baseURL "" and falls back to jurisdiction routing — a degraded
// catalog must not sink the fan-out.
func resolveCellBaseURLs(ctx context.Context, c cellCoreClient, cells []cellGroup) {
	ctx, cancel := context.WithTimeout(ctx, cellResolveTimeout)
	defer cancel()

	clusters, err := c.ListClusters(ctx)
	if err != nil {
		logging.Debug(ctx, "cell fan-out: list clusters failed, using jurisdiction routing", "error", err.Error())
		return
	}
	bySlug := make(map[string]coreapi.Cluster, len(clusters.Clusters))
	for _, cl := range clusters.Clusters {
		bySlug[strings.ToLower(strings.TrimSpace(cl.Slug))] = cl
	}
	for i := range cells {
		cl, ok := bySlug[cells[i].clusterSlug]
		if !ok {
			logging.Debug(ctx, "cell fan-out: cluster not in catalog, using jurisdiction routing",
				"cluster_slug", cells[i].clusterSlug, "cell", cells[i].cell)
			continue
		}
		// A concrete baseURL needs a jurisdiction to mint the matching token
		// for — mirroring resolveRepoCellTarget, which refuses a target unless
		// both are present. Setting baseURL with an unknown jurisdiction would
		// dial the cell with a home-jurisdiction token.
		jurisdiction := cells[i].jurisdiction
		if j := strings.ToLower(strings.TrimSpace(cl.Jurisdiction)); j != "" {
			jurisdiction = j
		}
		if jurisdiction == "" {
			logging.Debug(ctx, "cell fan-out: no jurisdiction for cluster, using home routing",
				"cluster_slug", cells[i].clusterSlug, "cell", cells[i].cell)
			continue
		}
		cells[i].jurisdiction = jurisdiction
		cells[i].baseURL = strings.TrimRight(strings.TrimSpace(cl.ApiUrl.Or("")), "/")
	}
}

// cellTarget converts the group's routing coordinates into the auth layer's
// CellTarget: full target when the catalog resolved a baseURL,
// jurisdiction-only when it didn't, nil (home routing) when neither is known.
func (g cellGroup) cellTarget() *auth.CellTarget {
	switch {
	case g.baseURL != "":
		return &auth.CellTarget{BaseURL: g.baseURL, Jurisdiction: g.jurisdiction}
	case g.jurisdiction != "":
		return &auth.CellTarget{Jurisdiction: g.jurisdiction}
	default:
		return nil
	}
}

// label names the group in errors and logs: cell, else jurisdiction, else home.
func (g cellGroup) label() string {
	switch {
	case g.cell != "":
		return g.cell
	case g.jurisdiction != "":
		return g.jurisdiction
	default:
		return "home"
	}
}

// cellClientBuilder is what fanOutCells needs from the auth layer;
// *auth.CellClientFactory satisfies it. A seam so fan-out tests don't run the
// real discovery/exchange stack.
type cellClientBuilder interface {
	ClientFor(ctx context.Context, target *auth.CellTarget) (*api.Client, error)
}

// newCellClientBuilder builds the per-operation cell client factory: the
// subject is resolved once and identity tokens are minted once per
// jurisdiction, however many cells the fan-out touches. Swapped in tests.
var newCellClientBuilder = func(ctx context.Context, insecureHTTP bool) (cellClientBuilder, error) {
	return auth.NewEntireAPICellClientFactory(ctx, insecureHTTP)
}

// cellCallResult is one cell's outcome in a fan-out: the group it was asked
// for, and either fn's value or the error (client construction or fn itself).
type cellCallResult[T any] struct {
	group cellGroup
	value T
	err   error
}

// fanOutCells calls fn once per cell group — concurrently when there is more
// than one — under a per-cell timeout, and returns every cell's outcome in
// input order. One bad cell never sinks the operation: its error is recorded
// in its slot and the other cells proceed; the caller decides how partial
// results surface (typically: merge successes, warn about failures, error only
// when every cell failed). The returned error is non-nil only when no per-cell
// call could even start (factory construction failed — e.g. not logged in).
func fanOutCells[T any](ctx context.Context, insecureHTTP bool, perCellTimeout time.Duration, cells []cellGroup, fn func(ctx context.Context, group cellGroup, client *api.Client) (T, error)) ([]cellCallResult[T], error) {
	if len(cells) == 0 {
		return nil, nil
	}
	factory, err := newCellClientBuilder(ctx, insecureHTTP)
	if err != nil {
		return nil, err
	}

	call := func(ctx context.Context, g cellGroup) cellCallResult[T] {
		ctx, cancel := context.WithTimeout(ctx, perCellTimeout)
		defer cancel()
		res := cellCallResult[T]{group: g}
		client, err := factory.ClientFor(ctx, g.cellTarget())
		if err != nil {
			res.err = err
			return res
		}
		res.value, res.err = fn(ctx, g, client)
		return res
	}

	results := make([]cellCallResult[T], len(cells))
	if len(cells) == 1 {
		results[0] = call(ctx, cells[0])
		return results, nil
	}
	var wg sync.WaitGroup
	for i := range cells {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = call(ctx, cells[i])
		}(i)
	}
	wg.Wait()
	return results, nil
}
