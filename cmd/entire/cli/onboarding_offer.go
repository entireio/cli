package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/onboarding"
)

// onboardingSetupMode is the answer to enable's single consent prompt.
type onboardingSetupMode int

const (
	// setupModeAll runs every missing rung back-to-back without further questions.
	setupModeAll onboardingSetupMode = iota
	// setupModeStepByStep confirms each missing rung individually.
	setupModeStepByStep
	// setupModeSkip declines the offers this once — tracking continues either
	// way; the closing checklist carries the run-later commands and re-running
	// enable re-offers whatever is still missing.
	setupModeSkip
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
	promptMode  func(missing []onboarding.Result) (onboardingSetupMode, error)
	confirmRung func(r onboarding.Result) (bool, error)
	styles      func(w io.Writer) statusStyles
}

func (r onboardingOfferRunner) run(ctx context.Context, w io.Writer) {
	ladder := onboardingLadder(r.deps)
	results := ladder.Checks(ctx)

	if actionable := r.offerable(results); len(actionable) > 0 && r.canPrompt() {
		mode, err := r.promptMode(actionable)
		if err != nil {
			// Cancelling the consent prompt (Ctrl-C) behaves like skip: enable
			// has already succeeded, so fall through to the checklist, whose
			// hints are the resume path.
			mode = setupModeSkip
		}
		if mode != setupModeSkip {
			succeeded := r.runOffers(ctx, w, ladder, mode)
			results = patchSucceededOffers(ladder.Checks(ctx), succeeded)
		}
	}

	fmt.Fprint(w, r.renderChecklist(w, results))
}

// patchSucceededOffers reconciles the closing re-check with what just
// happened: a rung whose offer succeeded this pass but whose probe hasn't
// caught up (mirror create returns before the server-side clone finishes)
// renders as in-progress, not as missing with a retry hint.
func patchSucceededOffers(results []onboarding.Result, succeeded map[string]bool) []onboarding.Result {
	for i, res := range results {
		if !succeeded[res.Rung.Key] || res.Check.State == onboarding.StateDone {
			continue
		}
		results[i].Check = onboarding.Check{
			State:  onboarding.StateUnknown,
			Detail: "created — sync in progress",
		}
	}
	return results
}

// offerable filters the remaining work down to rungs this runner can act on:
// missing or blocked, with an offer wired. Blocked rungs count because an
// earlier offer in the same pass (login) can unblock them.
func (r onboardingOfferRunner) offerable(results []onboarding.Result) []onboarding.Result {
	var actionable []onboarding.Result
	for _, res := range onboarding.Missing(results) {
		if r.offerFns[res.Rung.Key] != nil {
			actionable = append(actionable, res)
		}
	}
	return actionable
}

// runOffers walks the ladder in order, re-checking each rung immediately
// before its offer, and returns the keys whose offer succeeded. Offers are
// best-effort: a failure prints a notice and the closing checklist keeps the
// retry hint, but enable never fails.
func (r onboardingOfferRunner) runOffers(ctx context.Context, w io.Writer, ladder onboarding.Ladder, mode onboardingSetupMode) map[string]bool {
	succeeded := map[string]bool{}
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
			accepted, err := r.confirmRung(onboarding.Result{Rung: rung, Check: check})
			if err != nil || !accepted {
				continue
			}
		}
		if err := offer(ctx); err != nil {
			fmt.Fprintf(w, "  %s setup didn't complete: %v\n", rung.Title, err)
			continue
		}
		succeeded[rung.Key] = true
	}
	return succeeded
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
	r := newOnboardingOfferRunner(w)
	if assumeYes {
		r.canPrompt = func() bool { return false }
	}
	fmt.Fprintln(w)
	r.run(ctx, w)
}

// newOnboardingOfferRunner wires the production runner: real rung probes,
// the browser login and mirror-create offers, and huh-based prompts.
func newOnboardingOfferRunner(errW io.Writer) onboardingOfferRunner {
	deps := newOnboardingRungDeps()
	return onboardingOfferRunner{
		deps: deps,
		offerFns: map[string]func(ctx context.Context) error{
			onboarding.KeyAuth:   func(ctx context.Context) error { return runOnboardingLogin(ctx, errW) },
			onboarding.KeyMirror: func(ctx context.Context) error { return runOnboardingMirrorCreate(ctx, errW, deps) },
		},
		canPrompt:   interactive.CanPromptInteractively,
		promptMode:  promptOnboardingSetupMode,
		confirmRung: confirmOnboardingRung,
		styles:      newStatusStyles,
	}
}

// promptOnboardingSetupMode is enable's single consent prompt, summarizing
// what "finish setup" will do from the missing rungs' titles.
func promptOnboardingSetupMode(missing []onboarding.Result) (onboardingSetupMode, error) {
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
	if err := form.Run(); err != nil {
		return setupModeSkip, fmt.Errorf("setup consent prompt: %w", err)
	}
	return mode, nil
}

// onboardingSetupSummary describes what the fast path will do, e.g.
// "Logs in to entire.io and mirrors github.com/acme/api so sessions and
// commits appear in the web UI."
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
	summary := steps[0]
	var summarySb188 strings.Builder
	for i := 1; i < len(steps); i++ {
		if i == len(steps)-1 {
			summarySb188.WriteString(" and " + steps[i])
		} else {
			summarySb188.WriteString(", " + steps[i])
		}
	}
	summary += summarySb188.String()
	// Capitalize the first step: summaries are full sentences in the prompt.
	return string(summary[0]-'a'+'A') + summary[1:] + " so your work shows up in the web UI."
}

func confirmOnboardingRung(r onboarding.Result) (bool, error) {
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
	if err := form.Run(); err != nil {
		return false, fmt.Errorf("rung confirm prompt: %w", err)
	}
	return confirmed, nil
}
