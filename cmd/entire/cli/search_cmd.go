package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newSearchCmd() *cobra.Command {
	var (
		jsonOutput bool
		limitFlag  int
		pageFlag   int
		authorFlag string
		dateFlag   string
		branchFlag string
		repoFlag   string
	)

	cmd := &cobra.Command{
		Use:    "search [query]",
		Short:  "Search checkpoints using semantic and keyword matching",
		Hidden: true,
		Long: `Search checkpoints using hybrid search (semantic + keyword),
powered by the Entire search service.

Requires authentication via 'entire login' (GitHub device flow).

Run without arguments to open an interactive search. Results are
displayed in an interactive table. Use --json for machine-readable output.

CLI queries also support inline filters like author:<name>, date:<week|month>,
branch:<name>, repo:<owner/name>, and repo:* to search all accessible repos.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.Join(args, " ")

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

			w := cmd.OutOrStdout()
			isTerminal := isTerminalWriter(w)
			hasFilters := authorFlag != "" || dateFlag != "" || branchFlag != "" || len(repos) > 0

			// Fast-fail: no query + non-interactive mode = error (before auth/git checks)
			if query == "" && !hasFilters && (jsonOutput || !isTerminal || IsAccessibleMode()) {
				return errors.New("query required when using --json, accessible mode, or piped output. Usage: entire search <query>")
			}

			ghToken, err := auth.LookupCurrentToken()
			if err != nil {
				return fmt.Errorf("reading credentials: %w", err)
			}
			if ghToken == "" {
				return errors.New("not authenticated. Run 'entire login' to authenticate")
			}

			// Get the repo's GitHub remote URL
			repo, err := strategy.OpenRepository(ctx)
			if err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run this command from within a git repository.")
				return NewSilentError(err)
			}

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
				serviceURL = search.DefaultServiceURL
			}

			searchCfg := search.Config{
				ServiceURL:  serviceURL,
				GitHubToken: ghToken,
				Owner:       owner,
				Repo:        repoName,
				Repos:       repos,
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
				searchCfg.Limit = search.MaxLimit
				styles := newStatusStyles(w)
				model := newSearchModel(nil, "", 0, searchCfg, styles)
				model.mode = modeSearch
				model.input.Focus()
				p := tea.NewProgram(model, tea.WithAltScreen())
				if _, err := p.Run(); err != nil {
					return fmt.Errorf("TUI error: %w", err)
				}
				return nil
			}

			// Fetch max results so client-side pagination works.
			// The search API caps results at the limit, so we fetch
			// the maximum and paginate client-side for all output modes.
			requestedLimit := searchCfg.Limit
			requestedPage := searchCfg.Page
			searchCfg.Limit = search.MaxLimit
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
			p := tea.NewProgram(model, tea.WithAltScreen())
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	cmd.Flags().IntVar(&limitFlag, "limit", resultsPerPage, "Maximum number of results per page")
	cmd.Flags().IntVar(&pageFlag, "page", 1, "Page number (1-based)")
	cmd.Flags().StringVar(&authorFlag, "author", "", "Filter by author name")
	cmd.Flags().StringVar(&dateFlag, "date", "", "Filter by time period (week or month)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Filter by branch name")
	cmd.Flags().StringVar(&repoFlag, "repo", "", "Filter by repository (owner/name or *)")

	return cmd
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
		Results    []search.Result `json:"results"`
		Total      int             `json:"total"`
		Page       int             `json:"page"`
		TotalPages int             `json:"total_pages"`
		Limit      int             `json:"limit"`
	}{
		Results:    pageResults,
		Total:      total,
		Page:       page,
		TotalPages: totalPages,
		Limit:      limit,
	}
	data, err := jsonutil.MarshalIndentWithNewline(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}
	fmt.Fprint(w, string(data))
	return nil
}
