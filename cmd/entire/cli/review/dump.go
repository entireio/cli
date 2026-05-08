// Package review — see env.go for package-level rationale.
//
// dump.go provides DumpSink, a Sink implementation that writes a
// per-agent narrative dump to an io.Writer after the run completes.
// AgentEvent is a no-op; events are read from RunSummary.AgentRuns[].Buffer
// in RunFinished.
//
// Output format: each agent's block is composed as markdown (`# claude-code
// review`, with failure context in blockquotes/bold) and rendered through
// mdrender for terminal writers. Non-TTY writers receive raw markdown so
// pipelines can grep / pipe / save without ANSI escape codes.
package review

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/mdrender"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

// DumpSink writes per-agent narrative blocks to W after the run completes.
type DumpSink struct {
	W io.Writer
}

// Compile-time interface check.
var _ reviewtypes.Sink = DumpSink{}

// AgentEvent is intentionally a no-op. DumpSink renders post-run from
// the AgentRun.Buffer slices in RunFinished.
func (DumpSink) AgentEvent(_ string, _ reviewtypes.Event) {}

// RunFinished writes a narrative block per agent, then a counts line.
func (s DumpSink) RunFinished(summary reviewtypes.RunSummary) {
	for _, run := range summary.AgentRuns {
		s.dumpAgent(run)
	}
	s.dumpCounts(summary)
}

// dumpAgent composes one agent's section as markdown and writes it through
// mdrender. The counts line at the end of the run is intentionally NOT
// rendered through markdown — it's a terse status summary that benefits
// from staying on a single uncolored line for grep-ability.
//
// Markdown structure per agent:
//
//	# <name> review
//	(optional status line for cancelled / failed)
//	(optional blockquote for RunError events on failure)
//	(narrative — agent's AssistantText events joined)
func (s DumpSink) dumpAgent(run reviewtypes.AgentRun) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s review\n\n", run.Name)

	switch run.Status {
	case reviewtypes.AgentStatusCancelled:
		b.WriteString("_cancelled_\n")
	case reviewtypes.AgentStatusFailed:
		// Surface the wait error if any (process exit failure), then any
		// agent-level RunError events the parser emitted (typically a torn
		// stdout stream — caught at the orchestrator level by classifyStatus
		// even when the process itself exited 0).
		writeFailureHeader(&b, run.Err)
		for _, ev := range run.Buffer {
			re, ok := ev.(reviewtypes.RunError)
			if !ok || re.Err == nil {
				continue
			}
			// Skip the synthetic RunError that the orchestrator emits to drive
			// live TUI updates: it carries the same error as run.Err, which
			// the failure header already rendered. Without this the same
			// error text appears twice in adjacent output blocks.
			if run.Err != nil && errors.Is(re.Err, run.Err) {
				continue
			}
			fmt.Fprintf(&b, "> agent error: `%v`\n\n", re.Err)
		}
		// Render any narrative text the agent produced before the failure
		// surfaced — useful when the parser tore mid-response so reviewers
		// can see partial output instead of a bare "(failed)" line.
		if narrative := joinAssistantText(run.Buffer); narrative != "" {
			b.WriteString(narrative)
			b.WriteString("\n")
		}
	case reviewtypes.AgentStatusSucceeded, reviewtypes.AgentStatusUnknown:
		if narrative := joinAssistantText(run.Buffer); narrative != "" {
			b.WriteString(narrative)
			b.WriteString("\n")
		}
	}

	// RenderForWriter is TTY-aware: returns raw markdown for non-TTY writers,
	// glamour-styled output otherwise. Errors are best-effort — fall back to
	// raw markdown so the user always gets the content.
	rendered, err := mdrender.RenderForWriter(s.W, b.String())
	if err != nil {
		rendered = b.String()
	}
	fmt.Fprint(s.W, rendered)
}

// writeFailureHeader formats the failure block for a failed agent run.
// When run.Err is a *ProcessError carrying captured stderr, the stderr is
// rendered in a fenced code block on its own — preserving newlines so multi-line
// agent CLI output (auth errors, stack traces, etc.) is readable instead of
// collapsed inside an inline-code span. Generic errors (non-ProcessError, or
// ProcessError without stderr) keep the inline backtick rendering.
func writeFailureHeader(b *strings.Builder, runErr error) {
	if runErr == nil {
		b.WriteString("**Failed**\n\n")
		return
	}
	var pe *reviewtypes.ProcessError
	if errors.As(runErr, &pe) && pe.Stderr != "" {
		fmt.Fprintf(b, "**Failed:** `%s` exited (`%v`). Stderr:\n\n", pe.AgentName, pe.Err)
		fmt.Fprintf(b, "```\n%s\n```\n\n", pe.Stderr)
		return
	}
	fmt.Fprintf(b, "**Failed:** `%v`\n\n", runErr)
}

// joinAssistantText extracts AssistantText events from a buffer and joins
// them with newlines, trimming the result to keep dump output tight.
func joinAssistantText(buf []reviewtypes.Event) string {
	var b strings.Builder
	for _, ev := range buf {
		if at, ok := ev.(reviewtypes.AssistantText); ok {
			b.WriteString(at.Text)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func (s DumpSink) dumpCounts(summary reviewtypes.RunSummary) {
	succ, fail, canc := 0, 0, 0
	for _, r := range summary.AgentRuns {
		switch r.Status {
		case reviewtypes.AgentStatusSucceeded:
			succ++
		case reviewtypes.AgentStatusFailed:
			fail++
		case reviewtypes.AgentStatusCancelled:
			canc++
		case reviewtypes.AgentStatusUnknown:
			// Unknown status: not counted in any bucket.
		}
	}
	fmt.Fprintf(s.W, "%d agent(s) done — %d succeeded, %d failed, %d cancelled\n",
		len(summary.AgentRuns), succ, fail, canc)
}
