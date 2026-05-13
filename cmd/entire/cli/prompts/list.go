package prompts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/prompts/index"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var limitFlag int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent prompts",
		Long: `List recent prompts from checkpoint history, newest first.

Examples:
  entire prompts list
  entire prompts list --limit 50`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), limitFlag)
		},
	}

	cmd.Flags().IntVar(&limitFlag, "limit", 20, "Maximum number of prompts to show")
	return cmd
}

func runList(ctx context.Context, w io.Writer, _ io.Writer, limit int) error {
	store := index.NewIndexStore("")

	if !store.Exists() {
		fmt.Fprintln(w, "No prompt index found. Run 'entire prompts index --rebuild' first.")
		return nil
	}

	_, entries, err := store.Load(ctx)
	if err != nil {
		if errors.Is(err, index.ErrIndexMissing) || errors.Is(err, index.ErrIndexEmpty) {
			fmt.Fprintln(w, "Prompt index is empty.")
			return nil
		}
		return fmt.Errorf("loading index: %w", err)
	}

	if len(entries) == 0 {
		fmt.Fprintln(w, "No prompts found.")
		return nil
	}

	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	fmt.Fprintf(w, "Recent prompts (%d shown, %d total)\n\n", len(entries), len(entries))

	for _, entry := range entries {
		truncated := ""
		if entry.PromptTruncated {
			truncated = " (truncated)"
		}
		prompt := entry.PromptText
		if len(prompt) > 60 {
			prompt = prompt[:60] + "..."
		}
		fmt.Fprintf(w, "  %s  %s  %s  %s\n",
			entry.CheckpointID,
			entry.CreatedAt.Format("2006-01-02"),
			entry.Agent,
			entry.Branch,
		)
		fmt.Fprintf(w, "  %q%s\n\n", prompt, truncated)
	}

	return nil
}

var _ = strings.TrimSpace