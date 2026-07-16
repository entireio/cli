package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/codesearch"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

func newSearchCmd() *cobra.Command { //nolint:maintidx // command wiring is inherently complex
	var (
		jsonOutput       bool
		codeFlag         bool
		caseSensitive    bool
		limitFlag        int
		pageFlag         int
		authorFlag       string
		dateFlag         string
		branchFlag       string
		repoFlag         string
		allReposFlag     bool
		insecureHTTPAuth bool
	)

	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search checkpoints, commits, and sessions using semantic and keyword matching",
		Long: `Search checkpoints, commits, and sessions using hybrid search (semantic + keyword),
powered by the Entire search service.

Requires authentication via 'entire login' (GitHub device flow).

By default, results are scoped to the current repository. Use --all-repos to
search across all accessible repos.

Run without arguments to open an interactive search. Results are
displayed in an interactive table. Use --json for machine-readable output.

CLI queries also support inline filters like author:<name>, date:<week|month>,
branch:<name>, repo:<owner/name>, and repo:* to search all accessible repos.`,
		Args:   cobra.ArbitraryArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.Join(args, " ")

			if caseSensitive && !codeFlag {
				return errors.New("--case-sensitive can only be used with --code")
			}

			if codeFlag {
				// Reject flags that only apply to checkpoint search.
				for _, pair := range []struct{ flag, name string }{
					{authorFlag, "--author"},
					{dateFlag, "--date"},
					{branchFlag, "--branch"},
				} {
					if pair.flag != "" {
						return fmt.Errorf("%s cannot be used with --code", pair.name)
					}
				}
				if cmd.Flags().Changed("page") {
					return errors.New("--page cannot be used with --code")
				}

				// For code search, only extract repo: inline filters from
				// the query. Other checkpoint filters (author:, date:,
				// branch:) are not supported and must be preserved as
				// literal search text so "author:foo" searches for that
				// string in code rather than being silently consumed.
				codeQuery, inlineRepos := extractInlineRepoFilters(query)
				var codeRepos []string
				if repoFlag != "" {
					codeRepos = []string{repoFlag}
				}
				codeRepos = append(codeRepos, inlineRepos...)
				// repo:* or --all-repos means "all repos" — no filter.
				// Otherwise, if no explicit filter was given, scope to the
				// current repo (matching the checkpoint-search default).
				hasAllRepos := allReposFlag
				for _, r := range codeRepos {
					if r == search.AllReposFilter {
						hasAllRepos = true
					}
				}
				if hasAllRepos {
					codeRepos = nil
				} else {
					// Remove any stray "*" entries.
					filtered := codeRepos[:0]
					for _, r := range codeRepos {
						if r != search.AllReposFilter {
							filtered = append(filtered, r)
						}
					}
					codeRepos = filtered

					// No explicit repo filter → derive from git origin remote.
					// Use forge-prefixed slug so et/ forge repos match the index.
					if len(codeRepos) == 0 {
						slug := currentRepoSlugWithForge(ctx)
						if slug == "" {
							return errors.New("could not determine current repository for code search (use --repo or --all-repos)")
						}
						codeRepos = []string{slug}
					}
				}
				return runCodeSearch(ctx, cmd, codeSearchOpts{
					query:         codeQuery,
					repoFilters:   codeRepos,
					limit:         limitFlag,
					limitExplicit: cmd.Flags().Changed("limit"),
					caseSensitive: caseSensitive,
					jsonOutput:    jsonOutput,
					insecureHTTP:  insecureHTTPAuth,
				})
			}

			// Extract inline filters (author:, date:, branch:, repo:) from query args.
			// Keep the raw query for code search (which preserves author:/date:/branch:
			// as literal text via extractInlineRepoFilters).
			rawQuery := query
			parsed := search.ParseSearchInput(query)
			query = parsed.Query
			if authorFlag == "" {
				authorFlag = parsed.Author
			}
			if dateFlag == "" {
				dateFlag = parsed.Date
			}
			if branchFlag == "" {
				branchFlag = parsed.Branch
			}
			repos := parsed.Repos
			if repoFlag != "" {
				repos = []string{repoFlag}
			}
			if err := search.ValidateRepoFilters(repos); err != nil {
				return fmt.Errorf("validating repo filter: %w", err)
			}

			// Check for repo:* in inline filters
			allRepos := allReposFlag
			if len(repos) == 1 && repos[0] == search.AllReposFilter {
				allRepos = true
			}

			w := cmd.OutOrStdout()
			isTerminal := interactive.IsTerminalWriter(w)
			// Mirror search.Config.HasFilters (incl. --all-repos) so an empty
			// query with only filters isn't rejected here. This guard runs
			// before git/auth, so it can't call searchCfg.HasFilters() directly.
			hasFilters := authorFlag != "" || dateFlag != "" || branchFlag != "" || len(repos) > 0 || allRepos

			// Fast-fail: no query + non-interactive mode = error (before auth/git checks)
			if query == "" && !hasFilters && (jsonOutput || !isTerminal || IsAccessibleMode()) {
				return errors.New("query required when using --json, accessible mode, or piped output. Usage: entire search <query>")
			}

			// Get the repo's GitHub remote URL
			repo, err := strategy.OpenRepository(ctx)
			if err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run this command from within a git repository.")
				return NewSilentError(err)
			}
			defer repo.Close()

			remote, err := repo.Remote("origin")
			if err != nil {
				return fmt.Errorf("could not find 'origin' remote: %w", err)
			}
			urls := remote.Config().URLs
			if len(urls) == 0 {
				return errors.New("origin remote has no URLs configured")
			}

			owner, repoName, err := search.ParseGitHubRemote(urls[0])
			if err != nil {
				return fmt.Errorf("parsing remote URL: %w", err)
			}

			serviceURL := os.Getenv("ENTIRE_SEARCH_URL")
			if serviceURL == "" {
				// Search lives on the data API host. Fall back to
				// api.BaseURL() so ENTIRE_API_BASE_URL applies; the search
				// package's DefaultServiceURL is only consulted by callers
				// that bypass this entry point.
				serviceURL = api.BaseURL()
			}

			ghToken, err := resolveSearchToken(ctx, serviceURL, insecureHTTPAuth)
			if err != nil {
				return err
			}

			searchCfg := search.Config{
				ServiceURL:  serviceURL,
				GitHubToken: ghToken,
				Owner:       owner,
				Repo:        repoName,
				Repos:       repos,
				AllRepos:    allRepos,
				Query:       query,
				Limit:       limitFlag,
				Page:        pageFlag,
				Author:      authorFlag,
				Date:        dateFlag,
				Branch:      branchFlag,
			}

			// Use wildcard query when only filters are provided
			if query == "" && searchCfg.HasFilters() {
				searchCfg.Query = search.WildcardQuery
			}

			// No query provided + interactive = open TUI with search bar focused
			if query == "" && !searchCfg.HasFilters() {
				searchCfg.Limit = search.DefaultLimit
				styles := newStatusStyles(w)
				model := newSearchModel(nil, "", 0, searchCfg, styles, buildCodeSearchOpts(ctx, owner, repoName, nil, false, insecureHTTPAuth))
				model.mode = modeSearch
				model.input.Focus()
				model.codeLoading = false // don't fetch until a query is entered
				p := tea.NewProgram(model)
				if _, err := p.Run(); err != nil {
					return fmt.Errorf("TUI error: %w", err)
				}
				return nil
			}

			// Fetch a full page (DefaultLimit, matching the web UI) up front and
			// paginate client-side for all output modes; the requested --limit
			// only controls the client-side page size.
			requestedLimit := searchCfg.Limit
			requestedPage := searchCfg.Page
			searchCfg.Limit = search.DefaultLimit
			searchCfg.Page = 0 // let API default to page 1

			resp, err := search.Search(ctx, searchCfg)
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			// JSON output: explicit flag or piped/redirected stdout
			if jsonOutput || !isTerminal {
				return writeSearchJSON(w, resp, requestedLimit, requestedPage)
			}

			styles := newStatusStyles(w)

			// Accessible mode: static table
			if IsAccessibleMode() {
				if len(resp.Results) == 0 {
					fmt.Fprintln(w, "No results found.")
					return nil
				}
				renderSearchStatic(w, resp.Results, query, resp.Total, styles)
				return nil
			}

			// Interactive TUI
			codeOpts := buildCodeSearchOpts(ctx, owner, repoName, repos, allRepos, insecureHTTPAuth)
			if codeOpts != nil {
				// Use extractInlineRepoFilters on the raw query so author:/date:/branch:
				// tokens are preserved as literal code-search text, matching --code and
				// the TUI submit path. Inline repo: filters override the flag-based
				// scope, consistent with TUI re-search behavior.
				codeQuery, inlineRepos := extractInlineRepoFilters(rawQuery)
				codeOpts.query = codeQuery // empty → no initial code search (gated in newSearchModel)
				if len(inlineRepos) > 0 {
					if hasAllReposFilter(inlineRepos) {
						codeOpts.repoFilters = nil
					} else {
						codeOpts.repoFilters = inlineRepos
					}
				}
			}
			model := newSearchModel(resp.Results, query, resp.Total, searchCfg, styles, codeOpts)
			p := tea.NewProgram(model)
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&codeFlag, "code", false, "Search code content across repositories")
	cmd.Flags().BoolVar(&caseSensitive, "case-sensitive", false, "Case-sensitive code search (only with --code)")
	cmd.Flags().IntVar(&limitFlag, "limit", resultsPerPage, "Maximum number of results (per page for checkpoint search, total for --code)")
	cmd.Flags().IntVar(&pageFlag, "page", 1, "Page number (1-based)")
	cmd.Flags().StringVar(&authorFlag, "author", "", "Filter by author name")
	cmd.Flags().StringVar(&dateFlag, "date", "", "Filter by time period (week or month)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Filter by branch name")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "Filter by repository (gh/owner/repo, et/proj/repo, owner/repo, ULID, or *)")
	cmd.Flags().BoolVar(&allReposFlag, "all-repos", false, "Search all accessible repos instead of just the current one")
	addInsecureHTTPAuthFlag(cmd, &insecureHTTPAuth)

	cmd.RegisterFlagCompletionFunc("date", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) { //nolint:errcheck,gosec // only fails if the flag isn't defined; defined directly above
		return []string{"week", "month"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("repo", completeRepoFlag) //nolint:errcheck,gosec // only fails if the flag isn't defined; defined directly above

	return cmd
}

// resolveSearchToken returns a bearer scoped to the search service host.
// In split-host deployments this triggers an RFC 8693 exchange so the bearer
// carries the data-API audience rather than the auth-host one; single-host
// setups hit the same-host shortcut and return the core token unchanged.
// insecureHTTPAuth opts into non-loopback http:// resources at the
// tokenmanager layer, matching the per-command --insecure-http-auth pattern
// used by NewAuthenticatedAPIClient and newRecapClient.
func resolveSearchToken(ctx context.Context, serviceURL string, insecureHTTPAuth bool) (string, error) {
	if insecureHTTPAuth {
		auth.EnableInsecureHTTP()
	}
	token, err := auth.ResolveDataAPIToken(ctx, serviceURL)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return "", errors.New("not authenticated. Run 'entire login' to authenticate")
	}
	if err != nil {
		return "", fmt.Errorf("reading credentials: %w", err)
	}
	return token, nil
}

