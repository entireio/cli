package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/onboarding"
)

// onboardingSetupMode is the answer to enable's single consent prompt.
type onboardingSetupMode int

const (
	// setupModeSkip declines the offers this once — tracking continues either
	// way; the closing checklist carries the run-later commands and re-running
	// enable re-offers whatever is still missing. Deliberately the zero value:
	// a forgotten or defaulted mode must never run login or mirror creation.
	setupModeSkip onboardingSetupMode = iota
	// setupModeAll runs every missing rung back-to-back without further questions.
	setupModeAll
	// setupModeStepByStep confirms each missing rung individually.
	setupModeStepByStep
)

// onboardingOfferRunner drives the connect rungs at the end of `entire
// enable`: one consent prompt, then offers for whatever is missing, then the
// closing checklist. Rung state is recomputed before each offer, so a login
// completed in this pass unblocks the mirror rung in the same pass, and a
// rung finished out-of-band is never re-offered — which is also what makes
// re-running `entire enable` the resume path after an interrupted setup.
// All fields are seams; newOnboardingOfferRunner wires production behavior.
type onboardingOfferRunner struct {
	deps        onboardingRungDeps
	offerFns    map[string]func(ctx context.Context) error
	canPrompt   func() bool
	promptMode  func(ctx context.Context, missing []onboarding.Result) (onboardingSetupMode, error)
	confirmRung func(ctx context.Context, r onboarding.Result) (bool, error)
	styles      func(w io.Writer) statusStyles
}

func (r onboardingOfferRunner) run(ctx context.Context, w io.Writer) {
	ladder := onboardingLadder(r.deps)
	results := ladder.Checks(ctx)

	if actionable := r.offerable(results); len(actionable) > 0 && r.canPrompt() {
		mode, err := r.promptMode(ctx, actionable)
		if err != nil {
			// Cancelling the consent prompt (Ctrl-C) behaves like skip: enable
			// has already succeeded, so fall through to the checklist, whose
			// hints are the resume path. Logged so a broken terminal doesn't
			// masquerade as a user choice.
			logging.Debug(ctx, "onboarding: consent prompt did not complete", "error", err)
			mode = setupModeSkip
		}
		if mode != setupModeSkip {
			r.runOffers(ctx, w, ladder, mode)
			results = ladder.Checks(ctx)
		}
	}

	fmt.Fprint(w, r.renderChecklist(w, results))
}

// offerable filters the remaining work down to rungs this runner can act on.
// Missing rungs with an offer always qualify. Blocked rungs qualify only when
// an offerable Missing rung precedes them in the ladder — an earlier offer in
// the same pass (login) can unblock them. Without that, the consent prompt
// would promise work that can never run (e.g. auth Unknown leaves mirror
// Blocked with nothing to unblock it).
func (r onboardingOfferRunner) offerable(results []onboarding.Result) []onboarding.Result {
	var actionable []onboarding.Result
	unblockPossible := false
	for _, res := range results {
		if res.Check.State == onboarding.StateMissing && r.offerFns[res.Rung.Key] != nil {
			actionable = append(actionable, res)
			unblockPossible = true
			continue
		}
		if res.Check.State == onboarding.StateBlocked && r.offerFns[res.Rung.Key] != nil && unblockPossible {
			actionable = append(actionable, res)
		}
	}
	return actionable
}

// runOffers walks the ladder in order, re-checking each rung immediately
// before its offer. Offers are best-effort: a failure prints a notice and
// the closing checklist keeps the retry hint, but enable never fails.
func (r onboardingOfferRunner) runOffers(ctx context.Context, w io.Writer, ladder onboarding.Ladder, mode onboardingSetupMode) {
	for _, rung := range ladder {
		offer := r.offerFns[rung.Key]
		if offer == nil {
			continue
		}
		check := rung.Check(ctx)
		if check.State != onboarding.StateMissing {
			continue
		}
		if mode == setupModeStepByStep {
			accepted, err := r.confirmRung(ctx, onboarding.Result{Rung: rung, Check: check})
			if err != nil {
				logging.Debug(ctx, "onboarding: rung confirm prompt did not complete", "rung", rung.Key, "error", err)
				continue
			}
			if !accepted {
				continue
			}
		}
		if err := offer(ctx); err != nil {
			logging.Warn(ctx, "onboarding: offer failed", "rung", rung.Key, "error", err)
			fmt.Fprintf(w, "  %s setup didn't complete: %v\n", rung.Title, err)
		}
	}
}

