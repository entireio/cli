package compact

import (
	"strings"
	"testing"

	"github.com/entireio/cli/redact"
)

// assertSliceMatchesDelta asserts that nonEmptyLines(full)[boundary:] equals the
// independently-compacted delta. This is the core CompactFull invariant: a reader
// that stores the full compact transcript and slices from boundary recovers
// exactly this checkpoint's content. Holds exactly when no single logical message
// straddles the StartLine boundary (the documented off-by-one case).
func assertSliceMatchesDelta(t *testing.T, full []byte, boundary int, delta []byte) {
	t.Helper()
	fullLines := nonEmptyLines(full)
	if boundary < 0 || boundary > len(fullLines) {
		t.Fatalf("boundary %d out of range for %d full lines", boundary, len(fullLines))
	}
	sliced := strings.Join(fullLines[boundary:], "\n")
	assertJSONLines(t, []byte(sliced), nonEmptyLines(delta))
}

func TestCompactFull_ClaudeJSONL_FullPlusBoundary(t *testing.T) {
	t.Parallel()

	input := redact.AlreadyRedacted([]byte(fixtureFullJSONL))
	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 3}

	full, boundary, err := FullWithBoundary(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Full output is the entire compacted session (4 lines), regardless of StartLine.
	expectedFull := []string{
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:00:00Z","content":[{"text":"hello"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:00:01Z","id":"msg-1","content":[{"type":"text","text":"Hi there!"},{"type":"tool_use","id":"tu-1","name":"Bash","input":{"command":"ls"},"result":{"output":"file1.txt\nfile2.txt","status":"success","file":{"filePath":"/repo/file1.txt","numLines":10}}}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"user","ts":"2026-01-01T00:01:00Z","content":[{"text":"now fix the bug"}]}`,
		`{"v":1,"agent":"claude-code","cli_version":"0.5.1","type":"assistant","ts":"2026-01-01T00:01:01Z","id":"msg-2","content":[{"type":"text","text":"I found the issue."},{"type":"tool_use","id":"tu-2","name":"Edit","input":{"file_path":"/repo/bug.go","old_string":"bad","new_string":"good"}}]}`,
	}
	assertJSONLines(t, full, expectedFull)

	// StartLine=3 lands on the second user turn → its compact slice begins at
	// full line 2 (user "now fix the bug").
	if boundary != 2 {
		t.Fatalf("boundary: got %d, want 2", boundary)
	}

	delta, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("delta compact error: %v", err)
	}
	assertSliceMatchesDelta(t, full, boundary, delta)
}

func TestCompactFull_StartLineZero_BoundaryZero(t *testing.T) {
	t.Parallel()

	input := redact.AlreadyRedacted([]byte(fixtureFullJSONL))

	full, boundary, err := FullWithBoundary(input, defaultOpts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if boundary != 0 {
		t.Fatalf("boundary: got %d, want 0", boundary)
	}

	// With StartLine=0, the full output is identical to a plain Compact.
	plain, err := Compact(input, defaultOpts)
	if err != nil {
		t.Fatalf("plain compact error: %v", err)
	}
	assertJSONLines(t, full, nonEmptyLines(plain))
}

func TestCompactFull_StartLineBeyondEnd_BoundaryAtEnd(t *testing.T) {
	t.Parallel()

	input := redact.AlreadyRedacted([]byte(fixtureFullJSONL))
	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 1000}

	full, boundary, err := FullWithBoundary(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The checkpoint added nothing past the end: its slice is empty, so the
	// boundary sits at the final line and full[boundary:] is empty.
	fullLines := nonEmptyLines(full)
	if boundary != len(fullLines) {
		t.Fatalf("boundary: got %d, want %d (all lines before this checkpoint)", boundary, len(fullLines))
	}
	if got := fullLines[boundary:]; len(got) != 0 {
		t.Fatalf("expected empty slice past boundary, got %d lines", len(got))
	}
}

// TestFullWithBoundary_StraddlingAssistantFragments_RoundsToInclusion pins the
// documented behavior when StartLine falls between two same-ID streaming
// assistant fragments: compaction merges them into one line, which no integer
// boundary can split. The boundary rounds to inclusion (0 here), so the merged
// line — carrying both the pre-start and post-start fragment — stays in the
// slice. This never drops this checkpoint's content (FRAG_B), at the cost of
// the slice head repeating one merged line from the previous checkpoint (FRAG_A).
func TestFullWithBoundary_StraddlingAssistantFragments_RoundsToInclusion(t *testing.T) {
	t.Parallel()

	input := redact.AlreadyRedacted([]byte(
		`{"type":"assistant","timestamp":"t0","message":{"id":"msg_1","content":[{"type":"text","text":"FRAG_A"}]}}
{"type":"assistant","timestamp":"t1","message":{"id":"msg_1","content":[{"type":"text","text":"FRAG_B"}]}}
`))
	// StartLine=1 lands between the two fragments of the same streaming message.
	opts := MetadataFields{Agent: "claude-code", CLIVersion: "0.5.1", StartLine: 1}

	full, boundary, err := FullWithBoundary(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The two fragments merge into a single compact line.
	fullLines := nonEmptyLines(full)
	if len(fullLines) != 1 {
		t.Fatalf("expected 1 merged compact line, got %d:\n%s", len(fullLines), full)
	}
	// Rounds to inclusion: the merged line stays in the slice.
	if boundary != 0 {
		t.Fatalf("boundary: got %d, want 0 (merged straddling line included)", boundary)
	}
	// The slice retains this checkpoint's content (FRAG_B) and, unavoidably, the
	// pre-start fragment (FRAG_A) merged into the same line.
	slice := strings.Join(fullLines[boundary:], "\n")
	if !strings.Contains(slice, "FRAG_B") {
		t.Errorf("slice dropped this checkpoint's content FRAG_B:\n%s", slice)
	}
	if !strings.Contains(slice, "FRAG_A") {
		t.Errorf("expected merged line to retain FRAG_A (inclusive rounding):\n%s", slice)
	}
}

func TestCompactFull_GeminiIndexFormat_Boundary(t *testing.T) {
	t.Parallel()

	input := redact.AlreadyRedacted([]byte(`{
		"sessionId": "s1",
		"messages": [
			{"id":"m1","timestamp":"2026-01-01T00:00:00Z","type":"user","content":"hello"},
			{"id":"m2","timestamp":"2026-01-01T00:00:01Z","type":"gemini","content":"hi there","tokens":{"input":10,"output":5}},
			{"id":"m3","timestamp":"2026-01-01T00:00:02Z","type":"user","content":"bye"}
		]
	}`))
	// Gemini treats StartLine as a message-index offset; skipping 1 message.
	opts := MetadataFields{Agent: "gemini-cli", CLIVersion: "0.5.1", StartLine: 1}

	full, boundary, err := FullWithBoundary(input, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Full = 3 compact lines; skipping message 0 (user "hello") → boundary 1.
	if got := len(nonEmptyLines(full)); got != 3 {
		t.Fatalf("full lines: got %d, want 3", got)
	}
	if boundary != 1 {
		t.Fatalf("boundary: got %d, want 1", boundary)
	}

	delta, err := Compact(input, opts)
	if err != nil {
		t.Fatalf("delta compact error: %v", err)
	}
	assertSliceMatchesDelta(t, full, boundary, delta)
}