// completeRepoFlag returns shell-completion suggestions for the search
// command's --repo flag. "*" is always offered so the wildcard works
// regardless of auth state. Errors are swallowed (rather than surfaced via
// ShellCompDirectiveError) because completion runs on every TAB press and
// must never pollute the user's prompt with error output.
func completeRepoFlag(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	suggestions := []string{"*"}
	client, err := NewAuthenticatedAPIClient(cmd.Context(), false)
	if err != nil {
		return suggestions, cobra.ShellCompDirectiveNoFileComp
	}
	repos, err := client.ListRepositories(cmd.Context(), api.RepositorySortRecent)
	if err != nil {
		return suggestions, cobra.ShellCompDirectiveNoFileComp
	}
	for _, r := range repos {
		if r.CheckpointCount == 0 {
			continue // searching a repo with no checkpoints would always be empty
		}
		suggestions = append(suggestions, r.FullName)
	}
	return suggestions, cobra.ShellCompDirectiveNoFileComp
}

// codeSearchEnabled reports whether the code search feature is gated on.
func codeSearchEnabled() bool {
	return os.Getenv("ENTIRE_CODE_SEARCH") == "1"
}

type codeSearchOpts struct {
	query           string
	repoFilters     []string
	resolvedRepoIDs []string // ULIDs resolved from repoFilters via repo index
	limit           int
	limitExplicit   bool // user passed --limit; don't override for text display
	caseSensitive   bool
	jsonOutput      bool
	insecureHTTP    bool
}

