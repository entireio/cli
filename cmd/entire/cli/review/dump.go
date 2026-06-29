// Package review — see env.go for package-level rationale.
//
// dump.go provides DumpSink, a Sink implementation that writes a
// per-agent narrative dump to an io.Writer after the run completes.
// AgentEvent is a no-op; events are read from RunSummary.AgentRuns[].Buffer
// in RunFinished.
//
// Each agent's block is plain markdown written as-is — NOT glamour-rendered.
// Worker narratives are raw material (the final report is styled, and drill-in
// shows the buffer); styling multi-MB output here wedged the finalize phase on
// glamour's super-linear cost.
package review

import (
	"errors"
	"fmt"
	"io"
	"strings"

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

// dumpAgent writes one agent's section as plain markdown directly to W.
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
			if sameFailureError(re.Err, run.Err) {
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

	fmt.Fprint(s.W, b.String())
}

func writeFailureHeader(b *strings.Builder, runErr error) {
	if runErr == nil {
		b.WriteString("**Failed**\n\n")
		return
	}
	var pe *reviewtypes.ProcessError
	if errors.As(runErr, &pe) && pe.Stderr != "" {
		fmt.Fprintf(b, "**Failed:** `%s` exited (`%v`). Stderr:\n\n", pe.AgentName, pe.Err)
		fence := codeFenceFor(pe.Stderr)
		fmt.Fprintf(b, "%s\n%s\n%s\n\n", fence, pe.Stderr, fence)
		return
	}
	fmt.Fprintf(b, "**Failed:** `%v`\n\n", runErr)
}

// codeFenceFor returns a backtick fence at least 3 long and at least one
// longer than the longest backtick run in s — per CommonMark §4.5, the
// closing fence must match or exceed the opening fence length, so this
// prevents stderr content with embedded ``` lines from terminating the
// fence early and rendering trailing content raw.
func codeFenceFor(s string) string {
	longest, current := 0, 0
	for _, r := range s {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
			continue
		}
		current = 0
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

func sameFailureError(a, b error) bool {
	if a == nil || b == nil {
		return false
	}
	return errors.Is(a, b) || errors.Is(b, a)
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
