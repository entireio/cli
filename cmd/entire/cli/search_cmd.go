package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

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
				// Parse inline repo: filters from the query for --code too.
				parsed := search.ParseSearchInput(query)
				codeRepo := repoFlag
				if codeRepo == "" && len(parsed.Repos) > 0 {
					codeRepo = parsed.Repos[0]
				}
				// repo:* means "all repos" — treat as no filter for code search.
				if codeRepo == search.AllReposFilter || allReposFlag {
					codeRepo = ""
				}
				return runCodeSearch(ctx, cmd, codeSearchOpts{
					query:         parsed.Query,
					repoFilter:    codeRepo,
					limit:         limitFlag,
					caseSensitive: caseSensitive,
					jsonOutput:    jsonOutput,
					insecureHTTP:  insecureHTTPAuth,
				})
			}

			// Extract inline filters (author:, date:, branch:, repo:) from query args
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
				model := newSearchModel(nil, "", 0, searchCfg, styles)
				model.mode = modeSearch
				model.input.Focus()
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
			model := newSearchModel(resp.Results, query, resp.Total, searchCfg, styles)
			p := tea.NewProgram(model)
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&codeFlag, "code", false, "Search code content via peregrine (requires ENTIRE_CODE_SEARCH=1)")
	cmd.Flags().BoolVar(&caseSensitive, "case-sensitive", false, "Case-sensitive code search (only with --code)")
	cmd.Flags().IntVar(&limitFlag, "limit", resultsPerPage, "Maximum number of results (per page for checkpoint search, total for --code)")
	cmd.Flags().IntVar(&pageFlag, "page", 1, "Page number (1-based)")
	cmd.Flags().StringVar(&authorFlag, "author", "", "Filter by author name")
	cmd.Flags().StringVar(&dateFlag, "date", "", "Filter by time period (week or month)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Filter by branch name")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "Filter by repository (owner/name or *)")
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
	query         string
	repoFilter    string
	limit         int
	caseSensitive bool
	jsonOutput    bool
	insecureHTTP  bool
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
		return errors.New("code search is not yet available. Set ENTIRE_CODE_SEARCH=1 to enable the preview")
	}

	if opts.query == "" {
		return errors.New("query required for code search. Usage: entire search --code <query>")
	}

	if opts.repoFilter != "" {
		if err := search.ValidateRepoFilters([]string{opts.repoFilter}); err != nil {
			return fmt.Errorf("validating repo filter: %w", err)
		}
	}

	w := cmd.OutOrStdout()

	var resp *codesearch.SearchResponse
	var err error

	if opts.repoFilter != "" {
		resp, err = searchSingleCell(ctx, opts)
	} else {
		resp, err = searchAllCells(ctx, opts)
	}
	if err != nil {
		return err
	}

	isTerminal := interactive.IsTerminalWriter(w)
	if opts.jsonOutput || !isTerminal {
		return writeCodeSearchJSON(w, resp)
	}

	writeCodeSearchText(w, resp)
	return nil
}

// searchSingleCell searches a single cell, routing to the repo's owning cell.
func searchSingleCell(ctx context.Context, opts codeSearchOpts) (*codesearch.SearchResponse, error) {
	client, err := NewAuthenticatedEntireAPICellClient(ctx, opts.insecureHTTP, opts.repoFilter, "")
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("resolving cell client: %w", err)
	}

	req := codesearch.SearchRequest{
		Query:         opts.query,
		Repos:         []string{opts.repoFilter},
		CaseSensitive: opts.caseSensitive,
	}
	if opts.limit > 0 {
		req.MaxResults = opts.limit
	}

	searchCtx, cancel := context.WithTimeout(ctx, codeSearchCellTimeout)
	defer cancel()

	resp, err := codesearch.Search(searchCtx, client, req)
	if err != nil {
		return nil, fmt.Errorf("code search failed: %w", err)
	}
	return resp, nil
}

// cellGroup groups repos by their cell for fan-out.
type cellGroup struct {
	cell         string
	jurisdiction string
}

// searchAllCells fans out code search across all cells that host the user's
// repos, mirroring the BFF's multi-region search pattern:
//  1. List repos from the control plane (entire-core) to get cell/jurisdiction
//  2. Deduplicate by cell
//  3. Create a cell client per jurisdiction (token exchange)
//  4. Search each cell in parallel with per-cell timeouts
//  5. Merge results
func searchAllCells(ctx context.Context, opts codeSearchOpts) (*codesearch.SearchResponse, error) {
	// Step 1: Get repos index from the control plane.
	coreClient, err := coreapi.New()
	if err != nil {
		if errors.Is(err, auth.ErrNotLoggedIn) {
			return nil, errors.New("not authenticated. Run 'entire login' to authenticate")
		}
		return nil, fmt.Errorf("resolving control-plane client: %w", err)
	}

	reposCtx, reposCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reposCancel()

	repoIndex, err := coreClient.ListRepos(reposCtx)
	if err != nil {
		return nil, fmt.Errorf("listing repos for cell discovery: %w", err)
	}

	// Step 2: Group repos by cell (deduplicate).
	cells := groupReposByCell(repoIndex.Repos)
	if len(cells) == 0 {
		return &codesearch.SearchResponse{}, nil
	}

	// Single cell — skip fan-out overhead.
	if len(cells) == 1 {
		return searchCell(ctx, opts, cells[0])
	}

	// Step 3–5: Fan out across cells in parallel.
	results := make([]codeSearchCellResult, len(cells))
	var wg sync.WaitGroup
	for i, cg := range cells {
		wg.Add(1)
		go func(idx int, cg cellGroup) {
			defer wg.Done()
			resp, err := searchCell(ctx, opts, cg)
			results[idx] = codeSearchCellResult{resp: resp, err: err}
		}(i, cg)
	}
	wg.Wait()

	return mergeSearchResults(ctx, cells, results), nil
}