// extractInlineRepoFilters extracts only repo: prefixed filters from a query
// string, returning the remaining query text and the list of repo values.
// Unlike search.ParseSearchInput, this does NOT consume author:, date:, or
// branch: tokens — those are checkpoint-search-only and should be treated as
// literal text in code search queries.
func extractInlineRepoFilters(query string) (remaining string, repos []string) {
	var kept []string
	for _, part := range strings.Fields(query) {
		if strings.HasPrefix(part, "repo:") {
			// Split comma-separated values (repo:a,b → [a, b]), matching
			// checkpoint search's parseListFilter behavior. Trim quotes so
			// repo:"gh/owner/repo" works like the unquoted form.
			for _, v := range strings.Split(part[5:], ",") {
				v = strings.Trim(v, `"'`)
				if v != "" {
					repos = append(repos, v)
				}
			}
		} else {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " "), repos
}

// hasAllReposFilter returns true if repos contains the wildcard "*" filter.
func hasAllReposFilter(repos []string) bool {
	for _, r := range repos {
		if r == search.AllReposFilter {
			return true
		}
	}
	return false
}

// filterRepoWildcards returns repos with AllReposFilter entries removed.
func filterRepoWildcards(repos []string) []string {
	var out []string
	for _, r := range repos {
		if r != search.AllReposFilter {
			out = append(out, r)
		}
	}
	return out
}

// buildCodeSearchOpts returns a *codeSearchOpts pre-populated with repo filters
// when ENTIRE_CODE_SEARCH=1 is set, or nil when the feature is off. It honors
// --repo, --all-repos, and inline repo: filters from the command line; when none
// are specified, it falls back to the current git origin slug.
func buildCodeSearchOpts(ctx context.Context, owner, repoName string, repos []string, allRepos, insecureHTTP bool) *codeSearchOpts {
	if !codeSearchEnabled() {
		return nil
	}
	var repoFilters []string
	switch {
	case allRepos:
		// nil repoFilters → searchAllCells searches all repos
	case len(repos) > 0:
		repoFilters = repos
	default:
		// Use forge-prefixed slug (e.g. "et/proj/repo") so Entire forge
		// repos match the index FullName. Falls back to owner/repo for
		// GitHub repos (gh/ prefix is stripped by resolveRepoFilters).
		if slug := currentRepoSlugWithForge(ctx); slug != "" {
			repoFilters = []string{slug}
		} else {
			repoFilters = []string{owner + "/" + repoName}
		}
	}
	return &codeSearchOpts{
		repoFilters:  repoFilters,
		limit:        search.DefaultLimit,
		insecureHTTP: insecureHTTP,
	}
}

// codeSearchCellTimeout bounds each per-cell search call (token exchange + API).
const codeSearchCellTimeout = 30 * time.Second

// runCodeSearch handles the --code flag path: search code content via peregrine.
//
// When a repo filter is specified, it routes to that repo's owning cell.
// Without a filter, it fans out across all cells that host the user's repos
// (mirroring the BFF's /api/v1/stream endpoint): list repos from the control
// plane, group by cell/jurisdiction, search each cell in parallel, merge.
func runCodeSearch(ctx context.Context, cmd *cobra.Command, opts codeSearchOpts) error {
	if !codeSearchEnabled() {
		return errors.New("code search is not yet available")
	}

	if opts.query == "" {
		return errors.New("query required for code search. Usage: entire search --code <query>")
	}

	w := cmd.OutOrStdout()
	textOutput := !opts.jsonOutput && interactive.IsTerminalWriter(w)

	// Text output shows up to maxCodeSearchFiles files with a few matches
	// each, so fetch a deeper result set than the default --limit (which is
	// tuned for flat JSON output) unless the user asked for a specific limit.
	if textOutput && !opts.limitExplicit {
		opts.limit = codeSearchTextFetchLimit
	}

	// Always fan out via searchAllCells — it fetches the repo index,
	// resolves slugs to ULIDs, and handles single- vs multi-jurisdiction.
	resp, err := searchAllCells(ctx, opts)
	if err != nil {
		return err
	}

	if !textOutput {
		return writeCodeSearchJSON(w, resp)
	}

	writeCodeSearchText(w, resp, newStatusStyles(w), opts.caseSensitive)
	return nil
}

// searchAllCells fans out code search across all cells that host the user's
// repos, using the shared cell-routing foundation (cell_fanout.go):
//  1. List repos from the control plane (entire-core) to discover cells
//  2. Resolve repo slug filters to ULIDs
//  3. Group by cell and resolve baseURLs via the shared helpers
//  4. Fan out via fanOutCells with per-cell codesearch.Search calls
//  5. Merge results (sorted by score, capped to limit)
func searchAllCells(ctx context.Context, opts codeSearchOpts) (*codesearch.SearchResponse, error) {
	// Step 1: Get repos index from the control plane.
	// coreapi.Client satisfies cellCoreClient (for resolveCellBaseURLs)
	// and also provides ListRepos (which cellCoreClient doesn't expose).
	coreClient, err := coreapi.New()
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("resolving control-plane client: %w", err)
	}

	reposCtx, reposCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reposCancel()

	repoIndex, err := coreClient.ListRepos(reposCtx, coreapi.ListReposParams{})
	if err != nil {
		return nil, fmt.Errorf("listing repos for cell discovery: %w", err)
	}

	if repoIndex.Truncated {
		logging.Warn(ctx, "repo index truncated; code search results may be incomplete")
	}

	// Step 2: Resolve repo slug filters to ULIDs and narrow to matching cells.
	indexRepos := repoIndex.Repos
	if len(opts.repoFilters) > 0 {
		resolved, filtered := resolveRepoFilters(opts.repoFilters, repoIndex.Repos)
		if len(resolved) == 0 {
			hint := ""
			if repoIndex.Truncated {
				hint = " (repo index was truncated — the repo may exist but was not included)"
			}
			return nil, fmt.Errorf("no matching repositories found for filter %q%s", opts.repoFilters, hint)
		}
		opts.resolvedRepoIDs = resolved
		indexRepos = filtered
	}

	// Step 3: Group repos by cell and resolve baseURLs via shared helpers.
	cells := groupReposByCell(indexRepos)
	if len(cells) == 0 {
		return &codesearch.SearchResponse{}, nil
	}
	resolveCellBaseURLs(ctx, coreClient, cells)

	// Step 4: Fan out via the shared fanOutCells helper.
	// Each cell gets the full limit for single-cell, or 2x for multi-cell so
	// the merge sees enough candidates from every region for proper global
	// ranking. mergeSearchResults applies the final cap.
	perCellLimit := opts.limit
	if len(cells) > 1 && perCellLimit > 0 {
		perCellLimit *= 2
	}
	results, err := fanOutCells(ctx, opts.insecureHTTP, codeSearchCellTimeout, cells, func(ctx context.Context, group cellGroup, client *api.Client) (*codesearch.SearchResponse, error) {
		var repoIDs []string
		if len(opts.resolvedRepoIDs) > 0 {
			repoIDs = group.repoIDs
		}
		req := codesearch.SearchRequest{
			Query:         opts.query,
			Repos:         repoIDs,
			CaseSensitive: opts.caseSensitive,
		}
		if perCellLimit > 0 {
			req.MaxResults = perCellLimit
		}
		return codesearch.Search(ctx, client, req)
	})
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("code search: %w", err)
	}

	return mergeSearchResults(ctx, opts.limit, results)
}

