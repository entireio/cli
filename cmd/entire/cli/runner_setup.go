package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/spf13/cobra"
)

const runnerSetupLimitErrorMessage = "limit must be greater than 0"

// runnerSetupMode is what one `runner setup` invocation does. The modes differ
// in how far they go, not in whether they ask: consent is settled once, when
// the mode is resolved, which is what makes --yes answer every question the
// command can raise. It previously answered only the create-defaults
// confirmation and left the tailoring one to prompt anyway.
type runnerSetupMode string

const (
	// setupModeAdapt creates the defaults when the repo has none, then tailors
	// every runner in scope to this repo, in place. The full action, so it is
	// what --yes and the interactive default select.
	setupModeAdapt runnerSetupMode = "adapt"
	// setupModeDefaults creates the generic defaults and stops. No provider call.
	setupModeDefaults runnerSetupMode = "defaults"
	// setupModePrintPrompt creates the defaults as needed and prints the
	// tailoring prompt for the caller's own agent instead of running a provider.
	setupModePrintPrompt runnerSetupMode = "print-prompt"
	// setupModeDryRun tailors and prints the result as a diff, writing nothing —
	// not even the defaults, so in a fresh repo it previews the embedded set.
	setupModeDryRun runnerSetupMode = "dry-run"
	// setupModeNone is the zero value: no mode was chosen, because the picker
	// was cancelled. It authorizes nothing, which is why both predicates below
	// answer false for it.
	setupModeNone runnerSetupMode = ""
)

// writesRunnerFiles reports whether this mode may create or rewrite files under
// .entire/runners. It is the consent question, asked positively and answered by
// a total switch, so an unhandled mode writes nothing rather than everything —
// the zero value used to fall through to the most invasive path. `exhaustive`
// makes a new mode a build failure here rather than a silent write.
func (m runnerSetupMode) writesRunnerFiles() bool {
	switch m {
	case setupModeAdapt, setupModeDefaults, setupModePrintPrompt:
		return true
	case setupModeDryRun, setupModeNone:
		return false
	}
	return false
}

// gathersSignal reports whether this mode reads repository signal, and so
// whether --sources and --limit apply to it at all.
func (m runnerSetupMode) gathersSignal() bool {
	switch m {
	case setupModeAdapt, setupModePrintPrompt, setupModeDryRun:
		return true
	case setupModeDefaults, setupModeNone:
		return false
	}
	return false
}

// needsProvider reports whether this mode calls the summary provider, so the
// caller can resolve it before spending seconds gathering signal for it.
func (m runnerSetupMode) needsProvider() bool {
	switch m {
	case setupModeAdapt, setupModeDryRun:
		return true
	case setupModeDefaults, setupModePrintPrompt, setupModeNone:
		return false
	}
	return false
}

type runnerSetupOptions struct {
	runner       string // optional: limit to one runner (id, with or without "trail-")
	assumeYes    bool   // -y: answer every prompt, i.e. create the defaults and tailor them
	defaultsOnly bool
	printPrompt  bool
	dryRun       bool
	agent        string // --agent: text-generation agent for this run; skips the provider choice, saves nothing
	debugDir     string // if set, dump prompt.txt (+ response.txt when a provider ran) here
	sources      []string
	limit        int
	insecureHTTP bool
}

