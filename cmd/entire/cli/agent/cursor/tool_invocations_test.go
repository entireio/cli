package cursor

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Cursor-shaped transcript lines. The envelope key is "role", not "type" —
// that difference is the whole reason these fixtures cannot be borrowed from
// claudecode, and the reason a walker that skips normalization sees nothing at
// all in a Cursor transcript.
const (
	cursorShellSearch = `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"entire search \"retry backoff\" --json","description":"search history"}}]}}
`
	cursorShellUnrelated = `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"git status --short"}}]}}
`
	cursorWriteBlock = `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"path":"/tmp/x/notes.md","contents":"line 1"}}]}}
`
	// A tool result: carries "tool_use_id", never "tool_use", and rides a
	// non-assistant envelope. Both prefilters must reject it.
	cursorToolResult = `{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"c1","content":"$ entire search foo\nno results"}]}}
`
	// Cursor closes a turn with an envelope that has a type and no message.
	cursorTurnEnded = `{"type":"turn_ended","status":"success"}
`
)

// TestCursorAgent_ImplementsToolInvocationScanner is the inverse of the
// TestCursorAgent_MustNotImplementToolInvocationScanner this replaces. That
// test held Cursor out of the probe while its shell-command input key was
// unconfirmed; the key is confirmed (`Shell` keys it `command`, verified
// against the real session in TestCursorAgent_RealSessionContainsToolUseBlocks),
// so the capability is now the thing worth pinning. Dropping the interface
// would silently downgrade every Cursor session to "cannot tell".
func TestCursorAgent_ImplementsToolInvocationScanner(t *testing.T) {
	t.Parallel()
	if _, ok := agent.AsToolInvocationScanner(&CursorAgent{}); !ok {
		t.Fatal("CursorAgent must implement ToolInvocationScanner: its shell-command input key " +
			"is confirmed, and dropping it reports every Cursor session as unsupported")
	}
}

// TestCursorAgent_ScanToolInvocations_RoleEnvelope is the Cursor-specific
// regression. Cursor keys the envelope "role"; Claude Code keys it "type". A
// walker that gates on Type without normalizing Role first returns false for
// every line of every Cursor transcript — a total, silent miss that still
// reports as a confident "did not run", since the agent implements the
// interface and is therefore counted as measurable.
func TestCursorAgent_ScanToolInvocations_RoleEnvelope(t *testing.T) {
	t.Parallel()

	var got []agent.ToolInvocation
	found := (&CursorAgent{}).ScanToolInvocations([]byte(cursorShellSearch), nil, func(inv agent.ToolInvocation) bool {
		got = append(got, inv)
		return false
	})

	if found {
		t.Error("ScanToolInvocations() = true, want false (visitor never accepted)")
	}
	if len(got) != 1 {
		t.Fatalf("visited %d invocations, want 1: %+v", len(got), got)
	}
	if got[0].Tool != "Shell" {
		t.Errorf("Tool = %q, want %q", got[0].Tool, "Shell")
	}
	if got[0].Command != `entire search "retry backoff" --json` {
		t.Errorf("Command = %q, want the shell command verbatim", got[0].Command)
	}
}

// TestCursorAgent_ScanToolInvocations_CommandKeyIsPopulated pins the one thing
// that blocked this implementation: that reading Cursor's shell command through
// ToolInvocation.Command yields the command rather than an empty string. An
// empty Command here is the exact false negative the interface exists to
// prevent — it reads downstream as "this session ran no such command".
func TestCursorAgent_ScanToolInvocations_CommandKeyIsPopulated(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(realSessionFixture)
	if err != nil {
		t.Fatalf("read %s: %v", realSessionFixture, err)
	}

	shellCommands := 0
	(&CursorAgent{}).ScanToolInvocations(data, nil, func(inv agent.ToolInvocation) bool {
		if inv.Tool != "Shell" {
			return false
		}
		shellCommands++
		if inv.Command == "" {
			t.Errorf("Shell invocation carries an empty Command; the `command` input key is not being read")
		}
		return false
	})

	// The fixture records two Shell calls, at lines 1 and 7.
	if shellCommands != 2 {
		t.Errorf("saw %d Shell invocations in the real session, want 2", shellCommands)
	}
}

// TestCursorAgent_ScanToolInvocations_RealSessionToolNames walks the captured
// session and pins the tool names reaching a caller, so a Cursor rename shows
// up here rather than as a quiet drop in whatever the caller matches on.
func TestCursorAgent_ScanToolInvocations_RealSessionToolNames(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(realSessionFixture)
	if err != nil {
		t.Fatalf("read %s: %v", realSessionFixture, err)
	}

	seen := map[string]bool{}
	(&CursorAgent{}).ScanToolInvocations(data, nil, func(inv agent.ToolInvocation) bool {
		seen[inv.Tool] = true
		return false
	})

	for _, want := range []string{"Shell", "Write", "StrReplace", "Read", "Glob", "Grep"} {
		if !seen[want] {
			t.Errorf("tool %q not visited; saw %v", want, seen)
		}
	}
}

