package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

type runnerSetupOptions struct {
	runner       string // optional: limit to one runner (id, with or without "trail-")
	run          bool   // headless apply vs. print prompt
	assumeYes    bool   // skip the create-defaults confirmation
	debugDir     string // if set, dump prompt.txt (+ response.txt on --run) here
	sources      []string
	limit        int
	insecureHTTP bool
}

func newRunnerSetupCmd() *cobra.Command {
	var (
		run       bool
		assumeYes bool
		debugDir  string
		sources   []string
		limit     int
	)

	cmd := &cobra.Command{
		Use:   "setup [<runner>]",
		Short: "Create and tailor this repository's trail runners",
		Long: `Set up the .entire/runners/*.json evaluators for this repository.

Runners (risk, confidence, drift, security, review, …) score and review a
branch's changes. The shipped templates are generic; "setup" tailors them to
THIS repo using gathered signal — its docs and structure, merged PRs and
issues, checkpoint churn hotspots, and past trail findings.

- In a repo with no runners, setup creates the default set first (use --yes to
  skip the confirmation), then tailors them.
- Run again in a repo that already has runners and setup offers to re-tune them.

By default setup prints the tailoring prompt to stdout, ready to paste into
your agent. With --run it executes the prompt headlessly through your
configured summary provider and rewrites the runner files in place (review with
git diff).

If <runner> is given (e.g. "risk" or "trail-risk"), only that runner is tuned.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runner := ""
			if len(args) == 1 {
				runner = args[0]
			}
			return runRunnerSetup(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), runnerSetupOptions{
				runner:       runner,
				run:          run,
				assumeYes:    assumeYes,
				debugDir:     debugDir,
				sources:      sources,
				limit:        limit,
				insecureHTTP: runnerInsecureHTTP(cmd),
			})
		},
	}

	cmd.Flags().BoolVar(&run, "run", false,
		"Run the configured summary provider headlessly to rewrite the runner files in place (default: print the prompt)")
	cmd.Flags().StringSliceVar(&sources, "sources", nil,
		"Comma-separated data sources to gather: repo, prs, checkpoints, trails, all (default: all)")
	cmd.Flags().IntVar(&limit, "limit", 20, "How many recent PRs/issues/trails to sample")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false,
		"Skip the confirmation when creating the default runner set in a repo that has none")
	cmd.Flags().StringVar(&debugDir, "debug-dir", "",
		"Write the assembled prompt (prompt.txt) and, with --run, the raw model response (response.txt) to this directory for debugging")

	return cmd
}

func runRunnerSetup(ctx context.Context, w, errW io.Writer, opts runnerSetupOptions) error {
	src, err := parseTuneSources(opts.sources)
	if err != nil {
		return err
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	// A repo with no runners gets the default set scaffolded (on confirmation),
	// which setup then tailors below.
	created, err := ensureRunnersPresent(w, errW, repoRoot, opts.assumeYes)
	if err != nil {
		return err
	}

	// Re-run on an already-configured repo: setup is done, so offer to re-tune
	// rather than silently re-emitting. --run is taken as an explicit yes.
	if len(created) == 0 && !opts.run {
		if !interactive.CanPromptInteractively() {
			fmt.Fprintln(errW, "Runners already configured. Re-run with --run to tailor them headlessly.")
			return nil
		}
		proceed, err := confirmTuneExisting()
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Fprintln(errW, "Runners already configured. Nothing to do.")
			return nil
		}
	}

	runners, err := loadTuneRunners(repoRoot, opts.runner)
	if err != nil {
		return err
	}

	stopGather := startSpinner(errW, "Gathering repository signal")
	brief := gatherTuningContext(ctx, errW, repoRoot, src, opts.limit, opts.insecureHTTP)
	stopGather(true)
	prompt := buildTunePrompt(brief, runners)

	if opts.debugDir != "" {
		writeTuneDebug(errW, opts.debugDir, "prompt.txt", prompt)
	}

	if !opts.run {
		fmt.Fprintln(w, prompt)
		if len(created) > 0 {
			fmt.Fprintf(errW, "\nCreated %d working default runner(s) (untracked). They are functional as-is; paste the prompt above into your agent to tailor them to this repo.\n", len(created))
		}
		fmt.Fprintf(errW, "\n%d runner(s) in scope. Paste the prompt above into your agent, or re-run with --run to apply headlessly.\n", len(runners))
		return nil
	}

	return applyTuneWithAgent(ctx, w, errW, runners, prompt, created, opts.debugDir)
}

// writeTuneDebug best-effort writes content to <dir>/<name> for debugging,
// reporting any failure as a warning rather than failing the command.
func writeTuneDebug(errW io.Writer, dir, name, content string) {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // user-specified debug dir
		fmt.Fprintf(errW, "warning: debug dir %s: %v\n", dir, err)
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // local debug artifact
		fmt.Fprintf(errW, "warning: writing %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(errW, "debug: wrote %s\n", path)
}

// applyTuneWithAgent runs the prompt through the configured summary provider
// (prompt -> text), parses the runner-id -> template map it returns, and
// surgically rewrites each runner file's prompt.template in place. createdIDs
// are runners onboarding just scaffolded from defaults; any of those left
// un-tailored is flagged so it isn't committed as if it were repo-specific.
func applyTuneWithAgent(ctx context.Context, w, errW io.Writer, runners []tuneRunner, prompt string, createdIDs []string, debugDir string) error {
	// Reuse the summary-provider resolution (selection + persistence), but pull
	// the raw TextGenerator rather than provider.Generator: the latter is a
	// summarize.Generator that turns a transcript Input into a checkpoint
	// Summary, whereas we need plain prompt->text generation here.
	provider, err := resolveCheckpointSummaryProvider(ctx, errW)
	if err != nil {
		return err
	}
	ag, err := agent.Get(provider.Name)
	if err != nil {
		return fmt.Errorf("loading provider %s: %w", provider.Name, err)
	}
	textGen, ok := agent.AsTextGenerator(ag)
	if !ok {
		return fmt.Errorf("provider %s cannot generate text", provider.Name)
	}

	stop := startSpinner(errW, fmt.Sprintf("Tuning %d runner(s) with %s", len(runners), provider.DisplayName))
	out, err := textGen.GenerateText(ctx, prompt, provider.Model)
	stop(err == nil)
	if err != nil {
		return fmt.Errorf("agent run failed: %w", err)
	}
	if debugDir != "" {
		writeTuneDebug(errW, debugDir, "response.txt", out)
	}

	templates, err := parseTuneOutput(out)
	if err != nil {
		return err
	}

	byID := make(map[string]tuneRunner, len(runners))
	for _, r := range runners {
		byID[normalizeRunnerID(r.ID)] = r
	}

	updated, skipped := 0, 0
	tailored := make(map[string]bool)
	for id, tmpl := range templates {
		r, ok := byID[normalizeRunnerID(id)]
		if !ok {
			fmt.Fprintf(errW, "skip %q: not a runner in scope\n", id)
			skipped++
			continue
		}
		if err := validateNewTemplate(r.Template, tmpl); err != nil {
			fmt.Fprintf(errW, "skip %s: %v\n", r.ID, err)
			skipped++
			continue
		}
		if dropped := droppedPlaceholders(r.Template, tmpl); len(dropped) > 0 {
			fmt.Fprintf(errW, "note: %s no longer references %v\n", r.ID, dropped)
		}
		newRaw, err := replaceRunnerTemplate(r.Raw, tmpl)
		if err != nil {
			fmt.Fprintf(errW, "skip %s: %v\n", r.ID, err)
			skipped++
			continue
		}
		if bytes.Equal(newRaw, r.Raw) {
			continue // model returned the current template verbatim — benign no-op
		}
		if err := os.WriteFile(r.Path, newRaw, 0o644); err != nil { //nolint:gosec // runner configs are repo-committed, world-readable config
			return fmt.Errorf("writing %s: %w", r.Path, err)
		}
		fmt.Fprintf(w, "updated %s\n", filepath.Base(r.Path))
		tailored[normalizeRunnerID(r.ID)] = true
		updated++
	}

	switch {
	case updated > 0:
		fmt.Fprintf(w, "\nUpdated %d runner(s). Review with: git diff .entire/runners\n", updated)
	case len(createdIDs) == 0 && skipped > 0:
		// Existing runners, model proposed templates, all rejected — a failed run.
		// (When onboarding just created the set, an un-tailored runner is reported
		// below as a generic default instead, which is more actionable.)
		return fmt.Errorf("model proposed %d template(s) but all were rejected or out of scope (see messages above)", skipped)
	case len(createdIDs) == 0:
		fmt.Fprintln(w, "No runner changes proposed.")
	}

	// Runners onboarding scaffolded but tuning didn't tailor remain the generic
	// defaults. Those are working minimal prompts (valid output contract), so
	// they're committable as-is — just note which are still generic.
	if untailored := untailoredRunners(createdIDs, tailored); len(untailored) > 0 {
		fmt.Fprintf(errW, "\n%d runner(s) kept as working defaults (generic, not tailored to this repo): %s\n",
			len(untailored), strings.Join(untailored, ", "))
		fmt.Fprintln(errW, "They are functional as-is; re-run `entire runner setup --run` to tailor them.")
	}
	return nil
}
