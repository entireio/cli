package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/internal/coreapi"
)

// semanticSearchV4CellTimeout bounds each per-cell v4 query (token exchange +
// the query-serve call), mirroring codeSearchCellTimeout.
const semanticSearchV4CellTimeout = 30 * time.Second

// semanticSearchControlPlaneTimeout bounds each control-plane discovery call
// (repo index / repo lookup) on the v4 path.
const semanticSearchControlPlaneTimeout = 10 * time.Second

// semanticSearcher performs one semantic search. The command layer builds one
// per invocation (newSemanticSearcher) and every entry point — the one-shot
// command, the TUI's initial fetch, interactive re-searches, and pagination —
// calls through it, so all of them share one discovery cache.
type semanticSearcher func(ctx context.Context, cfg search.Config) (*search.Response, error)

// newSemanticSearcher returns the semantic-search entry for this invocation: a
// v4 query-serve session (ENT-1055) that fans out across entire-api cells and
// caches control-plane discovery across calls.
func newSemanticSearcher(insecureHTTP bool) semanticSearcher {
	s := &semanticSearchV4Session{insecureHTTP: insecureHTTP}
	return s.search
}

// loginHintErr maps auth.ErrNotLoggedIn to the standard login hint; other
// errors pass through unchanged.
func loginHintErr(err error) error {
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return errors.New("not authenticated. Run 'entire login' to authenticate")
	}
	return err
}

// semanticSearchV4Session holds per-invocation state for the v4 query-serve
// path. Control-plane discovery (the repo index, per-slug repo lookups, the
// cluster catalog) is stable for the life of one command, so it is resolved
// once and reused across TUI re-searches and pagination instead of paying
// several network round trips per keystroke-search. Identity tokens are NOT
// cached here — fanOutCells mints them per search (at most one per
// jurisdiction), which keeps expiry handling in the auth layer.
type semanticSearchV4Session struct {
	insecureHTTP bool

	mu         sync.Mutex
	coreClient *coreapi.Client
	clusters   *cachedClusterClient
	fullIndex  *coreapi.ListReposOutputBody        // unfiltered index, fetched at most once
	slugRepos  map[string][]coreapi.RepoIndexEntry // per-filter exact-match lookups
}