// resolveRepoFilters matches user-provided filters against the repo index,
// returning the ULID list for peregrine and the subset of index entries whose
// repos matched (for cell grouping).
//
// Matching mirrors the BFF (code-search.ts lines 315-319):
//
//	slug = filter starts with "gh/" ? strip prefix : filter unchanged
//	match = id === filter || full_name === slug || full_name === filter
//
// Accepted filter formats:
//   - ULID            — matched directly on repo ID (raw filter)
//   - gh/owner/repo   — GitHub repo, stripped to owner/repo for FullName match
//   - owner/repo      — bare slug, matched on FullName directly
func resolveRepoFilters(filters []string, repos []coreapi.RepoIndexEntry) (repoIDs []string, matched []coreapi.RepoIndexEntry) {
	byName := make(map[string]coreapi.RepoIndexEntry, len(repos))
	byID := make(map[string]coreapi.RepoIndexEntry, len(repos))
	for _, r := range repos {
		byName[strings.ToLower(r.FullName)] = r
		byID[r.ID] = r
	}
	seen := make(map[string]bool) // dedup by ID
	for _, f := range filters {
		// BFF only strips gh/ prefix; other prefixes are left as-is.
		slug := f
		if strings.HasPrefix(f, "gh/") {
			slug = f[3:]
		}

		// Match order mirrors the BFF: id === filter || full_name === slug || full_name === filter
		// FullName comparison is case-insensitive so casing differences between
		// the git remote (e.g. entireio/CLI) and the repo index (entireio/cli)
		// don't cause a "no matching repositories found" failure.
		var r coreapi.RepoIndexEntry
		var ok bool
		if r, ok = byID[f]; !ok {
			if r, ok = byName[strings.ToLower(slug)]; !ok {
				r, ok = byName[strings.ToLower(f)]
			}
		}
		if ok && !seen[r.ID] {
			repoIDs = append(repoIDs, r.ID)
			matched = append(matched, r)
			seen[r.ID] = true
		}
	}
	return repoIDs, matched
}

