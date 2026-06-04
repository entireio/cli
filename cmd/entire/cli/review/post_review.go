// Package review — see env.go for package-level rationale.
//
// post_review.go owns the end-of-run UX: the inline `--prompt` ask, the
// post-review fix prompt ([Y]es / [s]elect / [n]o / [A]lways), and the
// findings-footer printed when the user declines or stdin is not
// promptable.
package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agentlaunch"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// stagePerRunContext renders the pre-launch staging view — the scope banner
// plus the checkpoints/sessions in scope — and collects the optional per-run
// prompt, all before any agent is spawned.
//
// Interactive (and no --prompt): a single styled huh form — the optional
// context input on top, the "what's being reviewed" summary as a Note
// underneath (scope line as the note title, the checkpoints/sessions block as
// its body). The user sees everything in scope while deciding what context to
// add, and the form holds the screen until they answer rather than flashing by
// before the live TUI.
//
// --prompt supplied, or non-interactive (CI / agent host): the same summary is
// printed plainly so it's still visible, and no prompt is asked.
//
// Returns the per-run prompt (the supplied --prompt, the typed value, or "").
func stagePerRunContext(ctx context.Context, out io.Writer, scopeBanner string, ctxResult ContextResult, perRunPrompt string) string {
	contextBanner := formatContextBanner(ctxResult)

	if perRunPrompt != "" || !interactive.CanPromptInteractively() {
		if scopeBanner != "" {
			fmt.Fprintln(out, scopeBanner)
		}
		fmt.Fprintln(out, contextBanner)
		if perRunPrompt != "" {
			fmt.Fprintln(out, "Context: "+perRunPrompt)
		}
		return perRunPrompt
	}

	noteTitle := "In scope"
	if scopeBanner != "" {
		noteTitle = scopeBanner
	}
	var value string
	form := newAccessibleForm(huh.NewGroup(
		huh.NewInput().
			Title("Add context for this run?").
			Description("Optional — press ↩ to skip").
			Value(&value),
		huh.NewNote().
			Title(noteTitle).
			Description(sanitizeForHuhNote(contextBanner)),
	))
	if err := form.RunWithContext(ctx); err != nil {
		// Cancellation = no per-run prompt; review proceeds.
		return ""
	}
	return strings.TrimSpace(value)
}

// sanitizeForHuhNote neutralises markdown that huh's Note renderer mangles in
// the terminal — notably backticks, which appear in checkpoint commit subjects
// like "add `entire review setup`". Display-only; the agent-facing checkpoint
// context (ContextResult.Prompt) is untouched.
func sanitizeForHuhNote(s string) string {
	return strings.ReplaceAll(s, "`", "'")
}

// findingsCount sums findings across all sources in a manifest. Used by
// the fix prompt header.
func findingsCount(m LocalReviewManifest) int {
	total := 0
	for _, src := range m.Sources {
		total += len(extractSourceFindings(reviewFixSource{
			Kind:   reviewFixSourceAgent,
			Agent:  src.Agent,
			Label:  src.Label,
			Output: src.Output,
		}, 0))
	}
	// If no structured findings parsed but at least one source has
	// non-empty output, treat the manifest as having one finding so
	// the prompt offers to apply something. Mirrors the
	// extractReviewFindings fallback used by `entire review fix`.
	if total == 0 {
		for _, src := range m.Sources {
			if strings.TrimSpace(src.Output) != "" {
				return 1
			}
		}
		if strings.TrimSpace(m.AggregateOutput) != "" {
			return 1
		}
	}
	return total
}

// postReviewFixLauncher abstracts the fix dispatch so RunPostReviewFixPrompt
// can be exercised in tests without spawning real agent subprocesses.
type postReviewFixLauncher func(ctx context.Context, cmd *cobra.Command, manifest LocalReviewManifest, fixer, perRunPrompt string, all bool, silentErr func(error) error) error