// search performs a v4 query-serve search across every cell that hosts the
// caller's in-scope repos, then merges the per-cell responses into one
// response. It is the semantic sibling of searchAllCells (code): resolve
// scope → group by hosting cell → one query-serve call per cell → tiered
// merge.
//
// Scope follows Config.ScopeSlugs — an explicit repo filter wins over
// --all-repos (the more specific filter scopes the search):
//   - unfiltered (repo:* / --all-repos with no explicit filter) → every cell
//     is queried with NO repo param, so query-serve returns everything the
//     caller's token authorizes there (matching the BFF and keeping the query
//     small for users with many repos).
//   - explicit repo filter(s) or the current-repo default → the slugs are
//     resolved via truncation-proof exact-match lookups and each cell is
//     scoped to the ULIDs it hosts.
//
// Completeness caveats (truncated index, failed regions) are returned as
// Response.Warnings for the caller to surface.
func (s *semanticSearchV4Session) search(ctx context.Context, cfg search.Config) (*search.Response, error) {
	if err := search.ValidateRepoFilters(cfg.Repos); err != nil {
		return nil, err //nolint:wrapcheck // user-facing validation message
	}

	coreClient, err := s.client()
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, loginHintErr(err)
		}
		return nil, fmt.Errorf("semantic search: resolving control-plane client: %w", err)
	}

	slugs, allRepos := cfg.ScopeSlugs()
	var cells []cellGroup
	var skipped []string
	var warnings []string
	indexTruncated := false
	scoped := !allRepos
	if scoped {
		if len(slugs) == 0 {
			return nil, errors.New("semantic search: could not determine the repository to search")
		}
		entries, err := s.resolveScope(ctx, slugs)
		if err != nil {
			return nil, err
		}
		// Scoped requests pin the picked placement's ULID (repoIDs below),
		// which query-serve resolves from its all-accessible byID set.
		cells, skipped = groupReposByCell(entries)
	} else {
		index, err := s.listFullIndex(ctx)
		if err != nil {
			return nil, fmt.Errorf("semantic search: listing repos for cell discovery: %w", err)
		}
		if index.Truncated {
			// Debug, not Warn: the user-facing channel is the Warnings entry;
			// slog's default handler would print a Warn straight to stderr on
			// commands whose context carries no logger.
			logging.Debug(ctx, "semantic search: repo index truncated; cross-repo results may be incomplete")
			warnings = append(warnings, "repo index truncated; cross-repo results may be incomplete")
			indexTruncated = true
		}
		// Broad requests send no repo pins; query-serve narrows pin-less
		// scope by the same election routedRepoPlacement picks by (ENT-1776),
		// so one grouping serves both request shapes.
		cells, skipped = groupReposByCell(index.Repos)
	}
	skipped = reportableSkippedRepos(ctx, scoped, len(cells), skipped)
	if len(skipped) > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d repo(s) with no searchable placement (missing or not ready): %s", len(skipped), strings.Join(skipped, ", ")))
	}
	// The BFF's scope-loss channels (search.ts): a truncated repo index means
	// broad coverage silently misses repos; an explicitly-filtered repo
	// skipped as not-ready narrows the promised scope. Unfiltered skips stay
	// silent — nothing advertised was narrowed.
	scopeNarrowed := scoped && len(skipped) > 0

	if len(cells) == 0 {
		resp := &search.Response{Results: []search.Result{}, Page: 1, Warnings: warnings, Counts: &search.TypeCounts{}}
		if indexTruncated {
			// Every facet count is a lower bound, not an exact zero — same
			// contract as the BFF's zero-routable-cells answer.
			resp.Truncated = true
			resp.CountsLowerBound = map[string]bool{
				facetRepos: true, facetCheckpoints: true, facetCommits: true, facetPRs: true, facetSessions: true,
			}
		}
		if indexTruncated || scopeNarrowed {
			resp.Partial = true
			resp.CoverageIncomplete = true
		}
		return resp, nil
	}
	resolveCellBaseURLs(ctx, s.clusterClient(coreClient), cells)

	results, err := fanOutCells(ctx, s.insecureHTTP, semanticSearchV4CellTimeout, cells, func(ctx context.Context, group cellGroup, client *api.Client) (*search.Response, error) {
		// Scoped: restrict the cell to the ULIDs it hosts. Unfiltered: send no
		// repo param and let query-serve search everything the token
		// authorizes in that cell (avoids per-request repo-filter caps for
		// large accounts).
		var repoIDs []string
		if scoped {
			repoIDs = group.repoIDs
		}
		return search.CellV4(ctx, client, cfg, repoIDs)
	})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, loginHintErr(err)
		}
		return nil, fmt.Errorf("semantic search: %w", err)
	}

	resp, err := mergeSemanticV4Responses(ctx, cfg.Limit, cfg.Page, results)
	if err != nil {
		return nil, err
	}
	resp.Warnings = append(warnings, resp.Warnings...)
	if indexTruncated {
		resp.Truncated = true
	}
	if indexTruncated || scopeNarrowed {
		resp.Partial = true
		resp.CoverageIncomplete = true
	}
	return resp, nil
}

// client returns the cached control-plane client, creating it on first use.
func (s *semanticSearchV4Session) client() (*coreapi.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coreClient != nil {
		return s.coreClient, nil
	}
	c, err := coreapi.New()
	if err != nil {
		return nil, err
	}
	s.coreClient = c
	return c, nil
}

