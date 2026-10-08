package claudecode

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// collectInvocations walks every invocation, so it also asserts the scanner
// keeps going when visit declines.
func collectInvocations(t *testing.T, data string, hints [][]byte) []agent.ToolInvocation {
	t.Helper()
	var got []agent.ToolInvocation
	if found := (&ClaudeCodeAgent{}).ScanToolInvocations([]byte(data), hints, func(inv agent.ToolInvocation) bool {
		got = append(got, inv)
		return false
	}); found {
		t.Fatalf("ScanToolInvocations returned true although visit never accepted")
	}
	return got
}

func TestScanToolInvocations_BashAndSubagent(t *testing.T) {
	t.Parallel()

	data := `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"entire search foo --json"}}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"Agent","input":{"subagent_type":"entire-search","prompt":"look"}}]}}
`
	got := collectInvocations(t, data, nil)
	if len(got) != 2 {
		t.Fatalf("got %d invocations, want 2: %+v", len(got), got)
	}
	if got[0].Tool != "Bash" || got[0].Command != "entire search foo --json" {
		t.Errorf("first invocation = %+v", got[0])
	}
	if got[1].Tool != "Agent" || got[1].SubagentType != "entire-search" {
		t.Errorf("second invocation = %+v", got[1])
	}
}

// TestScanToolInvocations_SkipsToolResultLines pins the prefilter detail that
// removes the largest false-positive class: a tool_result line carries
// "tool_use_id", which must not read as a tool_use block.
func TestScanToolInvocations_SkipsToolResultLines(t *testing.T) {
	t.Parallel()

	data := `{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"entire search foo"}]}}
`
	if got := collectInvocations(t, data, nil); len(got) != 0 {
		t.Errorf("got %d invocations from a tool_result line, want 0: %+v", len(got), got)
	}
}

// TestScanToolInvocations_HonorsHints pins the documented contract that hints
// are a performance filter: a line the caller's hints exclude is never parsed,
// which is exactly why a caller must pass hints its own matcher implies.
func TestScanToolInvocations_HonorsHints(t *testing.T) {
	t.Parallel()

	data := `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"entire search foo"}}]}}
`
	if got := collectInvocations(t, data, [][]byte{[]byte("entire search")}); len(got) != 1 {
		t.Errorf("matching hint: got %d invocations, want 1", len(got))
	}
	if got := collectInvocations(t, data, [][]byte{[]byte("nonexistent-hint")}); len(got) != 0 {
		t.Errorf("non-matching hint: got %d invocations, want 0 (line must not be parsed)", len(got))
	}
}

func TestScanToolInvocations_StopsOnFirstAccept(t *testing.T) {
	t.Parallel()

	data := `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"first"}}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"second"}}]}}
`
	seen := 0
	found := (&ClaudeCodeAgent{}).ScanToolInvocations([]byte(data), nil, func(agent.ToolInvocation) bool {
		seen++
		return true
	})
	if !found {
		t.Error("ScanToolInvocations returned false although visit accepted")
	}
	if seen != 1 {
		t.Errorf("visit called %d times, want 1 (walk must stop on first accept)", seen)
	}
}

// TestScanToolInvocations_MalformedLinesAreSkipped covers a transcript read
// mid-write, which is a normal condition rather than an error: the agent owns
// the file and this is a telemetry read.
func TestScanToolInvocations_MalformedLinesAreSkipped(t *testing.T) {
	t.Parallel()

	data := `not json at all
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use"
{"type":"assistant"}

{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"entire search ok"}}]}}
`
	got := collectInvocations(t, data, nil)
	if len(got) != 1 {
		t.Fatalf("got %d invocations, want 1: %+v", len(got), got)
	}
	if got[0].Command != "entire search ok" {
		t.Errorf("Command = %q", got[0].Command)
	}
}

// TestScanToolInvocations_FinalLineWithoutNewline covers the hand-rolled line
// split: a transcript whose last line has no trailing newline must still be
// visited.
func TestScanToolInvocations_FinalLineWithoutNewline(t *testing.T) {
	t.Parallel()

	data := `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"entire search tail"}}]}}`
	got := collectInvocations(t, data, nil)
	if len(got) != 1 {
		t.Fatalf("got %d invocations, want 1: %+v", len(got), got)
	}
}

func TestScanToolInvocations_EmptyTranscript(t *testing.T) {
	t.Parallel()

	if got := collectInvocations(t, "", nil); len(got) != 0 {
		t.Errorf("got %d invocations from an empty transcript, want 0", len(got))
	}
}

// TestScanToolInvocations_SkipsNonAssistantEnvelopes pins the envelope gate:
// only assistant envelopes carry live tool calls, matching every other
// tool_use consumer in this package. A non-assistant envelope whose message
// decodes into tool_use-shaped content must not be visited.
func TestScanToolInvocations_SkipsNonAssistantEnvelopes(t *testing.T) {
	t.Parallel()

	data := `{"type":"summary","uuid":"s1","message":{"content":[{"type":"tool_use","id":"tx","name":"Bash","input":{"command":"entire search foo"}}]}}
`
	if got := collectInvocations(t, data, nil); len(got) != 0 {
		t.Errorf("got %d invocations from a non-assistant envelope, want 0: %+v", len(got), got)
	}
}