// groupReposByCell deduplicates repos by cell, returning one entry per cell
// with its jurisdiction.
func groupReposByCell(repos []coreapi.RepoIndexEntry) []cellGroup {
	seen := make(map[string]string) // cell → jurisdiction
	for _, r := range repos {
		cell := strings.TrimSpace(r.Cell)
		if cell == "" {
			continue
		}
		if _, ok := seen[cell]; !ok {
			seen[cell] = strings.ToLower(strings.TrimSpace(r.Jurisdiction))
		}
	}
	groups := make([]cellGroup, 0, len(seen))
	for cell, jurisdiction := range seen {
		groups = append(groups, cellGroup{cell: cell, jurisdiction: jurisdiction})
	}
	return groups
}

// searchCell searches a single cell identified by jurisdiction, using
// auth.NewEntireAPICellClient with an explicit CellTarget.
func searchCell(ctx context.Context, opts codeSearchOpts, cg cellGroup) (*codesearch.SearchResponse, error) {
	cellCtx, cancel := context.WithTimeout(ctx, codeSearchCellTimeout)
	defer cancel()

	client, err := auth.NewEntireAPICellClient(cellCtx, opts.insecureHTTP, &auth.CellTarget{
		Jurisdiction: cg.jurisdiction,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving cell client for %s: %w", cg.jurisdiction, err)
	}

	req := codesearch.SearchRequest{
		Query:         opts.query,
		CaseSensitive: opts.caseSensitive,
	}
	if opts.limit > 0 {
		// Divide limit across cells — same as BFF's perCellMax.
		req.MaxResults = opts.limit
	}

	resp, err := codesearch.Search(cellCtx, client, req)
	if err != nil {
		return nil, fmt.Errorf("code search on cell %s: %w", cg.cell, err)
	}
	return resp, nil
}

// codeSearchCellResult holds the outcome of a single-cell search.
type codeSearchCellResult struct {
	resp *codesearch.SearchResponse
	err  error
}

// mergeSearchResults merges responses from multiple cells into one, combining
// results, stats, and repo_stats. Cell errors are logged but don't fail the
// overall search — one bad cell never sinks the page.
func mergeSearchResults(ctx context.Context, cells []cellGroup, results []codeSearchCellResult) *codesearch.SearchResponse {
	merged := &codesearch.SearchResponse{}
	for i, r := range results {
		if r.err != nil {
			logging.Debug(ctx, "code search cell failed, skipping",
				"cell", cells[i].cell,
				"jurisdiction", cells[i].jurisdiction,
				"error", r.err.Error())
			continue
		}
		if r.resp == nil {
			continue
		}
		merged.Results = append(merged.Results, r.resp.Results...)
		merged.RepoStats = append(merged.RepoStats, r.resp.RepoStats...)
		merged.Stats.TotalMatches += r.resp.Stats.TotalMatches
		merged.Stats.TotalFiles += r.resp.Stats.TotalFiles
		merged.Stats.ReposSearched += r.resp.Stats.ReposSearched
		if r.resp.Stats.DurationMs > merged.Stats.DurationMs {
			merged.Stats.DurationMs = r.resp.Stats.DurationMs // wall-clock = slowest cell
		}
		if merged.Query == "" {
			merged.Query = r.resp.Query
		}
	}
	return merged
}

// writeCodeSearchJSON writes code search results as JSON.
func writeCodeSearchJSON(w io.Writer, resp *codesearch.SearchResponse) error {
	out := struct {
		Query     string                 `json:"query"`
		Results   []codesearch.Result    `json:"results"`
		Total     int                    `json:"total"`
		Stats     codesearch.Stats       `json:"stats"`
		RepoStats []codesearch.RepoStats `json:"repo_stats,omitempty"`
	}{
		Query:     resp.Query,
		Results:   resp.Results,
		Total:     resp.Stats.TotalMatches,
		Stats:     resp.Stats,
		RepoStats: resp.RepoStats,
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

// writeCodeSearchText renders code search results in grep-style format.
func writeCodeSearchText(w io.Writer, resp *codesearch.SearchResponse) {
	if len(resp.Results) == 0 {
		fmt.Fprintln(w, "No code search results found.")
		return
	}
	for _, r := range resp.Results {
		fmt.Fprintf(w, "%s:%s:%d: %s\n", r.Repo, r.Path, r.Line, r.ContextLine)
	}
	fmt.Fprintf(w, "\n%d matches across %d files in %d repos (%.0fms)\n",
		resp.Stats.TotalMatches, resp.Stats.TotalFiles, resp.Stats.ReposSearched, resp.Stats.DurationMs)
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
