package review

import (
	"fmt"
	"strings"
)

// formatContextBanner returns the transparency block printed below the scope
// banner. It itemises the prior checkpoint/session context `entire review` is
// folding into the agent prompt so the user can see exactly what's being
// reviewed — the value over running the underlying skill manually. The block
// is never omitted; the empty variant reassures the user nothing went wrong,
// there simply is no history.
//
// Example:
//
//	Checkpoints in scope (2):
//	  • a3b2c4d5  feat(review): emit honest live tokens
//	  • b4c3d5e6  feat(review): flag-driven roles
//	In-progress sessions (1):
//	  • ac3d5c6e  Claude Code
//
// When counts are present but the itemised slices aren't populated (defensive),
// it falls back to a one-line count summary.
func formatContextBanner(r ContextResult) string {
	if r.Checkpoints == 0 && r.Sessions == 0 {
		return "No prior session or checkpoint context for this branch yet."
	}
	var b strings.Builder
	switch {
	case len(r.CheckpointItems) > 0:
		fmt.Fprintf(&b, "Checkpoints in scope (%d):\n", len(r.CheckpointItems))
		for _, c := range r.CheckpointItems {
			summary := c.Summary
			if summary == "" {
				summary = "(no summary)"
			}
			fmt.Fprintf(&b, "  • %s  %s\n", c.ID, summary)
		}
	case r.Checkpoints > 0:
		fmt.Fprintf(&b, "%s in scope.\n", pluralizeContextNoun(r.Checkpoints, "checkpoint", "checkpoints"))
	}
	switch {
	case len(r.SessionItems) > 0:
		fmt.Fprintf(&b, "In-progress sessions (%d):\n", len(r.SessionItems))
		for _, s := range r.SessionItems {
			fmt.Fprintf(&b, "  • %s  %s\n", s.ID, s.Agent)
		}
	case r.Sessions > 0:
		fmt.Fprintf(&b, "%s in progress.\n", pluralizeContextNoun(r.Sessions, "session", "sessions"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// pluralizeContextNoun returns "<n> <singular>" when n == 1 and
// "<n> <plural>" otherwise. Kept private to banner.go; the review package
// has no other plural cases that would justify a shared utility.
func pluralizeContextNoun(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
