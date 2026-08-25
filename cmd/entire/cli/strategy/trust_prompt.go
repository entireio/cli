package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// TrustDecision is the resolved egress-trust outcome for a single pre-push in
// a globally enrolled repo.
type TrustDecision int

const (
	TrustHeld    TrustDecision = iota // checkpoints stay local this push; re-ask next push
	TrustGranted                      // consent recorded — the caller re-evaluates the gate, never reuses this as a boolean
)

type trustChoice string

const (
	trustChoiceYes    trustChoice = "yes"
	trustChoiceNotNow trustChoice = "not_now"
	trustChoiceAlways trustChoice = "always"
)

// resolveTrustDecision holds without prompting when there is no user to ask,
// otherwise asks and persists the answer. ask is injected for tests.
func resolveTrustDecision(ctx context.Context, hasTTY bool, ask func() (trustChoice, error), errOut io.Writer) (TrustDecision, error) {
	if !hasTTY {
		return TrustHeld, nil
	}
	choice, err := ask()
	if err != nil {
		return TrustHeld, err
	}
	return applyTrustChoice(ctx, choice, errOut), nil
}

// resolveTrustDecisionForPrePush is the production wiring: TTY detection plus
// the real uiform prompt. errOut receives persistence-failure warnings.
func resolveTrustDecisionForPrePush(ctx context.Context, errOut io.Writer) (TrustDecision, error) {
	return resolveTrustDecision(ctx, interactive.CanPromptInteractively(),
		func() (trustChoice, error) { return askTrustPrompt(ctx) }, errOut)
}

// applyTrustChoice persists the user's answer. A write failure warns and
// holds — never an error, which the pre-push hook would turn into a failed
// user push.
func applyTrustChoice(ctx context.Context, choice trustChoice, errOut io.Writer) TrustDecision {
	switch choice {
	case trustChoiceYes:
		// The same writer bare `entire trust` uses (one consent key shape).
		if _, err := settings.TrustCurrentRepo(ctx); err != nil {
			fmt.Fprintf(errOut, "Warning: couldn't save trust for this repo: %v\n", err)
			return TrustHeld
		}
		return TrustGranted
	case trustChoiceAlways:
		if err := settings.TrustAllRepos(ctx); err != nil {
			fmt.Fprintf(errOut, "Warning: couldn't save trust_all: %v\n", err)
			return TrustHeld
		}
		return TrustGranted
	case trustChoiceNotNow:
		return TrustHeld
	default:
		return TrustHeld
	}
}

// askTrustPrompt shows the three-option trust select. Pre-push stdin carries
// git ref lines — the form reads the terminal, never stdin. Ctrl-C / abort
// means "not now"; the user's push proceeds either way.
func askTrustPrompt(ctx context.Context) (trustChoice, error) {
	// An identity error needs no handling here: TrustCurrentRepo re-derives
	// it and its failure is warned-and-held by applyTrustChoice.
	yesLabel := "Yes — trust this repo (this folder only)"
	if id, err := settings.RepoTrustIdentity(ctx); err == nil && id.OriginKeyed() {
		yesLabel = fmt.Sprintf("Yes — trust this repo (all clones of %s)", id.DisplayScope())
	}
	// Start unset so accessible mode cannot turn an empty/default submission
	// into consent. Only one of the explicit option values grants anything.
	choice := trustChoice("")
	form := uiform.New(
		huh.NewGroup(
			huh.NewSelect[trustChoice]().
				Title("Entire captured agent sessions in this repo via global mode.").
				Description("Trust this folder and sync its checkpoints to your checkpoint sync remote?").
				Options(
					huh.NewOption(yesLabel, trustChoiceYes),
					huh.NewOption("Not now — keep capturing locally, ask again next push", trustChoiceNotNow),
					huh.NewOption("Always — trust every repo on this machine", trustChoiceAlways),
				).
				Value(&choice),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return trustChoiceNotNow, nil
		}
		return trustChoiceNotNow, fmt.Errorf("trust prompt: %w", err)
	}
	switch choice {
	case trustChoiceYes, trustChoiceNotNow, trustChoiceAlways:
		return choice, nil
	default:
		return trustChoiceNotNow, nil
	}
}