// launchFixFromManifest composes the manifest's prompt + the per-run
// prompt and dispatches the fix. The all=false branch defers to
// runReviewFix (which opens the selector); the all=true branch composes
// the prompt directly and calls agentlaunch.LaunchFixAgent.
func launchFixFromManifest(
	ctx context.Context,
	cmd *cobra.Command,
	manifest LocalReviewManifest,
	fixer, perRunPrompt string,
	all bool,
	silentErr func(error) error,
) error {
	if !all {
		// [s] / interactive picker — delegate to runReviewFix with the
		// manifest's handle as the target.
		return runReviewFix(ctx, cmd, reviewManifestHandle(manifest), false, fixer, perRunPrompt, silentErr)
	}
	sources, err := selectReviewFixSources(ctx, cmd, manifest, true /* all */)
	if err != nil {
		return err
	}
	prompt := composeReviewFixPrompt(manifest, sources)
	if perRunPrompt != "" {
		prompt += "\n\nAdditional context for this run:\n" + perRunPrompt
	}
	if err := agentlaunch.LaunchFixAgent(ctx, fixer, prompt); err != nil {
		return fmt.Errorf("launch review fix agent: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out)
	fmt.Fprintln(out, `Run: git commit -am "review: apply fixes"`)
	return nil
}

// printFindingsFooter prints the consolidated Run: block shown when the
// user picks [n] or when stdin isn't promptable.
func printFindingsFooter(w io.Writer, _ LocalReviewManifest) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Skipped. Findings preserved on disk.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run: entire review fix            apply when ready")
	fmt.Fprintln(w, "     entire review fix --all      apply all, skip picker")
	fmt.Fprintln(w, "     entire review findings       just browse the report")
}

// RunPostReviewFixPrompt is the end-of-review UX entrypoint. Called by
// runReview / runMultiAgentPath after the manifest is written. Always
// returns nil on the "skip" or "no findings" paths; only returns an
// error if the fix launch itself errored.
//
// userExplicitlyOmittedFixer is true when the invocation came via
// --reviewers <list> WITHOUT --fixer (the user signaled "just review,
// don't fix yet"). In that case the no-fixer footer is the inline
// `--fixer <agent>` hint, not the "Run: entire review setup" nag.
func RunPostReviewFixPrompt(
	ctx context.Context,
	cmd *cobra.Command,
	s *settings.EntireSettings,
	manifest LocalReviewManifest,
	perRunPrompt string,
	silentErr func(error) error,
	userExplicitlyOmittedFixer bool,
) error {
	return runPostReviewFixPromptWithDeps(ctx, cmd, s, manifest, perRunPrompt, silentErr, userExplicitlyOmittedFixer, launchFixFromManifest, os.Stdin, interactive.CanPromptInteractively())
}

// runPostReviewFixPromptWithDeps is the test-injectable form of
// RunPostReviewFixPrompt. Production code threads launchFixFromManifest,
// os.Stdin, and interactive.CanPromptInteractively(); tests inject
// capture stubs and a canPrompt flag to exercise the interactive switch
// branches without a real TTY.
func runPostReviewFixPromptWithDeps(
	ctx context.Context,
	cmd *cobra.Command,
	s *settings.EntireSettings,
	manifest LocalReviewManifest,
	perRunPrompt string,
	silentErr func(error) error,
	userExplicitlyOmittedFixer bool,
	launch postReviewFixLauncher,
	stdin io.Reader,
	canPrompt bool,
) error {
	out := cmd.OutOrStdout()
	if findingsCount(manifest) == 0 {
		return nil
	}

	fixer := FixerOf(s)
	if fixer == "" {
		// Two sub-cases:
		//   (a) saved config never set a fixer — point at setup.
		//   (b) --reviewers/--fixer was used and explicitly omitted
		//       --fixer — user signaled "just review, don't fix yet";
		//       offer a one-off `--fixer` hint instead of nagging
		//       about setup.
		fmt.Fprintln(out)
		if userExplicitlyOmittedFixer {
			fmt.Fprintln(out, "Findings ready. To apply: re-run with `--fixer <agent>`, or browse: `entire review findings`.")
		} else {
			fmt.Fprintln(out, "Found findings, but no Fixer is configured.")
			fmt.Fprintln(out, "Run: entire review setup")
		}
		return nil
	}

	if s != nil && s.FixAfterReview == settings.FixAfterReviewAlways {
		return launch(ctx, cmd, manifest, fixer, perRunPrompt, true /* all */, silentErr)
	}

	if !canPrompt {
		printFindingsFooter(out, manifest)
		return nil
	}

	fmt.Fprintf(out, "\nApply %d fixes now with %s?\n", findingsCount(manifest), displayLabelFor(fixer))
	fmt.Fprintln(out, "  [Y]es  ·  [s]elect fixes  ·  [n]o  ·  [A]lways")
	choice, err := ReadSingleKey(stdin, KeyChoice{Default: 'Y', Allowed: "YsnA"})
	if err != nil {
		return fmt.Errorf("read fix-prompt key: %w", err)
	}
	switch choice {
	case 'Y':
		return launch(ctx, cmd, manifest, fixer, perRunPrompt, true /* all */, silentErr)
	case 's':
		return launch(ctx, cmd, manifest, fixer, perRunPrompt, false /* all */, silentErr)
	case 'n':
		printFindingsFooter(out, manifest)
		return nil
	case 'A':
		if s == nil {
			s = &settings.EntireSettings{}
		}
		s.FixAfterReview = settings.FixAfterReviewAlways
		// Persist to clone-local preferences (gitignored), not project
		// settings.json. The legacy migration nudge fires when review
		// keys appear in the committable file; writing them there from
		// here would trip that prompt on every subsequent invocation.
		if err := saveFixAfterReviewPref(ctx, settings.FixAfterReviewAlways); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not persist 'always' preference:", err)
		}
		return launch(ctx, cmd, manifest, fixer, perRunPrompt, true /* all */, silentErr)
	}
	return nil
}

// saveFixAfterReviewPref persists the FixAfterReview mode into clone-local
// preferences (gitignored), where review-related settings belong. Mirrors
// the pattern in SaveReviewConfig / SaveReviewFixAgent (picker.go).
func saveFixAfterReviewPref(ctx context.Context, mode settings.FixAfterReviewMode) error {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return fmt.Errorf("load clone preferences: %w", err)
	}
	if prefs == nil {
		prefs = &settings.ClonePreferences{}
	}
	prefs.FixAfterReview = mode
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		return fmt.Errorf("save clone preferences: %w", err)
	}
	return nil
}