// mergeSearchResults merges responses from multiple cells into one, combining
// results, stats, and repo_stats. Results are sorted by Score (descending) for
// global relevance ranking and truncated to limit. Individual cell errors are
// logged and skipped, but if ALL cells fail the error is surfaced.
func mergeSearchResults(ctx context.Context, limit int, results []cellCallResult[*codesearch.SearchResponse]) (*codesearch.SearchResponse, error) {
	merged := &codesearch.SearchResponse{}
	var lastErr error
	successCount := 0
	for _, r := range results {
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if r.value == nil {
			continue
		}
		successCount++
		merged.Results = append(merged.Results, r.value.Results...)
		merged.RepoStats = append(merged.RepoStats, r.value.RepoStats...)
		merged.Stats.TotalMatches += r.value.Stats.TotalMatches
		merged.Stats.TotalFiles += r.value.Stats.TotalFiles
		merged.Stats.ReposSearched += r.value.Stats.ReposSearched
		if r.value.Stats.DurationMs > merged.Stats.DurationMs {
			merged.Stats.DurationMs = r.value.Stats.DurationMs // wall-clock = slowest cell
		}
		if merged.Query == "" {
			merged.Query = r.value.Query
		}
	}

	if successCount == 0 && lastErr != nil {
		return nil, fmt.Errorf("code search failed: %w", lastErr)
	}

	// Track partial failures so consumers (especially --json) can see them.
	var failedJurisdictions []string
	for _, r := range results {
		if r.err == nil {
			continue
		}
		failedJurisdictions = append(failedJurisdictions, r.group.label())
	}
	if len(failedJurisdictions) > 0 {
		logging.Warn(ctx, "code search partial failure; results may be incomplete",
			"succeeded", successCount,
			"total", len(results),
			"failed_cells", failedJurisdictions)
	}

	// Sort by score descending so results are globally ranked by relevance,
	// not grouped by whichever cell returned first. Stable sort with a
	// tiebreaker keeps --json output deterministic across runs.
	sort.SliceStable(merged.Results, func(i, j int) bool {
		a, b := merged.Results[i], merged.Results[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})

	// Deduplicate results that may appear from overlapping cells (e.g. a repo
	// with empty jurisdiction searched via both home and explicit cell).
	seen := make(map[string]bool, len(merged.Results))
	deduped := merged.Results[:0]
	for _, r := range merged.Results {
		key := r.Repo + "\x00" + r.Path + "\x00" + fmt.Sprintf("%d:%d", r.Line, r.Column)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, r)
	}
	merged.Results = deduped

	// Deduplicate RepoStats by repo name. A repo that appears in more than one
	// cell is a mirror placement returning the SAME content (this PR fans out
	// across placements, so e.g. a US-homed repo with an EU mirror is now
	// searched in both cells) — not additional matches. Keep one representative
	// entry per repo (the max of each count; mirror copies are identical, max
	// only guards against minor per-cell skew) instead of summing, and record
	// the duplicated portion so the aggregate stats can drop the double-count.
	type repoStatAcc struct {
		idx                    int
		sumMatches, maxMatches int
		sumFiles, maxFiles     int
		cellCount              int
	}
	accByRepo := make(map[string]*repoStatAcc, len(merged.RepoStats))
	var dedupedStats []codesearch.RepoStats
	for _, rs := range merged.RepoStats {
		acc, ok := accByRepo[rs.Repo]
		if !ok {
			acc = &repoStatAcc{idx: len(dedupedStats)}
			accByRepo[rs.Repo] = acc
			dedupedStats = append(dedupedStats, codesearch.RepoStats{Repo: rs.Repo})
		}
		acc.cellCount++
		acc.sumMatches += rs.MatchCount
		acc.sumFiles += rs.FileCount
		acc.maxMatches = max(acc.maxMatches, rs.MatchCount)
		acc.maxFiles = max(acc.maxFiles, rs.FileCount)
	}
	var overcountMatches, overcountFiles, overcountRepos int
	for _, acc := range accByRepo {
		dedupedStats[acc.idx].MatchCount = acc.maxMatches
		dedupedStats[acc.idx].FileCount = acc.maxFiles
		overcountMatches += acc.sumMatches - acc.maxMatches
		overcountFiles += acc.sumFiles - acc.maxFiles
		overcountRepos += acc.cellCount - 1
	}
	merged.RepoStats = dedupedStats

	// The per-cell Stats were summed above, so a mirrored repo's matches were
	// counted once per cell. Subtract the duplicated copies identified via
	// RepoStats so the totals reflect distinct content, not the same content
	// seen from every mirror cell. This preserves per-cell truncation (the
	// base is peregrine's own totals; we only remove the provable duplicate
	// portion) and zero-match repos (they contribute 0 to the subtraction).
	// A repo with matches but no RepoStats row, or a zero-match mirror repo,
	// can't be de-duplicated from the response and keeps its summed
	// contribution — a mild over-count, far less misleading than reporting
	// every mirrored match twice. Clamp at zero against inconsistent input.
	merged.Stats.TotalMatches = max(0, merged.Stats.TotalMatches-overcountMatches)
	merged.Stats.TotalFiles = max(0, merged.Stats.TotalFiles-overcountFiles)
	merged.Stats.ReposSearched = max(0, merged.Stats.ReposSearched-overcountRepos)

	// Cap to the caller's requested limit.
	if limit > 0 && len(merged.Results) > limit {
		merged.Results = merged.Results[:limit]
	}

	// Surface partial failures in the response so JSON consumers can detect them.
	merged.FailedJurisdictions = failedJurisdictions

	return merged, nil
}

