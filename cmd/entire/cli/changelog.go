package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/changelog"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	"github.com/spf13/cobra"
)

func newChangelogCmd() *cobra.Command {
	return newChangelogCmdWithClient(nil)
}

func newChangelogCmdWithClient(client *changelog.Client) *cobra.Command {
	var limit int
	var asJSON bool
	var onlyCLI bool
	run := func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		if limit <= 0 {
			return errors.New("--limit must be positive")
		}
		query := ""
		if len(args) > 0 {
			query = args[0]
		}
		if client == nil {
			var err error
			client, err = changelog.NewClient(nil, "")
			if err != nil {
				return fmt.Errorf("create changelog client: %w", err)
			}
		}
		stop := func(bool) {}
		if !asJSON && !IsAccessibleMode() && interactive.ShouldStyle(cmd.OutOrStdout()) {
			message := "Loading changelog"
			if query != "" {
				message = "Searching changelog"
			}
			stop = startSpinner(cmd.ErrOrStderr(), message)
		}
		entries, err := client.Read(cmd.Context(), limit, query, onlyCLI)
		stop(false)
		if err != nil {
			return fmt.Errorf("read product changelog: %w", err)
		}
		return writeChangelog(cmd.OutOrStdout(), entries, asJSON)
	}
	cmd := &cobra.Command{
		Use:     "changelog",
		Short:   "Read Entire product updates",
		Long:    "Read full product announcements from entire.io and CLI releases from GitHub, newest first.\nNo login, Git repository, or Entire setup is required.",
		Example: "  entire changelog\n  entire changelog --limit 10 --json\n  entire changelog search \"git network\"",
		Args:    cobra.NoArgs,
		RunE:    run,
	}
	cmd.PersistentFlags().IntVar(&limit, "limit", 5, "Maximum number of entries (positive integer)")
	cmd.PersistentFlags().BoolVar(&onlyCLI, "only-cli", false, "Show only CLI releases")
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "Output full entries as a JSON array")
	cmd.AddCommand(&cobra.Command{
		Use:     "search <query>",
		Short:   "Search the complete product changelog",
		Long:    "Find a case-insensitive literal phrase in titles, descriptions, or full Markdown bodies.\nResults are newest first. Search covers product posts and CLI releases. A no-match search may fetch every published changelog post.",
		Example: "  entire changelog search subagent\n  entire changelog search \"git network\" --limit 10 --json",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(cmd, args); err != nil {
				return fmt.Errorf("changelog search: %w", err)
			}
			if strings.TrimSpace(args[0]) == "" {
				return errors.New("search query must not be blank")
			}
			return nil
		},
		RunE: run,
	})
	return cmd
}

func writeChangelog(w io.Writer, entries []changelog.Entry, asJSON bool) error {
	if asJSON {
		if entries == nil {
			entries = []changelog.Entry{}
		}
		if err := json.NewEncoder(w).Encode(entries); err != nil {
			return fmt.Errorf("write changelog JSON: %w", err)
		}
		return nil
	}
	var markdown strings.Builder
	if len(entries) == 0 {
		markdown.WriteString("No changelog entries found.\n")
	}
	for i, entry := range entries {
		if i > 0 {
			markdown.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&markdown, "# %s\n\n%s · %s\n\n%s\n", entry.Title, entry.Date, entry.URL, entry.Content)
	}
	output := markdown.String()
	if !IsAccessibleMode() {
		// Source Markdown remains useful if terminal rendering fails.
		if rendered, err := mdrender.RenderForWriter(w, output); err == nil {
			output = rendered
		}
	}
	if _, err := io.WriteString(w, output); err != nil {
		return fmt.Errorf("write changelog: %w", err)
	}
	return nil
}

func isChangelogCommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "changelog" && c.Parent() != nil && c.Parent().Parent() == nil {
			return true
		}
	}
	return false
}
