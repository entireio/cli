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
	var warnings []string
	scoped := !allRepos
	if scoped {
		if len(slugs) == 0 {
			return nil, errors.New("semantic search: could not determine the repository to search")
		}
		entries, err := s.resolveScope(ctx, slugs)
		if err != nil {
			return nil, err
		}
		cells = groupReposByCell(entries)
	} else {
		index, err := s.listFullIndex(ctx)
		if err != nil {
			return nil, fmt.Errorf("semantic search: listing repos for cell discovery: %w", err)
		}
		if index.Truncated {
			// Debug, not Warn: the user-facing channel is the Warnings entry;
			// slog's default handler would print a Warn straight to stderr on
			// commands that never ran logging.Init.
			logging.Debug(ctx, "semantic search: repo index truncated; cross-repo results may be incomplete")
			warnings = append(warnings, "repo index truncated; cross-repo results may be incomplete")
		}
		cells = groupReposByCell(index.Repos)
	}

	if len(cells) == 0 {
		return &search.Response{Results: []search.Result{}, Page: 1, Warnings: warnings}, nil
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

func (c *cachedClusterClient) ListMirrors(ctx context.Context, params coreapi.ListMirrorsParams) (*coreapi.ListMirrorsOutputBody, error) {
	return c.inner.ListMirrors(ctx, params) //nolint:wrapcheck // transparent delegation
}

// mergedTier2Max mirrors query-serve's maxTier2 and the BFF's MERGED_TIER2_MAX:
// the ANN-only fallback tail is capped when it's all there is.
const mergedTier2Max = 15

// tierOf returns a result's tier, or -1 when unset (treated as the ANN-only
// fallback tier).
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

// annOrScoreOf prefers the raw ANN score, falling back to the overall score —
// the ordering key query-serve/BFF use for the tier-2 ANN fallback (for
// tier-2 rows Score is itself ANN-derived, so lower is better in both cases).
func annOrScoreOf(r search.Result) float64 {
	if r.Meta.ANNScore != nil {
		return *r.Meta.ANNScore
	}
	return r.Meta.Score
}

// semanticCellPage is one cell's successful response plus the classification
// bit the merge keys on.
type semanticCellPage struct {
	body     *search.Response
	hasUpper bool // any non-repo tier-0/1 row
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
			p := semanticCellPage{body: r.value}
			for _, res := range r.value.Results {
				// Repo rows never count as an upper tier — they're always
				// merged regardless of the cell's checkpoint/commit tiers.
				if res.Type != search.TypeRepo && (tierOf(res) == 0 || tierOf(res) == 1) {
					p.hasUpper = true
					break
				}
			}
			pages = append(pages, p)
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

// rankSemanticResults buckets every page's rows and applies the ordering
// query-serve uses within a cell (and the BFF uses across cells): repos first,
// then tier-0 by BM25 desc, tier-1 by rerank score desc, promoted tier-2 by
// ANN asc. Tier-2 rows arriving alongside tier 0/1 from the same cell were
// deliberately promoted by query-serve; a cell whose page is entirely tier 2
// had nothing better — its ANN fallback, shown (uncapped here) only when tiers
// 0/1 are empty everywhere.
func rankSemanticResults(pages []semanticCellPage) (merged []search.Result, globalUpper bool) {
	for _, p := range pages {
		if p.hasUpper {
			globalUpper = true
			break
		}
	}

	var repos, tier0, tier1, promotedTier2, fallbackTier2 []search.Result
	for _, p := range pages {
		for _, r := range p.body.Results {
			switch {
			case r.Type == search.TypeRepo:
				repos = append(repos, r)
			case tierOf(r) == 0:
				tier0 = append(tier0, r)
			case tierOf(r) == 1:
				tier1 = append(tier1, r)
			case p.hasUpper:
				promotedTier2 = append(promotedTier2, r)
			default:
				fallbackTier2 = append(fallbackTier2, r)
			}
		}
	}
	sort.SliceStable(repos, func(i, j int) bool { return repos[i].Meta.Score > repos[j].Meta.Score })
	merged = append(merged, repos...)
	if globalUpper {
		sort.SliceStable(tier0, func(i, j int) bool { return bm25Of(tier0[i]) > bm25Of(tier0[j]) })
		sort.SliceStable(tier1, func(i, j int) bool { return tier1[i].Meta.Score > tier1[j].Meta.Score })
		sort.SliceStable(promotedTier2, func(i, j int) bool { return annOrScoreOf(promotedTier2[i]) < annOrScoreOf(promotedTier2[j]) })
		merged = append(merged, tier0...)
		merged = append(merged, tier1...)
		merged = append(merged, promotedTier2...)
	} else {
		sort.SliceStable(fallbackTier2, func(i, j int) bool { return annOrScoreOf(fallbackTier2[i]) < annOrScoreOf(fallbackTier2[j]) })
		merged = append(merged, fallbackTier2...)
	}
	return merged, globalUpper
}

// dedupSemanticResults removes cross-cell duplicates by type+id (a repo
// mirrored across cells returns the same logical result from each), keeping
// the first (higher-ranked) copy. Results without an id are always kept. The
// per-type dupe tally feeds the total/count corrections.
func dedupSemanticResults(merged []search.Result) ([]search.Result, map[string]int) {
	seen := make(map[string]bool, len(merged))
	dupesByType := make(map[string]int)
	deduped := merged[:0]
	for _, r := range merged {
		id := r.ResultID()
		if id == "" {
			deduped = append(deduped, r)
			continue
		}
		key := r.Type + "\x00" + id
		if seen[key] {
			dupesByType[r.Type]++
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	return deduped, dupesByType
}

// aggregateSemanticTotals sums per-cell totals and counts, minus dedup, and
// excluding cells whose entire page was a discarded ANN fallback — their
// matches are unreachable, so they must not be advertised.
func aggregateSemanticTotals(pages []semanticCellPage, globalUpper bool, dupesByType map[string]int) (int, *search.TypeCounts) {
	dupes := 0
	for _, n := range dupesByType {
		dupes += n
	}
	total := -dupes
	counts := &search.TypeCounts{}
	for _, p := range pages {
		if globalUpper && !p.hasUpper {
			// This page's non-repo rows were its ANN fallback and were
			// dropped by the merge — those matches are unreachable, so only
			// its repo rows (which are always merged) may be counted.
			repoRows := 0
			if p.body.Counts != nil {
				repoRows = p.body.Counts.Repos
			} else {
				for _, r := range p.body.Results {
					if r.Type == search.TypeRepo {
						repoRows++
					}
				}
			}
			total += repoRows
			counts.Repos += repoRows
			continue
		}
		total += p.body.Total
		if p.body.Counts != nil {
			counts.Repos += p.body.Counts.Repos
			counts.Checkpoints += p.body.Counts.Checkpoints
			counts.Commits += p.body.Counts.Commits
			counts.PRs += p.body.Counts.PRs
			counts.Sessions += p.body.Counts.Sessions
		}
	}
	subtractDupeCounts(counts, dupesByType)
	return max(total, 0), counts
}

// mergeSemanticV4Responses interleaves per-cell query-serve responses into one,
// applying the SAME ordering query-serve uses within a cell (see
// rankSemanticResults); when no cell produced tier 0/1 the ANN-only fallback
// tail is shown (deduped, then capped). Results are deduped by type+id and
// capped to limit. Rerank scores share a space across cells (same Cohere
// model), so interleaving is meaningful.
//
// Totals/counts include only cells whose results were actually mergeable (see
// aggregateSemanticTotals). All-cells-failed is an error; a partial failure is
// noted in Warnings and the surviving cells are merged.
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
		// straight to stderr on commands that never ran logging.Init.
		logging.Debug(ctx, "semantic search: partial failure; results may be incomplete",
			"succeeded", len(pages), "total", len(results), "failed_cells", failed)
		warnings = append(warnings, fmt.Sprintf("search failed in %d of %d regions; results may be incomplete", len(failed), len(pages)+len(failed)))
	}

	merged, globalUpper := rankSemanticResults(pages)
	merged, dupesByType := dedupSemanticResults(merged)

	// Cap the ANN-only fallback tail AFTER dedup (repos always precede it),
	// so cross-cell duplicates don't shrink the visible page below the cap.
	if !globalUpper {
		nRepos := 0
		for _, r := range merged {
			if r.Type != search.TypeRepo {
				break
			}
			nRepos++
		}
		if len(merged) > nRepos+mergedTier2Max {
			merged = merged[:nRepos+mergedTier2Max]
		}
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	if merged == nil {
		merged = []search.Result{}
	}

	total, counts := aggregateSemanticTotals(pages, globalUpper, dupesByType)
	return &search.Response{
		Results:  merged,
		Total:    total,
		Page:     max(page, 1),
		Counts:   counts,
		Warnings: warnings,
	}, nil
}

// subtractDupeCounts removes deduplicated rows from the aggregate per-type
// counts so `counts` reflects distinct results, matching the corrected total.
func subtractDupeCounts(counts *search.TypeCounts, dupesByType map[string]int) {
	for typ, n := range dupesByType {
		switch typ {
		case search.TypeCheckpoint:
			counts.Checkpoints = max(0, counts.Checkpoints-n)
		case search.TypeCommit:
			counts.Commits = max(0, counts.Commits-n)
		case search.TypeSession:
			counts.Sessions = max(0, counts.Sessions-n)
		case search.TypeRepo:
			counts.Repos = max(0, counts.Repos-n)
		case search.TypePR:
			counts.PRs = max(0, counts.PRs-n)
		}
	}
}