// TestCursorAgent_ScanToolInvocations_SkipsToolResultAndNonAssistant covers the
// two shapes that must never count: a tool_result line (whose "tool_use_id" is
// the largest false-positive class for any substring probe) and a
// non-assistant envelope.
func TestCursorAgent_ScanToolInvocations_SkipsToolResultAndNonAssistant(t *testing.T) {
	t.Parallel()

	data := []byte(cursorToolResult + cursorTurnEnded)
	visited := 0
	found := (&CursorAgent{}).ScanToolInvocations(data, nil, func(agent.ToolInvocation) bool {
		visited++
		return true
	})

	if found || visited != 0 {
		t.Errorf("ScanToolInvocations() = %v after %d visits, want false after 0", found, visited)
	}
}

// TestCursorAgent_ScanToolInvocations_HonorsHints pins the prefilter as a
// performance filter: a line containing none of the hints is skipped, and nil
// hints visit everything.
func TestCursorAgent_ScanToolInvocations_HonorsHints(t *testing.T) {
	t.Parallel()

	data := []byte(cursorShellUnrelated + cursorShellSearch)

	visited := 0
	found := (&CursorAgent{}).ScanToolInvocations(data, [][]byte{[]byte("entire search")}, func(inv agent.ToolInvocation) bool {
		visited++
		return inv.Command == `entire search "retry backoff" --json`
	})
	if !found {
		t.Error("ScanToolInvocations() = false, want true (the hinted line carries the invocation)")
	}
	if visited != 1 {
		t.Errorf("visited %d invocations with a hint, want 1 (the unrelated line must be skipped)", visited)
	}

	visited = 0
	(&CursorAgent{}).ScanToolInvocations(data, nil, func(agent.ToolInvocation) bool {
		visited++
		return false
	})
	if visited != 2 {
		t.Errorf("visited %d invocations with nil hints, want 2", visited)
	}
}

// TestCursorAgent_ScanToolInvocations_StopsOnFirstAccept pins that the walk
// short-circuits, so a caller answering "did this happen at all" does not pay
// for the rest of a multi-megabyte transcript.
func TestCursorAgent_ScanToolInvocations_StopsOnFirstAccept(t *testing.T) {
	t.Parallel()

	data := []byte(cursorShellSearch + cursorWriteBlock + cursorShellUnrelated)
	visited := 0
	found := (&CursorAgent{}).ScanToolInvocations(data, nil, func(agent.ToolInvocation) bool {
		visited++
		return true
	})

	if !found {
		t.Error("ScanToolInvocations() = false, want true")
	}
	if visited != 1 {
		t.Errorf("visited %d invocations, want 1 (walk must stop on the first accept)", visited)
	}
}

// TestCursorAgent_ScanToolInvocations_MalformedAndEmpty covers the fail-open
// paths: a half-written line (the transcript is written by another process) and
// an empty transcript must neither panic nor abort the walk.
func TestCursorAgent_ScanToolInvocations_MalformedAndEmpty(t *testing.T) {
	t.Parallel()

	truncated := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","inp`
	data := []byte(truncated + "\n" + cursorShellSearch)

	visited := 0
	found := (&CursorAgent{}).ScanToolInvocations(data, nil, func(agent.ToolInvocation) bool {
		visited++
		return true
	})
	if !found || visited != 1 {
		t.Errorf("ScanToolInvocations() = %v after %d visits, want true after 1 (malformed line skipped, walk continues)", found, visited)
	}

	if (&CursorAgent{}).ScanToolInvocations(nil, nil, func(agent.ToolInvocation) bool { return true }) {
		t.Error("ScanToolInvocations(nil) = true, want false")
	}
}

// TestCursorAgent_ScanToolInvocations_FinalLineWithoutNewline pins that the
// hand-rolled line split does not drop a transcript's last line — Cursor writes
// the file incrementally, so the newest invocation is routinely the unterminated
// one.
func TestCursorAgent_ScanToolInvocations_FinalLineWithoutNewline(t *testing.T) {
	t.Parallel()

	data := []byte(cursorWriteBlock + `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"entire search foo"}}]}}`)

	if !(&CursorAgent{}).ScanToolInvocations(data, nil, func(inv agent.ToolInvocation) bool {
		return inv.Command == "entire search foo"
	}) {
		t.Error("ScanToolInvocations() = false, want true (final line without a trailing newline must be walked)")
	}
}
