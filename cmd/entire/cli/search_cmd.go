package cli

import (
	"errors"
	"fmt"
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
		authorFlag string
		dateFlag   string
	)

	cmd := &cobra.Command{
		Use:    "search [query]",
		Short:  "Search checkpoints using semantic and keyword matching",
		Hidden: true,
		Long: `Search checkpoints using hybrid search (semantic + keyword),
powered by the Entire search service.

Requires authentication via 'entire login' (GitHub device flow).

Run without arguments to open an interactive search. Results are
displayed in an interactive table. Use --json for machine-readable output.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.Join(args, " ")

			// Extract inline filters (author:, date:) from query args
			parsed := search.ParseSearchInput(query)
			query = parsed.Query
			if authorFlag == "" {
				authorFlag = parsed.Author
			}
			if dateFlag == "" {
				dateFlag = parsed.Date
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
				Query:       query,
				Limit:       limitFlag,
				Author:      authorFlag,
				Date:        dateFlag,
			}

			w := cmd.OutOrStdout()
			isTerminal := isTerminalWriter(w)

			// No query and no filters + non-interactive = error
			if query == "" && !searchCfg.HasFilters() && (jsonOutput || !isTerminal || IsAccessibleMode()) {
				return errors.New("query required when using --json, accessible mode, or piped output. Usage: entire search <query>")
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

			// Fetch max results for TUI so client-side pagination works.
			// The search API uses limit to cap total results fetched, so
			// server-side page param alone is insufficient for pagination.
			willUseTUI := !jsonOutput && isTerminal && !IsAccessibleMode()
			if willUseTUI {
				searchCfg.Limit = search.MaxLimit
			}

			resp, err := search.Search(ctx, searchCfg)
			if err != nil {
				return fmt.Errorf("search failed: %w", err)
			}

			// JSON output: explicit flag or piped/redirected stdout
			if jsonOutput || !isTerminal {
				if len(resp.Results) == 0 {
					fmt.Fprintln(w, "[]")
					return nil
				}
				data, err := jsonutil.MarshalIndentWithNewline(resp.Results, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling results: %w", err)
				}
				fmt.Fprint(w, string(data))
				return nil
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
	cmd.Flags().IntVar(&limitFlag, "limit", 20, "Maximum number of results")
	cmd.Flags().StringVar(&authorFlag, "author", "", "Filter by author name")
	cmd.Flags().StringVar(&dateFlag, "date", "", "Filter by time period (week or month)")

	return cmd
}
