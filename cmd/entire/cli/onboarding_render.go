package cli

import (
	"context"
	"fmt"
	"net/url"
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
// plus, when actionable steps remain, a pointer back at `entire enable`,
// which re-offers whatever is missing.
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
		detail := r.Check.Detail
		// An unknown rung (e.g. offline mirror probe) doesn't count as a
		// missing step, so its hint would otherwise never surface — carry it
		// in the row instead of leaving a bare "?" with nothing actionable.
		if detail == "" && r.Check.State == onboarding.StateUnknown && r.Check.Hint != "" {
			detail = "couldn't check — retry with `" + r.Check.Hint + "`"
		}
		if detail != "" {
			b.WriteString("  ")
			b.WriteString(sty.render(sty.dim, detail))
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

// renderConnectedLine is the fully-connected collapse: identity plus, when a
// mirror exists, the repo's entire.io overview link — the payoff the setup
// was for, e.g. "Connected: peyton · https://entire.io/gh/o/r".
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
		if u := repoWebURL(mirror); u != "" {
			line += sty.render(sty.dim, " · ") + sty.render(sty.cyan, u)
		} else {
			line += sty.render(sty.dim, " · ") + "mirrored to " + mirror
		}
	}
	return line + "\n"
}

// repoWebURL builds the entire.io overview page for a mirrored GitHub repo
// from the checklist's "github.com/owner/repo" detail; "" when the detail
// isn't that shape.
func repoWebURL(mirrorSlug string) string {
	rest, ok := strings.CutPrefix(mirrorSlug, "github.com/")
	if !ok {
		return ""
	}
	owner, repo, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("%s/gh/%s/%s", webBaseURL(), url.PathEscape(owner), url.PathEscape(repo))
}
