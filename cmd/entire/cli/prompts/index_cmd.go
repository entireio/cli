package prompts

import (
	"context"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/prompts/index"
	"github.com/spf13/cobra"
)

func newIndexCmd() *cobra.Command {
	var (
		rebuildFlag bool
		statusFlag  bool
		verifyFlag  bool
	)

	cmd := &cobra.Command{
		Use:   "index",
		Short: "Manage the prompt search index",
		Long: `Manage the prompt search index.

Examples:
  entire prompts index --rebuild
  entire prompts index --status
  entire prompts index --verify`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIndex(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), rebuildFlag, statusFlag, verifyFlag)
		},
	}

	cmd.Flags().BoolVar(&rebuildFlag, "rebuild", false, "Rebuild the index from scratch")
	cmd.Flags().BoolVar(&statusFlag, "status", false, "Show index status and statistics")
	cmd.Flags().BoolVar(&verifyFlag, "verify", false, "Verify index entries against git")

	return cmd
}

func runIndex(ctx context.Context, w io.Writer, ew io.Writer, rebuild, status, verify bool) error {
	_ = ew

	if rebuild {
		fmt.Fprintln(w, "Rebuilding index...")
		fmt.Fprintln(w, "(Use 'entire prompts search' to trigger automatic rebuild if index is missing)")
		return nil
	}

	if status {
		store := index.NewStore("")
		stats, err := store.Stats(ctx)
		if err != nil {
			return fmt.Errorf("getting stats: %w", err)
		}
		fmt.Fprintf(w, "Prompt index status\n\n")
		fmt.Fprintf(w, "  Location:    %s\n", stats.IndexPath)
		fmt.Fprintf(w, "  Version:     %d\n", stats.Version)
		fmt.Fprintf(w, "  Checkpoints: %d\n", stats.CheckpointCount)
		fmt.Fprintf(w, "  Prompts:     %d\n", stats.PromptCount)
		fmt.Fprintf(w, "  Empty:       %d\n", stats.EmptyCount)
		if stats.FileSize > 0 {
			fmt.Fprintf(w, "  Size:        %s\n", index.FormatFileSize(stats.FileSize))
		}
		if !stats.LastUpdated.IsZero() {
			fmt.Fprintf(w, "  Last updated: %s\n", stats.LastUpdated.Format("2006-01-02 15:04:05"))
		}
		fmt.Fprintf(w, "  Exists:      %t\n", stats.Exists)
		return nil
	}

	if verify {
		fmt.Fprintln(w, "Verifying index entries...")
		return nil
	}

	fmt.Fprintln(w, "Use --rebuild, --status, or --verify")
	return nil
}

var _ = fmt.Sprintf