// listFullIndex fetches (once) and caches the caller's full repo index, used
// for unfiltered searches and raw-ID filter matching.
func (s *semanticSearchV4Session) listFullIndex(ctx context.Context) (*coreapi.ListReposOutputBody, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fullIndex != nil {
		return s.fullIndex, nil
	}
	reposCtx, cancel := context.WithTimeout(ctx, semanticSearchControlPlaneTimeout)
	defer cancel()
	index, err := s.coreClient.ListRepos(reposCtx, coreapi.ListReposParams{})
	if err != nil {
		return nil, err
	}
	s.fullIndex = index
	return index, nil
}

// resolveScope resolves explicit repo filters (or the current-repo default)
// to index entries, deduped by repo ID. owner/name slugs use the exact-match
// ListRepos filter — immune to index truncation on large accounts — while raw
// IDs (no slash) fall back to matching against the full index. Zero matches
// is a clear error; a partially matched multi-repo filter proceeds with the
// matches, like resolveRepoFilters does for code search.
func (s *semanticSearchV4Session) resolveScope(ctx context.Context, slugs []string) ([]coreapi.RepoIndexEntry, error) {
	var entries []coreapi.RepoIndexEntry
	seen := make(map[string]bool, len(slugs))
	for _, f := range slugs {
		matched, err := s.lookupFilter(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, e := range matched {
			if !seen[e.ID] {
				seen[e.ID] = true
				entries = append(entries, e)
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no matching repositories found for %v (is the repo mirrored to Entire?)", slugs)
	}
	return entries, nil
}

// lookupFilter resolves one repo filter to index entries, cached per session.
// Matching mirrors resolveRepoFilters: a gh/ prefix is stripped, owner/name
// matches full_name (case-insensitive, server-side exact match), and a raw
// ULID matches the index entry's ID.
func (s *semanticSearchV4Session) lookupFilter(ctx context.Context, filter string) ([]coreapi.RepoIndexEntry, error) {
	s.mu.Lock()
	if cached, ok := s.slugRepos[filter]; ok {
		s.mu.Unlock()
		return cached, nil
	}
	s.mu.Unlock()

	slug := strings.TrimPrefix(filter, "gh/")
	var matched []coreapi.RepoIndexEntry
	if strings.Contains(slug, "/") {
		lookupCtx, cancel := context.WithTimeout(ctx, semanticSearchControlPlaneTimeout)
		defer cancel()
		out, err := s.coreClient.ListRepos(lookupCtx, coreapi.ListReposParams{Filter: coreapi.NewOptString(slug)})
		if err != nil {
			return nil, fmt.Errorf("semantic search: resolving repository %q: %w", filter, err)
		}
		matched = out.Repos
	} else {
		// Raw repo ID — the exact-match filter only matches full_name, so
		// match by ID against the full index.
		index, err := s.listFullIndex(ctx)
		if err != nil {
			return nil, fmt.Errorf("semantic search: resolving repository %q: %w", filter, err)
		}
		_, matched = resolveRepoFilters([]string{filter}, index.Repos)
	}

	s.mu.Lock()
	if s.slugRepos == nil {
		s.slugRepos = make(map[string][]coreapi.RepoIndexEntry)
	}
	s.slugRepos[filter] = matched
	s.mu.Unlock()
	return matched, nil
}

// clusterClient returns a cellCoreClient whose ListClusters is memoized for
// the session, so re-searches don't refetch the (stable) cluster catalog.
func (s *semanticSearchV4Session) clusterClient(inner *coreapi.Client) cellCoreClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clusters == nil {
		s.clusters = &cachedClusterClient{inner: inner}
	}
	return s.clusters
}

// cachedClusterClient is a cellCoreClient that caches a successful
// ListClusters result; failures are not cached, so the next call retries.
// The other methods delegate unchanged.
type cachedClusterClient struct {
	inner cellCoreClient

	mu       sync.Mutex
	clusters *coreapi.ListClustersOutputBody
}

func (c *cachedClusterClient) ListClusters(ctx context.Context) (*coreapi.ListClustersOutputBody, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clusters != nil {
		return c.clusters, nil
	}
	out, err := c.inner.ListClusters(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // transparent delegation
	}
	c.clusters = out
	return out, nil
}

func (c *cachedClusterClient) GetRepo(ctx context.Context, params coreapi.GetRepoParams) (*coreapi.Repo, error) {
	return c.inner.GetRepo(ctx, params) //nolint:wrapcheck // transparent delegation
}

func (c *cachedClusterClient) ListRepos(ctx context.Context, params coreapi.ListReposParams) (*coreapi.ListReposOutputBody, error) {
	return c.inner.ListRepos(ctx, params) //nolint:wrapcheck // transparent delegation
}

// tierOf returns a result's tier, or -1 when unset (dropped by the merge,
// like every tier outside 0/1 — see rankSemanticResults).
func tierOf(r search.Result) int {
	if r.Meta.Tier == nil {
		return -1
	}
	return *r.Meta.Tier
}

func bm25Of(r search.Result) float64 {
	if r.Meta.BM25Score != nil {
		return *r.Meta.BM25Score
	}
	return 0
}

// rerankOf returns query-serve's cross-encoder relevance score, or 0 when the
// row was not reranked (its cell fell back). Rows without a score sort after
// reranked ones, matching the BFF's rerankOf.
func rerankOf(r search.Result) float64 {
	if r.Meta.RerankScore != nil {
		return *r.Meta.RerankScore
	}
	return 0
}

// semanticCellPage is one cell's successful response.
type semanticCellPage struct {
	body *search.Response
}

// classifySemanticCells splits per-cell outcomes into successful pages, hard
// failures, and quietly-skipped cells. A cell is skipped — not failed — when
// it can't serve the search yet: its gateway has no query-serve route, or the
// cluster catalog doesn't expose the placement's jurisdiction at all (a cell
// mid-onboarding). Neither is worth warning the user about on every search.
func classifySemanticCells(ctx context.Context, results []cellCallResult[*search.Response]) (pages []semanticCellPage, failed []string, lastErr error) {
	var skipped, unmatched []string
	var unmatchedErr error
	for _, r := range results {
		switch {
		case errors.Is(r.err, search.ErrCellUnavailable), errors.Is(r.err, auth.ErrNoCellForJurisdiction):
			skipped = append(skipped, r.group.label())
		case errors.Is(r.err, search.ErrRepoFilterUnmatched):
			// The cell answered; the repo filter just matched nothing there.
			// Quiet like a skip when another cell has results, but if NO cell
			// matches it must surface as a repo problem, never a region one.
			unmatched = append(unmatched, r.group.label())
			unmatchedErr = r.err
		case r.err != nil:
			lastErr = r.err
			failed = append(failed, r.group.label())
		case r.value != nil:
			pages = append(pages, semanticCellPage{body: r.value})
		}
	}
	if len(skipped) > 0 {
		logging.Debug(ctx, "semantic search: cells without query-serve skipped", "skipped_cells", skipped)
	}
	if len(unmatched) > 0 {
		// unmatchedErr carries the server's own message (CellV4 wraps it into
		// the sentinel) — keep it visible here for diagnosis.
		logging.Debug(ctx, "semantic search: cells where the repo filter matched nothing", "unmatched_cells", unmatched, "error", unmatchedErr.Error())
	}
	if len(pages) == 0 && lastErr == nil {
		switch {
		case len(unmatched) > 0:
			// Takes priority over the region message: a cell answering proves
			// its region serves semantic search.
			lastErr = errNoRepoAvailable
		case len(skipped) > 0:
			lastErr = errNoRegionAvailable
		}
	}
	return pages, failed, lastErr
}

// errNoRepoAvailable is returned when at least one cell answered but none
// matched the repo filter. A typo'd name or missing access cannot reach this
// point — resolveScope already validated the slug against the control-plane
// repo index — so the message names only the causes that survive: query-serve
// hasn't indexed the repo, or its owner org isn't enabled for semantic search.
var errNoRepoAvailable = errors.New("semantic search cannot search this repo yet — it may not be indexed, or semantic search may not be enabled for its owner")

// errNoRegionAvailable is returned when every queried cell lacks query-serve.
var errNoRegionAvailable = errors.New("semantic search is not yet available in the region(s) hosting this search")

// tier0Row pairs a tier-0 result with its rank within the cell that returned
// it — the selection order the mixed-capability fallback preserves (see
// sortTier0Rows).
type tier0Row struct {
	r        search.Result
	cellRank int
}

// rankSemanticResults buckets every page's rows and applies the SAME ordering
// the BFF's cross-cell merge uses (see the ordering contract on
// mergeSemanticV4Responses): repos first by retrieval score desc, then tier 0
// (sortTier0Rows), then tier 1 by rerank desc with retrieval Score tiebreak.
// Every other non-repo row — tier 2 (the retired ANN-only fallback, still
// emitted by un-redeployed cells) and rows with no tier at all — is DROPPED,
// exactly as the BFF drops them. Ordering by rerank score — not BM25 or the
// per-namespace retrieval Score — is what keeps the CLI in parity with the
// web: query-serve documents that Score "is not the field results are ordered
// by … sorting by it undoes reranking."
func rankSemanticResults(pages []semanticCellPage) (merged []search.Result) {
	var repos, tier1 []search.Result
	var tier0 []tier0Row
	for _, p := range pages {
		cellRank := 0
		for _, r := range p.body.Results {
			switch {
			case r.Type == search.TypeRepo:
				repos = append(repos, r)
			case tierOf(r) == 0:
				tier0 = append(tier0, tier0Row{r: r, cellRank: cellRank})
				cellRank++
			case tierOf(r) == 1:
				tier1 = append(tier1, r)
			}
		}
	}
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].Meta.Score > repos[j].Meta.Score })
	merged = append(merged, repos...)
	sortTier0Rows(tier0)
	// Tier 1: rerank score decides, retrieval Score breaks ties. Rows whose
	// cell fell back (no rerank score) sort last, matching the BFF.
	sort.SliceStable(tier1, func(i, j int) bool {
		if ri, rj := rerankOf(tier1[i]), rerankOf(tier1[j]); ri != rj {
			return ri > rj
		}
		return tier1[i].Meta.Score > tier1[j].Meta.Score
	})
	for _, t := range tier0 {
		merged = append(merged, t.r)
	}
	merged = append(merged, tier1...)
	return merged
}