// writeCodeSearchJSON writes code search results as JSON.
func writeCodeSearchJSON(w io.Writer, resp *codesearch.SearchResponse) error {
	out := struct {
		Query               string                 `json:"query"`
		Results             []codesearch.Result    `json:"results"`
		Total               int                    `json:"total"`
		Stats               codesearch.Stats       `json:"stats"`
		RepoStats           []codesearch.RepoStats `json:"repo_stats,omitempty"`
		FailedJurisdictions []string               `json:"failed_jurisdictions,omitempty"`
	}{
		Query:               resp.Query,
		Results:             resp.Results,
		Total:               len(resp.Results),
		Stats:               resp.Stats,
		RepoStats:           resp.RepoStats,
		FailedJurisdictions: resp.FailedJurisdictions,
	}
	if out.Results == nil {
		out.Results = []codesearch.Result{}
	}
	data, err := jsonutil.MarshalIndentWithNewline(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling code search results: %w", err)
	}
	fmt.Fprint(w, string(data))
	return nil
}

// maxContextLineLen is the maximum number of characters to display for a
// context_line in grep-style text output. Lines longer than this are truncated
// with an ellipsis so that JSONL/minified files don't blow up the terminal.
const maxContextLineLen = 200

// Text output display caps: show breadth (files) over depth (in-file matches).
// The fetch limit leaves headroom beyond files×matches so per-file overflow
// ("+ N matches") counts have data to count.
const (
	maxCodeSearchFiles       = 10  // files shown in text output
	maxCodeSearchFileMatches = 3   // matches shown per file
	codeSearchTextFetchLimit = 100 // results fetched for text display
)

