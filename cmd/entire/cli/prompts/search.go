package prompts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/prompts/index"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

func newSearchCmd() *cobra.Command {
	var (
		limitFlag  int
		jsonFlag   bool
		agentFlag  string
		branchFlag string
		kindFlag   string
		afterFlag  string
		filesFlag  string
	)

	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search prompts from checkpoint history",
		Long: `Search prompts from your checkpoint history by keywords.

Examples:
  entire prompts search "cache decision"
  entire prompts search --limit 50 --agent claude
  entire prompts search --json --branch main`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), strings.Join(args, " "), index.SearchConfig{
				Limit:  limitFlag,
				JSON:   jsonFlag,
				Agent:  agentFlag,
				Branch: branchFlag,
				Kind:   kindFlag,
				After:  afterFlag,
				Files:  filesFlag,
			})
		},
	}

	cmd.Flags().IntVar(&limitFlag, "limit", 20, "Maximum number of results")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&agentFlag, "agent", "", "Filter by agent")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "Filter by branch")
	cmd.Flags().StringVar(&kindFlag, "kind", "", "Filter by kind (session or agent_review)")
	cmd.Flags().StringVar(&afterFlag, "after", "", "Filter by date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&filesFlag, "files", "", "Filter by files touched")

	return cmd
}

func runSearch(ctx context.Context, w io.Writer, ew io.Writer, query string, cfg index.SearchConfig) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return errors.New("not a git repository")
	}

	if len(strings.TrimSpace(query)) < 2 {
		return errors.New("query too short — enter at least one word")
	}

	store := index.NewStore(repoRoot)

	if !store.Exists() {
		fmt.Fprintln(ew, "No prompt index found. Running automatic rebuild...")
		if err := rebuildIndex(ctx, ew, repoRoot); err != nil {
			return fmt.Errorf("rebuilding index: %w", err)
		}
	}

	entries, err := store.Load(ctx)
	if err != nil {
		if errors.Is(err, index.ErrIndexMissing) || errors.Is(err, index.ErrIndexCorrupt) {
			fmt.Fprintln(ew, "Prompt index is corrupt or missing. Running rebuild...")
			if err := rebuildIndex(ctx, ew, repoRoot); err != nil {
				return fmt.Errorf("rebuilding index: %w", err)
			}
			entries, err = store.Load(ctx)
		}
		if err != nil {
			return fmt.Errorf("loading index: %w", err)
		}
	}

	cfg.Query = query
	results := index.Search(entries, cfg)

	if len(results) == 0 {
		fmt.Fprintf(w, "No results for %q.\n", query)
		return nil
	}

	if cfg.JSON && !isStdoutTTY() {
		fmt.Fprintln(ew, "Warning: --json output includes full prompt text. Ensure this is not captured in logs.")
	}

	if cfg.JSON {
		return writeJSONResults(w, results, query)
	}

	return writeTTYResults(w, results, query)
}

func isStdoutTTY() bool {
	fi, _ := os.Stdout.Stat()
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func rebuildIndex(ctx context.Context, w io.Writer, repoRoot string) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return fmt.Errorf("opening repository: %w", err)
	}

	store := index.NewStore(repoRoot)
	builder := index.NewBuilder(repo, store)

	fmt.Fprintln(w, "Building prompt index...")

	progressFn := func(done, total int) {
		if total > 0 {
			fmt.Fprintf(w, "\r  %d / %d", done, total)
		}
	}

	if err := builder.Build(ctx, w, progressFn); err != nil {
		return fmt.Errorf("building index: %w", err)
	}

	fmt.Fprintln(w, "")
	return nil
}

func writeTTYResults(w io.Writer, results []index.ScoredEntry, query string) error {
	fmt.Fprintf(w, "\nSearch results for %q  (%d found)\n\n", query, len(results))

	for _, r := range results {
		truncatedNote := ""
		if r.TruncatedMatch {
			truncatedNote = " (truncated)"
		}

		prompt := r.Entry.PromptText
		if len(prompt) > 70 {
			prompt = prompt[:70] + "..."
		}

		fmt.Fprintf(w, "  %s  %s  %s  %s\n",
			r.Entry.CheckpointID,
			r.Entry.CreatedAt.Format("2006-01-02"),
			r.Entry.Agent,
			r.Entry.Branch,
		)
		fmt.Fprintf(w, "  %q%s\n\n", prompt, truncatedNote)
	}

	return nil
}

func writeJSONResults(w io.Writer, results []index.ScoredEntry, query string) error {
	type JSONResult struct {
		CheckpointID    string   `json:"checkpoint_id"`
		SessionIndex    int      `json:"session_index"`
		TurnIndex       int      `json:"turn_index"`
		Kind            string   `json:"kind"`
		Prompt          string   `json:"prompt"`
		PromptTruncated bool     `json:"prompt_truncated"`
		CommitHash      string   `json:"commit_hash"`
		CommitMessage   string   `json:"commit_message"`
		Branch          string   `json:"branch"`
		Agent           string   `json:"agent"`
		Model           string   `json:"model"`
		FilesTouched    []string `json:"files_touched"`
		CreatedAt       string   `json:"created_at"`
		Score           float64  `json:"score"`
	}

	output := struct {
		Query   string       `json:"query"`
		Total   int          `json:"total"`
		Results []JSONResult `json:"results"`
	}{
		Query:   query,
		Total:   len(results),
		Results: make([]JSONResult, len(results)),
	}

	for i, r := range results {
		output.Results[i] = JSONResult{
			CheckpointID:    r.Entry.CheckpointID,
			SessionIndex:    r.Entry.SessionIndex,
			TurnIndex:       r.Entry.TurnIndex,
			Kind:            r.Entry.Kind,
			Prompt:          r.Entry.PromptText,
			PromptTruncated: r.Entry.PromptTruncated,
			CommitHash:      r.Entry.CommitHash,
			CommitMessage:   r.Entry.CommitMessage,
			Branch:          r.Entry.Branch,
			Agent:           r.Entry.Agent,
			Model:           r.Entry.Model,
			FilesTouched:    r.Entry.FilesTouched,
			CreatedAt:       r.Entry.CreatedAt.Format("2006-01-02T15:04:05Z"),
			Score:           r.Score,
		}
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling JSON: %w", err)
	}
	n, err := w.Write(data)
	if err != nil {
		return fmt.Errorf("writing JSON: %w", err)
	}
	if n != len(data) {
		return errors.New("incomplete write")
	}
	return nil
}