func (r onboardingOfferRunner) renderChecklist(w io.Writer, results []onboarding.Result) string {
	sty := r.styles(w)
	var b strings.Builder
	b.WriteString(renderOnboardingChecklist(results, sty))
	// One "run later" line per unique command, in ladder order — a blocked
	// rung whose fix is another rung's command (mirror needs login) must not
	// repeat it.
	seen := map[string]bool{}
	for _, res := range onboarding.Missing(results) {
		if res.Check.Hint == "" || seen[res.Check.Hint] {
			continue
		}
		seen[res.Check.Hint] = true
		fmt.Fprintf(&b, "  run later: %s\n", sty.render(sty.cyan, res.Check.Hint))
	}
	return b.String()
}

// runEnableOnboarding runs the connect ladder at the end of `entire enable`.
// assumeYes (--yes) forces the non-interactive contract: no prompts and no
// implicit login/mirror — the checklist hints carry the follow-up commands.
// Best-effort by design: onboarding output problems never fail enable.
var runEnableOnboarding = func(ctx context.Context, w io.Writer, assumeYes bool) {
	// The user explicitly asked for setup — retry probes that recently
	// failed instead of serving the cached failure (which would silently
	// suppress the mirror offer for the rest of the failure TTL).
	defaultMirrorProbeCache().clearUnreachable()
	r := newOnboardingOfferRunner(w)
	if assumeYes {
		r.canPrompt = func() bool { return false }
	}
	fmt.Fprintln(w)
	r.run(ctx, w)
}

// newOnboardingOfferRunner wires the production runner: real rung probes,
// the login/mirror/import offers, and huh-based prompts. w is enable's
// output writer — offers write their progress there.
func newOnboardingOfferRunner(w io.Writer) onboardingOfferRunner {
	deps := newOnboardingRungDeps()
	return onboardingOfferRunner{
		deps: deps,
		offerFns: map[string]func(ctx context.Context) error{
			onboarding.KeyAuth:   func(ctx context.Context) error { return runOnboardingLogin(ctx, w) },
			onboarding.KeyMirror: func(ctx context.Context) error { return runOnboardingMirrorCreate(ctx, w, deps) },
			onboarding.KeyImport: func(ctx context.Context) error { return runOnboardingImport(ctx, w) },
		},
		canPrompt:   interactive.CanPromptInteractively,
		promptMode:  promptOnboardingSetupMode,
		confirmRung: confirmOnboardingRung,
		styles:      newStatusStyles,
	}
}

// promptOnboardingSetupMode is enable's single consent prompt, summarizing
// what "finish setup" will do from the missing rungs.
func promptOnboardingSetupMode(ctx context.Context, missing []onboarding.Result) (onboardingSetupMode, error) {
	summary := onboardingSetupSummary(missing)
	mode := setupModeAll
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[onboardingSetupMode]().
				Title("Finish setup?").
				Description(summary).
				Options(
					huh.NewOption("Yes, set up everything (recommended)", setupModeAll),
					huh.NewOption("Choose step by step", setupModeStepByStep),
					huh.NewOption("Skip for now — finish anytime with `entire enable`", setupModeSkip),
				).
				Value(&mode),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return setupModeSkip, fmt.Errorf("setup consent prompt: %w", err)
	}
	return mode, nil
}

// onboardingSetupSummary describes what the fast path will do, e.g.
// "Logs in to entire.io and mirrors this repo so your work shows up in the
// web UI."
func onboardingSetupSummary(missing []onboarding.Result) string {
	steps := make([]string, 0, len(missing))
	for _, r := range missing {
		switch r.Rung.Key {
		case onboarding.KeyAuth:
			steps = append(steps, "logs in to entire.io")
		case onboarding.KeyMirror:
			steps = append(steps, "mirrors this repo")
		case onboarding.KeyImport:
			steps = append(steps, "imports existing agent history")
		}
	}
	if len(steps) == 0 {
		return ""
	}
	summary := formatTokenClassList(steps)
	// Capitalize the first step: summaries are full sentences in the prompt.
	return strings.ToUpper(summary[:1]) + summary[1:] + " so your work shows up in the web UI."
}

func confirmOnboardingRung(ctx context.Context, r onboarding.Result) (bool, error) {
	title := r.Rung.Title
	if r.Check.Detail != "" {
		title += " — " + r.Check.Detail
	}
	confirmed := true
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(title).
				Value(&confirmed),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		return false, fmt.Errorf("rung confirm prompt: %w", err)
	}
	return confirmed, nil
}
