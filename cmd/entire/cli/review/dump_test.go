package review

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func makeSummary(runs ...reviewtypes.AgentRun) reviewtypes.RunSummary {
	return reviewtypes.RunSummary{AgentRuns: runs}
}

// Tests use bytes.Buffer as the writer, which is NOT a terminal — so DumpSink's
// markdown is passed through as-is via mdrender.RenderForWriter. Assertions
// therefore match the raw markdown body the user would see when running
// `entire review > out.txt`.

func TestDumpSink_SucceededAgent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	run := reviewtypes.AgentRun{
		Name:   "claude-code",
		Status: reviewtypes.AgentStatusSucceeded,
		Buffer: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "First finding"},
			reviewtypes.AssistantText{Text: "Second finding"},
		},
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if !strings.Contains(out, "# claude-code review") {
		t.Errorf("expected markdown agent heading, got:\n%s", out)
	}
	if !strings.Contains(out, "First finding") {
		t.Errorf("expected first finding in output, got:\n%s", out)
	}
	if !strings.Contains(out, "Second finding") {
		t.Errorf("expected second finding in output, got:\n%s", out)
	}
	if !strings.Contains(out, "1 agent(s) done — 1 succeeded, 0 failed, 0 cancelled") {
		t.Errorf("expected counts line, got:\n%s", out)
	}
}

func TestDumpSink_FailedAgentWithErr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	run := reviewtypes.AgentRun{
		Name:   "codex",
		Status: reviewtypes.AgentStatusFailed,
		Err:    errors.New("binary not found"),
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if !strings.Contains(out, "**Failed:** `binary not found`") {
		t.Errorf("expected bold failure marker with error, got:\n%s", out)
	}
	if !strings.Contains(out, "1 agent(s) done — 0 succeeded, 1 failed, 0 cancelled") {
		t.Errorf("expected counts line, got:\n%s", out)
	}
}

func TestDumpSink_FailedAgentNoErr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	run := reviewtypes.AgentRun{
		Name:   "codex",
		Status: reviewtypes.AgentStatusFailed,
		Err:    nil,
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if !strings.Contains(out, "**Failed**") {
		t.Errorf("expected bold Failed marker, got:\n%s", out)
	}
	// Must not contain "**Failed:** " which would indicate an error was printed.
	if strings.Contains(out, "**Failed:** ") {
		t.Errorf("unexpected error detail in output, got:\n%s", out)
	}
}

func TestDumpSink_FailedAgentWithProcessErrorRendersStderrAsCodeFence(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	stderr := "Error: API key invalid\nPlease set ANTHROPIC_API_KEY\nHint: see /docs/auth"
	pe := &reviewtypes.ProcessError{
		AgentName: "claude-code",
		Err:       errors.New("exit status 1"),
		Stderr:    stderr,
	}
	run := reviewtypes.AgentRun{
		Name:   "claude-code",
		Status: reviewtypes.AgentStatusFailed,
		Err:    pe,
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()

	// Failure header still mentions the exit error.
	if !strings.Contains(out, "exit status 1") {
		t.Errorf("expected exit status mention in failure header, got:\n%s", out)
	}
	// Each stderr line appears in output.
	for _, line := range []string{
		"Error: API key invalid",
		"Please set ANTHROPIC_API_KEY",
		"Hint: see /docs/auth",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("expected stderr line %q in output, got:\n%s", line, out)
		}
	}
	// Stderr is rendered in a fenced code block (```), not crammed into a
	// single inline-code span (`...`). The fence is the load-bearing fix:
	// multi-line stderr inside backticks loses newlines under markdown rendering.
	if !strings.Contains(out, "```") {
		t.Errorf("expected fenced code block delimiting stderr, got:\n%s", out)
	}
	// The whole error is NOT crammed into one inline-code span. The current
	// (broken) rendering produces a single backticked line containing the
	// full stderr; the fix moves stderr into its own fenced block, so the
	// header line itself does not contain "stderr:".
	if strings.Contains(out, "**Failed:** `claude-code: exit status 1: stderr:") {
		t.Errorf("stderr must not be jammed into the inline failure header, got:\n%s", out)
	}
}