// writeCodeSearchText renders code search results grouped by file (ripgrep
// style): a colored "repo:path" header per file, indented line-numbered
// matches beneath it, and a dimmed stats footer. Colors are applied only when
// the writer supports them (styles.colorEnabled); piped output stays plain.
func writeCodeSearchText(w io.Writer, resp *codesearch.SearchResponse, styles statusStyles, caseSensitive bool) {
	if len(resp.Results) == 0 {
		if len(resp.FailedJurisdictions) > 0 {
			fmt.Fprintf(w, "No code search results found (some regions failed: %s)\n",
				strings.Join(resp.FailedJurisdictions, ", "))
		} else {
			fmt.Fprintln(w, "No code search results found.")
		}
		return
	}

	// Group results by repo:path, preserving first-appearance order so the
	// best-scored file stays on top (results arrive globally score-sorted).
	type fileGroup struct {
		key     string
		results []codesearch.Result
	}
	var groups []fileGroup
	idx := make(map[string]int, len(resp.Results))
	for _, r := range resp.Results {
		key := r.Repo + ":" + r.Path
		i, ok := idx[key]
		if !ok {
			i = len(groups)
			idx[key] = i
			groups = append(groups, fileGroup{key: key})
		}
		groups[i].results = append(groups[i].results, r)
	}

	shown := 0
	for gi, g := range groups {
		if gi == maxCodeSearchFiles {
			break
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, styles.render(styles.cyan, g.key))
		for mi, r := range g.results {
			if mi == maxCodeSearchFileMatches {
				break
			}
			// Truncate before highlighting but append the ellipsis after,
			// so the non-ASCII "…" doesn't disable case-insensitive
			// highlighting (isASCII) for the rest of the line.
			line := r.ContextLine
			ellipsis := ""
			if runes := []rune(line); len(runes) > maxContextLineLen {
				line = string(runes[:maxContextLineLen])
				ellipsis = "…"
			}
			lineNo := styles.render(styles.dim, fmt.Sprintf("%d:", r.Line))
			fmt.Fprintf(w, "  %s %s%s\n", lineNo, highlightCodeMatches(line, resp.Query, styles, caseSensitive), ellipsis)
			shown++
		}
		// ponytail: overflow counts only what this page fetched (peregrine
		// has no per-file totals); a hot file shows "+ 97 matches" at most.
		if extra := len(g.results) - maxCodeSearchFileMatches; extra > 0 {
			label := "matches"
			if extra == 1 {
				label = "match"
			}
			fmt.Fprintln(w, styles.render(styles.dim, fmt.Sprintf("  + %d %s", extra, label)))
		}
	}

	var summary string
	if resp.Stats.TotalMatches > shown {
		summary = fmt.Sprintf("Showing %d of %d matches across %d files in %d repos (%.0fms)",
			shown, resp.Stats.TotalMatches, resp.Stats.TotalFiles, resp.Stats.ReposSearched, resp.Stats.DurationMs)
	} else {
		summary = fmt.Sprintf("%d matches across %d files in %d repos (%.0fms)",
			resp.Stats.TotalMatches, resp.Stats.TotalFiles, resp.Stats.ReposSearched, resp.Stats.DurationMs)
	}
	fmt.Fprintf(w, "\n%s\n", styles.render(styles.dim, summary))
	if len(resp.FailedJurisdictions) > 0 {
		warning := fmt.Sprintf("Warning: results may be incomplete (failed jurisdictions: %s)",
			strings.Join(resp.FailedJurisdictions, ", "))
		fmt.Fprintln(w, styles.render(styles.yellow, warning))
	}
}

