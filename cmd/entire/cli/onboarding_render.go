package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/onboarding"
)

// onboardingStatusResults computes the setup-ladder results for `entire
// status`. Package-level seam so status tests can stub ladder state without
// keyring/network access.
var onboardingStatusResults = func(ctx context.Context) []onboarding.Result {
	return onboardingLadder(newOnboardingRungDeps()).Checks(ctx)
}

// renderOnboardingChecklist renders the setup ladder for `entire status` and
// enable's closing summary. Fully connected collapses to a single line so the
// steady state stays quiet; anything less renders one row per applicable rung
// plus a pointer back at `entire enable`, which re-offers whatever is missing.
func renderOnboardingChecklist(results []onboarding.Result, sty statusStyles) string {
	if onboarding.Complete(results) {
		return renderConnectedLine(results, sty)
	}

	done, total := onboarding.Summary(results)
	var b strings.Builder
	b.WriteString(sty.render(sty.bold, fmt.Sprintf("Setup %d/%d complete", done, total)))
	b.WriteString("\n")

	titleWidth := 0
	for _, r := range results {
		if r.Check.State != onboarding.StateNotApplicable && len(r.Rung.Title) > titleWidth {
			titleWidth = len(r.Rung.Title)
		}
	}
	for _, r := range results {
		if r.Check.State == onboarding.StateNotApplicable {
			continue
		}
		b.WriteString("  ")
		b.WriteString(onboardingRowMarker(r.Check.State, sty))
		b.WriteString(" ")
		fmt.Fprintf(&b, "%-*s", titleWidth, r.Rung.Title)
		if r.Check.Detail != "" {
			b.WriteString("  ")
			b.WriteString(sty.render(sty.dim, r.Check.Detail))
		}
		b.WriteString("\n")
	}

	if remaining := len(onboarding.Missing(results)); remaining > 0 {
		step := "steps"
		if remaining == 1 {
			step = "step"
		}
		b.WriteString(sty.render(sty.cyan,
			fmt.Sprintf("→ Run `entire enable` to finish setup (%d %s left)", remaining, step)))
		b.WriteString("\n")
	}
	return b.String()
}

func onboardingRowMarker(state onboarding.State, sty statusStyles) string {
	switch state {
	case onboarding.StateDone:
		return sty.render(sty.green, "✓")
	case onboarding.StateMissing, onboarding.StateBlocked:
		return sty.render(sty.red, "✗")
	case onboarding.StateUnknown, onboarding.StateNotApplicable:
		return sty.render(sty.dim, "?")
	}
	return "?"
}

// renderConnectedLine is the fully-connected collapse: identity plus mirror
// slug when one exists, e.g. "Connected: peyton · mirrored to github.com/o/r".
func renderConnectedLine(results []onboarding.Result, sty statusStyles) string {
	var identity, mirror string
	for _, r := range results {
		switch r.Rung.Key {
		case onboarding.KeyAuth:
			identity = r.Check.Detail
		case onboarding.KeyMirror:
			if r.Check.State == onboarding.StateDone {
				mirror = r.Check.Detail
			}
		}
	}
	line := "Connected"
	if identity != "" {
		line += ": " + identity
	}
	if mirror != "" {
		line += sty.render(sty.dim, " · ") + "mirrored to " + mirror
	}
	return line + "\n"
}
