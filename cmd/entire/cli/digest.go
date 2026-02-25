package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/digest"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newDigestCmd() *cobra.Command {
	var periodFlag string
	var authorFlag string
	var formatFlag string
	var aiCurateFlag bool
	var outputFlag string
	var limitFlag int
	var allPromptsFlag bool
	var shortFlag bool
	var noPagerFlag bool

	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Team activity digest from AI coding sessions",
		Long: `Generate a digest of your team's AI coding sessions.

Shows who's been working on what, notable prompts, and usage stats.
Data comes from the entire/checkpoints/v1 branch.

By default, uses Claude to classify the most interesting, funny, and
notable prompts into highlights. Falls back to heuristic scoring if
Claude is unavailable.

Examples:
  entire digest                     # Last 7 days, AI-curated highlights
  entire digest --period 30d        # Last 30 days
  entire digest --author myk        # Just one person's sessions
  entire digest --all-prompts       # Show all prompts (no limit)
  entire digest --limit 10          # Top 10 per author
  entire digest --short             # Stats only, no prompts
  entire digest --ai-curate=false   # Skip AI, use heuristic scoring only
  entire digest --format markdown   # Output as markdown`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkDisabledGuard(cmd.Context(), cmd.OutOrStdout()) {
				return nil
			}
			return runDigest(cmd.OutOrStdout(), cmd.ErrOrStderr(), digestFlags{
				period:     periodFlag,
				author:     authorFlag,
				format:     formatFlag,
				aiCurate:   aiCurateFlag,
				output:     outputFlag,
				limit:      limitFlag,
				allPrompts: allPromptsFlag,
				short:      shortFlag,
				noPager:    noPagerFlag,
			})
		},
	}

	cmd.Flags().StringVar(&periodFlag, "period", "7d", "Time period: today, 7d, 30d, all")
	cmd.Flags().StringVar(&authorFlag, "author", "", "Filter to a specific author (name or prefix)")
	cmd.Flags().StringVar(&formatFlag, "format", "terminal", "Output format: terminal, markdown, json")
	cmd.Flags().BoolVar(&aiCurateFlag, "ai-curate", true, "Use Claude to classify prompt highlights (disable with --ai-curate=false)")
	cmd.Flags().StringVar(&outputFlag, "output", "", "Write output to file instead of stdout")
	cmd.Flags().IntVar(&limitFlag, "limit", 0, "Max prompts per author (default 5)")
	cmd.Flags().BoolVar(&allPromptsFlag, "all-prompts", false, "Show all prompts (no limit)")
	cmd.Flags().BoolVarP(&shortFlag, "short", "s", false, "Show stats only (no prompts)")
	cmd.Flags().BoolVar(&noPagerFlag, "no-pager", false, "Disable automatic pager")

	return cmd
}

type digestFlags struct {
	period     string
	author     string
	format     string
	aiCurate   bool
	output     string
	limit      int
	allPrompts bool
	short      bool
	noPager    bool
}

func runDigest(w, errW io.Writer, flags digestFlags) error {
	ctx := context.Background()
	repo, err := openRepository(ctx)
	if err != nil {
		fmt.Fprintln(errW, "Not a git repository. Run 'entire digest' from within a git repository.")
		return NewSilentError(err)
	}

	store := checkpoint.NewGitStore(repo)

	d, err := digest.Generate(ctx, store, digest.Options{
		Period:       flags.period,
		AuthorFilter: flags.author,
		Limit:        flags.limit,
		AllPrompts:   flags.allPrompts,
	})
	if err != nil {
		return fmt.Errorf("failed to generate digest: %w", err)
	}

	if d.Total.Checkpoints == 0 {
		fmt.Fprintf(w, "No checkpoints found in the last %s.\n", flags.period)
		fmt.Fprintln(w, "Work with your AI agent and commit to create checkpoints.")
		return nil
	}

	// AI curation
	var highlights *digest.Highlights
	if flags.aiCurate {
		classifier := &digest.Classifier{}
		highlights, err = classifier.Classify(ctx, d)
		if err != nil {
			fmt.Fprintf(errW, "Warning: AI curation unavailable (%v), showing stats only\n", err)
		}
	}

	// Determine output writer
	out := w
	if flags.output != "" {
		f, fileErr := os.Create(flags.output)
		if fileErr != nil {
			return fmt.Errorf("failed to create output file: %w", fileErr)
		}
		defer f.Close()
		out = f
	}

	// Build output string
	var sb strings.Builder
	formatOpts := digest.FormatOptions{Short: flags.short}

	switch flags.format {
	case "json":
		digest.FormatJSON(&sb, d)
	case "markdown":
		digest.FormatMarkdown(&sb, d, highlights, formatOpts)
	case "terminal":
		digest.FormatTerminal(&sb, d, highlights, formatOpts)
	default:
		return fmt.Errorf("unknown format %q: use \"terminal\", \"markdown\", or \"json\"", flags.format)
	}

	content := sb.String()

	// Output with pager if appropriate
	if flags.output == "" && !flags.noPager {
		outputDigestWithPager(out, content)
	} else {
		fmt.Fprint(out, content)
	}

	if flags.output != "" {
		fmt.Fprintf(w, "Digest written to %s\n", flags.output)
	}

	return nil
}

// outputDigestWithPager pipes content through a pager if stdout is a terminal and content is long.
func outputDigestWithPager(w io.Writer, content string) {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(w, content)
		return
	}

	_, height, err := term.GetSize(int(f.Fd()))
	if err != nil {
		height = 24
	}

	lineCount := strings.Count(content, "\n")
	if lineCount <= height-2 {
		fmt.Fprint(w, content)
		return
	}

	pager := os.Getenv("PAGER")
	if pager == "" {
		pager = "less"
	}

	cmd := exec.CommandContext(context.Background(), pager) //nolint:gosec
	cmd.Stdin = strings.NewReader(content)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprint(w, content)
	}
}