// highlightCodeMatches bold-red highlights occurrences of query in line
// (grep convention). Matching mirrors the search: case-insensitive unless
// caseSensitive is set. Case folding is only applied when both strings are
// pure ASCII, since Unicode case mappings can change byte widths and
// misalign offsets against the original line; non-ASCII input falls back to
// exact matching. Returns line unchanged when color is disabled or there's
// nothing to highlight.
func highlightCodeMatches(line, query string, styles statusStyles, caseSensitive bool) string {
	if !styles.colorEnabled || query == "" {
		return line
	}
	haystack, needle := line, query
	if !caseSensitive && isASCII(line) && isASCII(query) {
		haystack, needle = strings.ToLower(line), strings.ToLower(query)
	}
	matchStyle := styles.red.Bold(true)
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(haystack[i:], needle)
		if j < 0 {
			break
		}
		j += i
		b.WriteString(line[i:j])
		b.WriteString(matchStyle.Render(line[j : j+len(needle)]))
		i = j + len(needle)
	}
	if i == 0 {
		return line // no matches; skip the builder copy
	}
	b.WriteString(line[i:])
	return b.String()
}

// isASCII reports whether s contains only ASCII bytes.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// writeSearchJSON writes client-side paginated search results as JSON.
func writeSearchJSON(w io.Writer, resp *search.Response, limit, page int) error {
	if limit <= 0 {
		limit = resultsPerPage
	}

	total := len(resp.Results)
	totalPages := (total + limit - 1) / limit
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}

	// Slice results for the requested page.
	start := (page - 1) * limit
	end := start + limit
	var pageResults []search.Result
	if start < total {
		if end > total {
			end = total
		}
		pageResults = resp.Results[start:end]
	}
	if pageResults == nil {
		pageResults = []search.Result{}
	}

	out := struct {
		Results    []search.Result    `json:"results"`
		Total      int                `json:"total"`
		Page       int                `json:"page"`
		TotalPages int                `json:"total_pages"`
		Limit      int                `json:"limit"`
		Counts     *search.TypeCounts `json:"counts,omitempty"`
	}{
		Results:    pageResults,
		Total:      total,
		Page:       page,
		TotalPages: totalPages,
		Limit:      limit,
		Counts:     resp.Counts,
	}
	data, err := jsonutil.MarshalIndentWithNewline(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}
	fmt.Fprint(w, string(data))
	return nil
}