// sortTier0Rows orders the tier-0 band the way the BFF's sortTier0 does
// (ENT-1431). When EVERY row was judged by the cross-encoder, rerank score
// decides and BM25 breaks ties. When any row is missing a rerank score — its
// cell predates tier-0 reranking — a global rerank sort would demote all of
// that cell's full-phrase hits regardless of relevance, so instead the rows are
// interleaved positionally (each cell's #1, then each cell's #2, …), preserving
// every cell's own selection order, with BM25 breaking ties between same-rank
// rows. Converges to rerank-first once every cell is current.
func sortTier0Rows(tier0 []tier0Row) {
	allScored := true
	for _, t := range tier0 {
		if t.r.Meta.RerankScore == nil {
			allScored = false
			break
		}
	}
	if allScored {
		sort.SliceStable(tier0, func(i, j int) bool {
			if ri, rj := rerankOf(tier0[i].r), rerankOf(tier0[j].r); ri != rj {
				return ri > rj
			}
			return bm25Of(tier0[i].r) > bm25Of(tier0[j].r)
		})
		return
	}
	sort.SliceStable(tier0, func(i, j int) bool {
		if tier0[i].cellRank != tier0[j].cellRank {
			return tier0[i].cellRank < tier0[j].cellRank
		}
		return bm25Of(tier0[i].r) > bm25Of(tier0[j].r)
	})
}