func TestDumpSink_DoesNotDoublePrintSyntheticRunErrorMatchingRunErr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	// The orchestrator emits a synthetic RunError carrying waitErr so the live
	// TUI sees the failure mid-run. That same waitErr is also stored on
	// AgentRun.Err and rendered in the failure header (fenced stderr block).
	// The dump must not also render a "> agent error: <err>" blockquote for
	// the synthetic event — that produces visible duplication where the same
	// error text appears twice in adjacent output.
	waitErr := errors.New("exit status 1")
	run := reviewtypes.AgentRun{
		Name:   "claude-code",
		Status: reviewtypes.AgentStatusFailed,
		Err:    waitErr,
		Buffer: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Finished{Success: true},
			reviewtypes.RunError{Err: waitErr}, // synthetic, same pointer as run.Err
		},
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if strings.Contains(out, "> agent error:") {
		t.Errorf("synthetic RunError matching run.Err must not produce a blockquote, got:\n%s", out)
	}
	// Header should still be present.
	if !strings.Contains(out, "**Failed:**") {
		t.Errorf("expected failure header, got:\n%s", out)
	}
}

func TestDumpSink_FailedAgentRunErrorEvent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	run := reviewtypes.AgentRun{
		Name:   "codex",
		Status: reviewtypes.AgentStatusFailed,
		Err:    nil,
		Buffer: []reviewtypes.Event{
			reviewtypes.RunError{Err: errors.New("torn stdout stream")},
		},
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if !strings.Contains(out, "> agent error: `torn stdout stream`") {
		t.Errorf("expected blockquoted RunError detail, got:\n%s", out)
	}
}

func TestDumpSink_CancelledAgent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	run := reviewtypes.AgentRun{
		Name:   "gemini-cli",
		Status: reviewtypes.AgentStatusCancelled,
		Buffer: []reviewtypes.Event{
			reviewtypes.AssistantText{Text: "partial output"},
		},
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if !strings.Contains(out, "_cancelled_") {
		t.Errorf("expected italic cancelled marker, got:\n%s", out)
	}
	// Narrative should not be dumped for cancelled runs.
	if strings.Contains(out, "partial output") {
		t.Errorf("narrative should not appear for cancelled agent, got:\n%s", out)
	}
}

func TestDumpSink_Mixed(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	summary := makeSummary(
		reviewtypes.AgentRun{
			Name:   "claude-code",
			Status: reviewtypes.AgentStatusSucceeded,
			Buffer: []reviewtypes.Event{reviewtypes.AssistantText{Text: "looks good"}},
		},
		reviewtypes.AgentRun{
			Name:   "codex",
			Status: reviewtypes.AgentStatusFailed,
			Err:    errors.New("timeout"),
		},
		reviewtypes.AgentRun{
			Name:   "gemini-cli",
			Status: reviewtypes.AgentStatusCancelled,
		},
	)
	sink.RunFinished(summary)

	out := buf.String()
	if !strings.Contains(out, "# claude-code review") {
		t.Errorf("expected claude-code heading, got:\n%s", out)
	}
	if !strings.Contains(out, "# codex review") {
		t.Errorf("expected codex heading, got:\n%s", out)
	}
	if !strings.Contains(out, "# gemini-cli review") {
		t.Errorf("expected gemini-cli heading, got:\n%s", out)
	}
	if !strings.Contains(out, "3 agent(s) done — 1 succeeded, 1 failed, 1 cancelled") {
		t.Errorf("expected mixed counts line, got:\n%s", out)
	}
}

func TestDumpSink_EmptyAgentRuns(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	sink.RunFinished(reviewtypes.RunSummary{})

	out := buf.String()
	if !strings.Contains(out, "0 agent(s) done — 0 succeeded, 0 failed, 0 cancelled") {
		t.Errorf("expected empty counts line, got:\n%s", out)
	}
}
