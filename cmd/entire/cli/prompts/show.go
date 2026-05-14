package prompts

import (
	"context"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/prompts/index"
	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <checkpoint-id>",
		Short: "Show the prompt for a checkpoint",
		Long: `Show the full prompt text for a specific checkpoint.

Examples:
  entire prompts show a3b2c4d5e6f7
  entire prompts show abc123`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}

	return cmd
}

func runShow(ctx context.Context, w io.Writer, cpIDPrefix string) error {
	store := index.NewStore("")
	entries, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("loading index: %w", err)
	}

	prefix := index.ParseCheckpointIDPrefix(cpIDPrefix)
	if prefix == "" {
		return fmt.Errorf("invalid checkpoint ID: %s", cpIDPrefix)
	}

	exactMatches := make(map[string][]index.Entry)
	prefixMatches := make([]index.Entry, 0)

	for _, entry := range entries {
		if entry.CheckpointID == prefix {
			exactMatches[entry.CheckpointID] = append(exactMatches[entry.CheckpointID], entry)
		} else if len(entry.CheckpointID) >= len(prefix) && entry.CheckpointID[:len(prefix)] == prefix {
			prefixMatches = append(prefixMatches, entry)
		}
	}

	if len(exactMatches) > 0 {
		for cpID, matches := range exactMatches {
			entry := matches[0]
			truncatedNote := ""
			if entry.PromptTruncated {
				truncatedNote = truncatedNoteSuffix
			}
			fmt.Fprintf(w, "Checkpoint:   %s\n", entry.CheckpointID)
			fmt.Fprintf(w, "Commit:       %s — %s\n", entry.CommitHash, entry.CommitMessage)
			fmt.Fprintf(w, "Branch:       %s\n", entry.Branch)
			fmt.Fprintf(w, "Agent:        %s\n", entry.Agent)
			fmt.Fprintf(w, "Model:        %s\n", entry.Model)
			fmt.Fprintf(w, "Created:      %s\n", entry.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Fprintf(w, "Kind:         %s\n", entry.Kind)
			if len(matches) > 1 {
				fmt.Fprintf(w, "Sessions:     %d\n\n", len(matches))
			} else {
				fmt.Fprintf(w, "Session:      %d of %d\n\n", entry.SessionIndex+1, entry.SessionIndex+1)
			}
			fmt.Fprintf(w, "Prompt (turn %d of %d):%s\n", entry.TurnIndex+1, entry.TurnIndex+1, truncatedNote)
			fmt.Fprintln(w, "─────────────────────────────────────────────────────────────")
			fmt.Fprintf(w, "%s\n", entry.PromptText)
			fmt.Fprintln(w, "─────────────────────────────────────────────────────────────")

			if len(entry.FilesTouched) > 0 {
				fmt.Fprintln(w, "Files touched:")
				for _, f := range entry.FilesTouched {
					fmt.Fprintf(w, "  %s\n", f)
				}
			}
			fmt.Fprintf(w, "\nRun: entire checkpoint explain %s\n", cpID)
			fmt.Fprintf(w, "Run: entire checkpoint rewind --to %s\n", cpID)
		}
		return nil
	}

	if len(prefixMatches) == 0 {
		return fmt.Errorf("checkpoint not found: %s", cpIDPrefix)
	}

	seenCP := make(map[string]bool)
	uniqueMatches := make([]index.Entry, 0)
	for _, e := range prefixMatches {
		if !seenCP[e.CheckpointID] {
			seenCP[e.CheckpointID] = true
			uniqueMatches = append(uniqueMatches, e)
		}
	}

	if len(uniqueMatches) == 1 {
		entry := uniqueMatches[0]
		truncatedNote := ""
		if entry.PromptTruncated {
			truncatedNote = truncatedNoteSuffix
		}
		fmt.Fprintf(w, "Checkpoint:   %s\n", entry.CheckpointID)
		fmt.Fprintf(w, "Commit:       %s — %s\n", entry.CommitHash, entry.CommitMessage)
		fmt.Fprintf(w, "Branch:       %s\n", entry.Branch)
		fmt.Fprintf(w, "Agent:        %s\n", entry.Agent)
		fmt.Fprintf(w, "Model:        %s\n", entry.Model)
		fmt.Fprintf(w, "Created:      %s\n", entry.CreatedAt.Format("2006-01-02 15:04:05"))
		fmt.Fprintf(w, "Kind:         %s\n", entry.Kind)
		fmt.Fprintf(w, "Session:      %d of %d\n\n", entry.SessionIndex+1, entry.SessionIndex+1)
		fmt.Fprintf(w, "Prompt (turn %d of %d):%s\n", entry.TurnIndex+1, entry.TurnIndex+1, truncatedNote)
		fmt.Fprintln(w, "─────────────────────────────────────────────────────────────")
		fmt.Fprintf(w, "%s\n", entry.PromptText)
		fmt.Fprintln(w, "─────────────────────────────────────────────────────────────")

		if len(entry.FilesTouched) > 0 {
			fmt.Fprintln(w, "Files touched:")
			for _, f := range entry.FilesTouched {
				fmt.Fprintf(w, "  %s\n", f)
			}
		}
		fmt.Fprintf(w, "\nRun: entire checkpoint explain %s\n", entry.CheckpointID)
		fmt.Fprintf(w, "Run: entire checkpoint rewind --to %s\n", entry.CheckpointID)
		return nil
	}

	fmt.Fprintf(w, "Ambiguous prefix %q. Did you mean:\n\n", cpIDPrefix)
	for _, entry := range uniqueMatches {
		fmt.Fprintf(w, "  %s  %s  %s  %s\n",
			entry.CheckpointID,
			entry.CreatedAt.Format("2006-01-02"),
			entry.Agent,
			entry.Branch,
		)
	}

	return nil
}
