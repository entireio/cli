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
	if !strings.Contains(out, "exit status 1") {
		t.Errorf("expected exit status mention in failure header, got:\n%s", out)
	}
	for _, line := range []string{
		"Error: API key invalid",
		"Please set ANTHROPIC_API_KEY",
		"Hint: see /docs/auth",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("expected stderr line %q in output, got:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "```\n"+stderr+"\n```") {
		t.Errorf("expected stderr in fenced code block, got:\n%s", out)
	}
	if strings.Contains(out, "**Failed:** `claude-code: exit status 1: stderr:") {
		t.Errorf("stderr must not be jammed into the inline failure header, got:\n%s", out)
	}
}

func TestDumpSink_DoesNotDoublePrintSyntheticRunErrorMatchingRunErr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	waitErr := errors.New("exit status 1")
	run := reviewtypes.AgentRun{
		Name:   "claude-code",
		Status: reviewtypes.AgentStatusFailed,
		Err:    waitErr,
		Buffer: []reviewtypes.Event{
			reviewtypes.Started{},
			reviewtypes.Finished{Success: true},
			reviewtypes.RunError{Err: waitErr},
		},
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if strings.Contains(out, "> agent error:") {
		t.Errorf("synthetic RunError matching run.Err must not produce a blockquote, got:\n%s", out)
	}
	if !strings.Contains(out, "**Failed:**") {
		t.Errorf("expected failure header, got:\n%s", out)
	}
}

func TestDumpSink_DoesNotDoublePrintRunErrorWrappedByRunErr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	streamErr := errors.New("torn stdout stream")
	run := reviewtypes.AgentRun{
		Name:   "claude-code",
		Status: reviewtypes.AgentStatusFailed,
		Err:    agentRunFailureError("claude-code", streamErr),
		Buffer: []reviewtypes.Event{
			reviewtypes.RunError{Err: streamErr},
		},
	}
	sink.RunFinished(makeSummary(run))

	out := buf.String()
	if strings.Contains(out, "> agent error:") {
		t.Errorf("RunError wrapped by run.Err must not be printed again, got:\n%s", out)
	}
	if !strings.Contains(out, "review agent claude-code reported failure: torn stdout stream") {
		t.Errorf("expected wrapped failure header, got:\n%s", out)
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

// TestDumpSink_FenceEscapesBackticksInStderr verifies that stderr containing
// a 3-backtick line does not terminate the surrounding code fence early.
// Per CommonMark §4.5 the closing fence must be at least as long as the
// opening fence, so the fence has to widen to one more backtick than the
// longest run in the content.
func TestDumpSink_FenceEscapesBackticksInStderr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	sink := DumpSink{W: &buf}

	stderr := "before\n```\ninner content\n```\nafter"
	pe := &reviewtypes.ProcessError{
		AgentName: "claude-code",
		Err:       errors.New("exit status 1"),
		Stderr:    stderr,
	}
	sink.RunFinished(makeSummary(reviewtypes.AgentRun{
		Name:   "claude-code",
		Status: reviewtypes.AgentStatusFailed,
		Err:    pe,
	}))

	out := buf.String()
	// Fence must widen to 4 backticks, with the full stderr (including the
	// embedded 3-backtick lines) sitting verbatim inside.
	wantFence := "````\n" + stderr + "\n````"
	if !strings.Contains(out, wantFence) {
		t.Errorf("expected widened fence around stderr, got:\n%s", out)
	}
	// "after" must still be inside the fence (i.e., immediately followed by
	// the closing fence), not orphaned outside.
	if !strings.Contains(out, "after\n````") {
		t.Errorf("trailing stderr content must remain inside the fence, got:\n%s", out)
	}
}

func TestCodeFenceFor_MinimumThreeBackticks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "```"},
		{"no backticks", "hello world", "```"},
		{"single backtick", "use `x` here", "```"},
		{"two backticks", "matched ``code`` style", "```"},
		{"three backticks on a line", "```\nfenced\n```", "````"},
		{"four backticks", "````", "`````"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codeFenceFor(tc.in); got != tc.want {
				t.Errorf("codeFenceFor(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
