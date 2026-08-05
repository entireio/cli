package main

import (
	"fmt"
	"io"
	"time"

	"charm.land/lipgloss/v2"
)

// mockDelay simulates real work so the flow feels authentic while iterating.
// Set to 0 with --fast for instant runs.
var mockDelay = 450 * time.Millisecond

var (
	styGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styCyan  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styBold  = lipgloss.NewStyle().Bold(true)
)

// runPlan executes the plan with mocked side effects: every step prints a
// progress line, pretends to work, then a ✓. Login/mirror print the shape of
// the real thing (browser, background clone) without doing any of it. This is
// the seam a real implementation would swap for actual work.
func runPlan(w io.Writer, p Plan) {
	fmt.Fprintln(w)
	handle := "Marvin"
	for _, step := range p.Steps {
		fmt.Fprintf(w, "  %s %s", styDim.Render("→"), step.Label)
		if step.Detail != "" {
			fmt.Fprintf(w, " %s", styDim.Render("· "+step.Detail))
		}
		fmt.Fprintln(w)
		time.Sleep(mockDelay)

		switch step.Kind {
		case StepLogin:
			fmt.Fprintf(w, "      %s\n", styDim.Render("opening https://entire.io/cli/auth … waiting for browser …"))
			time.Sleep(mockDelay)
			fmt.Fprintf(w, "    %s logged in as %s\n", styGreen.Render("✓"), styBold.Render(handle))
		case StepCreateMirror:
			fmt.Fprintf(w, "    %s mirror registered %s\n", styGreen.Render("✓"), styDim.Render("· initial clone continues in the background"))
		case StepCreateGitHubRepo:
			fmt.Fprintf(w, "    %s created %s\n", styGreen.Render("✓"), styBold.Render(p.Slug))
		case StepImport:
			fmt.Fprintf(w, "    %s imported %s\n", styGreen.Render("✓"), styDim.Render(step.Detail))
		default:
			fmt.Fprintf(w, "    %s done\n", styGreen.Render("✓"))
		}
	}
	fmt.Fprintln(w)
	printOutcome(w, p, handle)
}

// printOutcome is the closing line: the payoff when mirrored, or the local
// tracking + how-to-connect hint when not.
func printOutcome(w io.Writer, p Plan, handle string) {
	if p.Mirrors {
		url := "https://entire.io/gh/" + trimSlug(p.Slug)
		fmt.Fprintf(w, "%s %s %s %s\n",
			styGreen.Render("●"),
			styBold.Render("Connected:"),
			handle,
			styDim.Render("· ")+styCyan.Render(url))
		fmt.Fprintf(w, "  %s\n", styDim.Render("Code, commits, search & agent transcripts are now browsable there."))
		return
	}
	fmt.Fprintf(w, "%s %s\n", styGreen.Render("●"), styBold.Render("Enabled · tracking locally"))
	fmt.Fprintf(w, "  %s\n", styDim.Render("Not mirrored — entire.io shows only an onboarding page for this repo."))
	fmt.Fprintf(w, "  %s\n", styDim.Render("Unleash the full power of entire.io — re-run ")+styCyan.Render("entire enable")+styDim.Render(" to mirror this repo."))
	fmt.Fprintf(w, "  %s\n", styDim.Render("Docs: ")+styCyan.Render("https://docs.entire.io/guides/repositories/mirrors"))
}

func trimSlug(slug string) string {
	const p = "github.com/"
	if len(slug) > len(p) && slug[:len(p)] == p {
		return slug[len(p):]
	}
	return slug
}