// dedupSemanticResults removes cross-cell duplicate SESSION rows, keeping the
// first (higher-ranked) copy — the BFF's exact rule: sessions are the one type
// that legitimately repeats across cells (a crosslinked session attached to
// repos homed in different cells), while repo-bound types are canonical to one
// cell since the fan-out routes each repo to its single home placement
// (ENT-1672/ENT-1776). The key is the global session id when present, else the
// repo-qualified carrier checkpoint (server-folded legacy rows, ENT-1595);
// rows with neither are always kept.
func dedupSemanticResults(merged []search.Result) []search.Result {
	seen := make(map[string]bool)
	deduped := merged[:0]
	for _, r := range merged {
		if r.Type != search.TypeSession {
			deduped = append(deduped, r)
			continue
		}
		id := r.DedupID()
		if id == "" {
			deduped = append(deduped, r)
			continue
		}
		key := r.Type + "\x00" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	return deduped
}

// Facet names for the wire maps (truncated_types / counts_lower_bound) — the
// BFF's RESULT_TYPE_TO_FACET values.
const (
	facetRepos       = "repos"
	facetCheckpoints = "checkpoints"
	facetCommits     = "commits"
	facetPRs         = "prs"
	facetSessions    = "sessions"
)

// semanticFacetOf maps a wire result type to its counts/truncation facet name
// (the BFF's RESULT_TYPE_TO_FACET). Unknown types have no facet: they are kept
// in results, exempt from the per-type cap, and counted only in the total.
func semanticFacetOf(typ string) (string, bool) {
	switch typ {
	case search.TypeRepo:
		return facetRepos, true
	case search.TypeCheckpoint:
		return facetCheckpoints, true
	case search.TypeCommit:
		return facetCommits, true
	case search.TypePR:
		return facetPRs, true
	case search.TypeSession:
		return facetSessions, true
	}
	return "", false
}

// capSemanticPerType applies the BFF's per-type window: walk the merged list
// in order and keep at most `limit` rows of each facet, recording which facets
// overflowed. Rows without a facet mapping are kept uncounted.
func capSemanticPerType(merged []search.Result, limit int) ([]search.Result, map[string]bool) {
	truncated := make(map[string]bool)
	if limit <= 0 {
		return merged, truncated
	}
	perFacet := make(map[string]int, 5)
	out := merged[:0]
	for _, r := range merged {
		facet, ok := semanticFacetOf(r.Type)
		if !ok {
			out = append(out, r)
			continue
		}
		perFacet[facet]++
		if perFacet[facet] > limit {
			truncated[facet] = true
			continue
		}
		out = append(out, r)
	}
	return out, truncated
}

// deriveSemanticCounts computes total and per-type counts from the FINAL
// merged list — the BFF's per-type contract (ENT-1363): counts describe
// exactly what the caller can see, never the cells' corpus-count passes,
// with counts_lower_bound marking the facets whose lists were capped.
func deriveSemanticCounts(merged []search.Result) (int, *search.TypeCounts) {
	counts := &search.TypeCounts{}
	for _, r := range merged {
		switch r.Type {
		case search.TypeRepo:
			counts.Repos++
		case search.TypeCheckpoint:
			counts.Checkpoints++
		case search.TypeCommit:
			counts.Commits++
		case search.TypePR:
			counts.PRs++
		case search.TypeSession:
			counts.Sessions++
		}
	}
	return len(merged), counts
}

// mergeSemanticV4Responses interleaves per-cell query-serve responses into
// one final page. THE CROSS-CELL MERGE ORDERING CONTRACT lives here and in
// the BFF's mergeCellResponses (entire.io api/src/routes/search.ts) — the two
// implementations must stay byte-identical (ENT-1777); change one, change
// both, and update this block:
//
//  1. Band order: repo rows first (retrieval score desc), then tier 0, then
//     tier 1. Tier 2 and tier-less non-repo rows are dropped — the ANN-only
//     fallback tier is retired and only un-redeployed cells still emit it.
//     Band order deliberately overrides each cell's own post-fold final
//     order (entire-search#181 folds after ranking): the clients match each
//     other, not the leaf.
//  2. Tier 0: when every row carries a rerank score, rerank desc with BM25
//     tiebreak; when any cell predates tier-0 reranking, positional
//     interleave by in-cell rank with BM25 tiebreak (sortTier0Rows). Tier 1:
//     rerank desc, retrieval Score tiebreak. All sorts are STABLE — equal
//     keys keep cell-response order.
//  3. Dedupe AFTER ordering, BEFORE the per-type cap, sessions only, first
//     (highest-ranked) copy wins (dedupSemanticResults).
//  4. Per-type window: at most `limit` rows of each facet, overflow recorded
//     in truncated_types, unioned with the cells' own truncated_types
//     (capSemanticPerType). No global cut.
//  5. Counts and total derive from the FINAL list (deriveSemanticCounts);
//     counts_lower_bound == the capped facets.
//  6. Flags: truncated = any cell truncated; partial = truncated ∨ any cell
//     failed ∨ any cell partial ∨ mixed rerank capability; coverage_incomplete
//     = the same minus rerank-mix; reranked = every cell reranked (emitted
//     when any cell reported the field).
//
// Rerank scores share a space across cells (same Cohere model), so
// interleaving is meaningful. All-cells-failed is an error; a partial failure
// is noted in Warnings (and the flags) and the surviving cells are merged.
func mergeSemanticV4Responses(ctx context.Context, limit, page int, results []cellCallResult[*search.Response]) (*search.Response, error) {
	pages, failed, lastErr := classifySemanticCells(ctx, results)
	if len(pages) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("semantic search: %w", lastErr)
		}
		return &search.Response{Results: []search.Result{}, Page: 1}, nil
	}
	var warnings []string
	if len(failed) > 0 {
		// Debug, not Warn: the warning below already reaches the user via
		// Response.Warnings, and slog's default handler would print a Warn
		// straight to stderr on commands whose context carries no logger.
		logging.Debug(ctx, "semantic search: partial failure; results may be incomplete",
			"succeeded", len(pages), "total", len(results), "failed_cells", failed)
		warnings = append(warnings, fmt.Sprintf("search failed in %d of %d regions; results may be incomplete", len(failed), len(pages)+len(failed)))
	}

	merged := rankSemanticResults(pages)
	merged = dedupSemanticResults(merged)
	merged, truncatedTypes := capSemanticPerType(merged, limit)
	for _, p := range pages {
		for facet, v := range p.body.TruncatedTypes {
			if v {
				truncatedTypes[facet] = true
			}
		}
	}
	if merged == nil {
		merged = []search.Result{}
	}

	total, counts := deriveSemanticCounts(merged)
	resp := &search.Response{
		Results:  merged,
		Total:    total,
		Page:     max(page, 1),
		Counts:   counts,
		Warnings: warnings,
	}
	if len(truncatedTypes) > 0 {
		resp.TruncatedTypes = truncatedTypes
		resp.CountsLowerBound = make(map[string]bool, len(truncatedTypes))
		for facet := range truncatedTypes {
			resp.CountsLowerBound[facet] = true
		}
	}

	anyRerankedField, anyReranked, allReranked := false, false, true
	for _, p := range pages {
		if p.body.Truncated {
			resp.Truncated = true
		}
		if p.body.Partial {
			resp.Partial = true
			resp.CoverageIncomplete = true
		}
		if p.body.Reranked != nil {
			anyRerankedField = true
			if *p.body.Reranked {
				anyReranked = true
			} else {
				allReranked = false
			}
		} else {
			allReranked = false
		}
	}
	if len(failed) > 0 || resp.Truncated {
		resp.Partial = true
		resp.CoverageIncomplete = true
	}
	if anyReranked && !allReranked {
		// Mixed rerank capability skews ordering but not coverage — partial
		// without coverage_incomplete, matching the BFF.
		resp.Partial = true
	}
	if anyRerankedField {
		resp.Reranked = &allReranked
	}
	return resp, nil
}