func newRunnerSetupCmd() *cobra.Command {
	var (
		opts          runnerSetupOptions
		deprecatedRun bool
	)

	cmd := &cobra.Command{
		Use:   "setup [<runner>]",
		Short: "Create and tailor this repository's trail runners",
		Long: `Set up the .entire/runners/*.json evaluators for this repository.

Runners (risk, confidence, drift, security, review, …) score and review a
branch's changes. The shipped defaults are generic; setup can tailor them to
THIS repo using gathered signal — its docs and structure, merged PRs and
issues, checkpoint churn hotspots, and past trail findings.

In a terminal with no flags, setup asks whether you want the generic defaults
or defaults tailored to this repo. Otherwise name the action up front:

  -y, --yes           create the defaults if missing, then tailor them in place
      --defaults-only create the generic defaults and stop
      --print-prompt  create the defaults if missing, then print the tailoring prompt for your own agent to run
      --dry-run       show the tailoring as a diff and write nothing

--yes and --dry-run each call your configured summary provider once. With no
provider configured and several agents installed, you are asked which to use;
pass --agent <name> to name one for this run instead (nothing is saved).
Review a tailoring with git diff .entire/runners.

If <runner> is given (e.g. "risk" or "trail-risk"), only that runner is tuned.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.runner = args[0]
			}
			if deprecatedRun {
				opts.assumeYes = true
			}
			opts.insecureHTTP = runnerInsecureHTTP(cmd)
			return runRunnerSetup(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}

	cmd.Flags().BoolVarP(&opts.assumeYes, "yes", "y", false,
		"Create the default runners if missing and tailor them to this repo; add --agent to skip the provider choice too")
	cmd.Flags().BoolVar(&opts.defaultsOnly, "defaults-only", false,
		"Create the generic default runners and stop (no tailoring, no provider call)")
	cmd.Flags().BoolVar(&opts.printPrompt, "print-prompt", false,
		"Create the default runners if missing, then print the tailoring prompt for your own agent instead of running a provider")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false,
		"Show the tailoring as a diff and write nothing")
	cmd.Flags().StringVar(&opts.agent, "agent", "",
		"Text-generation agent to tailor with for this run (e.g. codex); skips the provider choice and saves nothing. Only for runs that tailor: --yes, --dry-run, or choosing to tailor at the prompt")
	cmd.Flags().StringSliceVar(&opts.sources, "sources", nil,
		"Comma-separated data sources to gather: repo, prs, checkpoints, trails, all (default: all)")
	cmd.Flags().IntVar(&opts.limit, "limit", 20, "How many recent PRs/issues/trails to sample")
	cmd.Flags().StringVar(&opts.debugDir, "debug-dir", "",
		"Write the assembled prompt (prompt.txt) and, when a provider runs, its raw response (response.txt) to this directory for debugging")

	// --run was the flag that made setup finish its job; tailoring is now the
	// default action, so it survives only as an alias for the flag that means it.
	cmd.Flags().BoolVar(&deprecatedRun, "run", false, "Deprecated alias for --yes")
	if err := cmd.Flags().MarkDeprecated("run", "use --yes (tailoring is now the default action)"); err != nil {
		panic(fmt.Sprintf("deprecate run flag: %v", err))
	}

	cmd.MarkFlagsMutuallyExclusive("defaults-only", "print-prompt", "dry-run")

	return cmd
}

func runRunnerSetup(ctx context.Context, w, errW io.Writer, opts runnerSetupOptions) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}

	haveRunners := runnerConfigsExist(repoRoot)
	mode, err := resolveRunnerSetupMode(ctx, errW, opts, haveRunners)
	if err != nil {
		return err
	}
	if mode == setupModeNone {
		return nil // picker cancelled; handleFormCancellation already said so
	}

	// --agent names the agent that tailors, so it is a contradiction for a mode
	// that never tailors — the same rule as dispatch's --agent without --local.
	// Adding -y cannot rescue such a run: --defaults-only and --print-prompt
	// outrank it in resolveRunnerSetupMode. Before the scaffold: a usage error
	// must not leave files behind.
	if opts.agent != "" && !mode.needsProvider() {
		return errors.New("--agent names the agent that tailors, but this run does not tailor; drop --agent, or tailor with --yes or --dry-run instead of --defaults-only or --print-prompt")
	}

	// --sources and --limit only steer the gather, so they are validated for
	// the modes that gather and not for --defaults-only, which reads neither.
	var src tuneSources
	if mode.gathersSignal() {
		if src, err = parseTuneSources(opts.sources); err != nil {
			return err
		}
		if opts.limit <= 0 {
			return errors.New(runnerSetupLimitErrorMessage)
		}
	}

	// Choosing a mode was the consent for creating the runner files. In
	// print-prompt mode stdout is the prompt itself — callers redirect it into
	// their own agent — so the "created" lines are narration and belong on
	// stderr with the rest of that mode's messages.
	var created []string
	if mode.writesRunnerFiles() && !haveRunners {
		scaffoldW := w
		if mode == setupModePrintPrompt {
			scaffoldW = errW
		}
		if created, err = createDefaultRunners(scaffoldW, repoRoot); err != nil {
			return err
		}
	}

	if mode == setupModeDefaults {
		if len(created) == 0 {
			fmt.Fprintln(errW, "Runners already configured. Nothing to do.")
			return nil
		}
		reportCreatedDefaults(errW, len(created), mode)
		return nil
	}

	// Resolved before the gather: resolution can fail outright, or stop to ask
	// which provider to use, and neither belongs after seconds of waiting. An
	// --agent override takes dispatch's path: validated, used once, not saved.
	var provider *checkpointSummaryProvider
	if mode.needsProvider() {
		if provider, err = resolveDispatchSummaryProvider(ctx, errW, opts.agent); err != nil {
			return err
		}
	}

	// A dry run creates nothing, so in a repo with no runners it previews the
	// tailoring of the embedded set it would otherwise have written. That is
	// the one place the preview's source differs, so it says so here.
	loadRunners := loadTuneRunners
	if !mode.writesRunnerFiles() && !haveRunners {
		loadRunners = defaultTuneRunners
		fmt.Fprintf(errW, "This repo has no runners yet and %s creates none, so the preview is against the embedded defaults.\n", mode)
	}
	runners, err := loadRunners(repoRoot, opts.runner)
	if err != nil {
		return err
	}
	if len(created) > 0 && mode == setupModeAdapt {
		reportCreatedDefaults(errW, len(created), mode)
	}

	stopGather := startSpinner(errW, "Gathering repository signal")
	brief := gatherTuningContext(ctx, errW, repoRoot, src, opts.limit, opts.insecureHTTP)
	stopGather(true)
	prompt := buildTunePrompt(brief, runners)

	if opts.debugDir != "" {
		writeTuneDebug(errW, opts.debugDir, "prompt.txt", prompt)
	}

	if mode == setupModePrintPrompt {
		fmt.Fprintln(w, prompt)
		if len(created) > 0 {
			reportCreatedDefaults(errW, len(created), mode)
		}
		fmt.Fprintf(errW, "\n%d runner(s) in scope. Paste the prompt above into your agent, or re-run with --yes to apply it headlessly.\n", len(runners))
		return nil
	}

	changes, skipped, err := runTuning(ctx, errW, provider, runners, prompt, opts.debugDir)
	if err != nil {
		return err
	}
	switch mode {
	case setupModeDryRun:
		previewTunedRunners(w, errW, len(runners), changes, skipped)
		return nil
	case setupModeAdapt:
		return applyTunedRunners(w, errW, repoRoot, changes, skipped, created)
	case setupModeDefaults, setupModePrintPrompt, setupModeNone:
		// All returned above. Reaching here means an early return was removed.
	}
	return fmt.Errorf("unhandled runner setup mode %q", mode)
}

// reportCreatedDefaults says the same thing about a fresh scaffold in every
// mode — one sentence, one place, so the three call sites cannot drift apart
// the way "default" / "generic default" / "working default" already had.
func reportCreatedDefaults(errW io.Writer, n int, mode runnerSetupMode) {
	next := "run `entire runner setup -y` to tailor them to this repo"
	switch mode {
	case setupModeAdapt:
		next = "tailoring them to this repo now…"
	case setupModePrintPrompt:
		next = "paste the prompt above into your agent to tailor them to this repo"
	case setupModeDefaults, setupModeDryRun, setupModeNone:
	}
	fmt.Fprintf(errW, "\nCreated %d default runner(s) (untracked, functional as-is); %s\n", n, next)
}

// resolveRunnerSetupMode settles what this invocation will do, and is the only
// place consent is taken. An explicit mode flag wins, --yes means the full
// action, a terminal is asked, and a non-interactive caller that named nothing
// is told which flag to pass rather than being given half the job.
func resolveRunnerSetupMode(ctx context.Context, errW io.Writer, opts runnerSetupOptions, haveRunners bool) (runnerSetupMode, error) {
	switch {
	case opts.dryRun:
		return setupModeDryRun, nil
	case opts.printPrompt:
		return setupModePrintPrompt, nil
	case opts.defaultsOnly:
		return setupModeDefaults, nil
	case opts.assumeYes:
		return setupModeAdapt, nil
	case interactive.CanPromptInteractively():
		return chooseRunnerSetupAction(ctx, errW, haveRunners)
	default:
		return setupModeNone, errors.New("no terminal to ask what setup should do: pass --yes (create the default runners and tailor them), --defaults-only, --print-prompt, or --dry-run — see --help")
	}
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
