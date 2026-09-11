package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
)

// Compile-time interface check.
var _ agent.TranscriptAnalyzer = (*CursorAgent)(nil)

// --- GetTranscriptPosition ---

func TestCursorAgent_GetTranscriptPosition(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	pos, err := ag.GetTranscriptPosition(path)
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 4 {
		t.Errorf("GetTranscriptPosition() = %d, want 4", pos)
	}
}

func TestCursorAgent_GetTranscriptPosition_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	pos, err := ag.GetTranscriptPosition("")
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 0 {
		t.Errorf("GetTranscriptPosition() = %d, want 0", pos)
	}
}

func TestCursorAgent_GetTranscriptPosition_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	pos, err := ag.GetTranscriptPosition("/nonexistent/path/transcript.jsonl")
	if err != nil {
		t.Fatalf("GetTranscriptPosition() error = %v", err)
	}
	if pos != 0 {
		t.Errorf("GetTranscriptPosition() = %d, want 0", pos)
	}
}

// --- ExtractPrompts ---

func TestCursorAgent_ExtractPrompts(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	prompts, err := ag.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("ExtractPrompts() returned %d prompts, want 2", len(prompts))
	}
	// Cursor's injected leading <timestamp> and the <user_query> wrapper are stripped
	// exactly once: the user's own pasted <timestamp>, exposed at the head of the
	// query by the unwrap, must survive (a second strip pass would eat it).
	if prompts[0] != "<timestamp>2026-01-01</timestamp> is my format, hello" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "<timestamp>2026-01-01</timestamp> is my format, hello")
	}
	if prompts[1] != "add 'one' to a file and commit" {
		t.Errorf("prompts[1] = %q, want %q", prompts[1], "add 'one' to a file and commit")
	}
}

func TestCursorAgent_ExtractPrompts_WithOffset(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	// Offset 2 skips the first user+assistant pair, leaving 1 user prompt
	prompts, err := ag.ExtractPrompts(path, 2)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("ExtractPrompts() returned %d prompts, want 1", len(prompts))
	}
	if prompts[0] != "add 'one' to a file and commit" {
		t.Errorf("prompts[0] = %q, want %q", prompts[0], "add 'one' to a file and commit")
	}
}

func TestCursorAgent_ExtractPrompts_EmptyFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}

	prompts, err := ag.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts() error = %v", err)
	}
	if len(prompts) != 0 {
		t.Errorf("ExtractPrompts() returned %d prompts, want 0", len(prompts))
	}
}

// --- ExtractSummary ---

func TestCursorAgent_ExtractSummary(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}
	if summary != "Created one.txt with one and committed." {
		t.Errorf("ExtractSummary() = %q, want %q", summary, "Created one.txt with one and committed.")
	}
}

func TestCursorAgent_ExtractSummary_EmptyFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("failed to write empty file: %v", err)
	}

	summary, err := ag.ExtractSummary(path)
	if err != nil {
		t.Fatalf("ExtractSummary() error = %v", err)
	}
	if summary != "" {
		t.Errorf("ExtractSummary() = %q, want empty string", summary)
	}
}

// --- ExtractModifiedFilesFromOffset ---

func TestCursorAgent_ExtractModifiedFilesFromOffset(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tmpDir := t.TempDir()
	path := writeSampleTranscript(t, tmpDir)

	// The sample is a text-only conversation, so there is nothing to attribute --
	// but the position must still advance to the sample's line count. Returning a
	// constant 0 here is what the removed stub did.
	files, pos, err := ag.ExtractModifiedFilesFromOffset(path, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v, want nil", err)
	}
	if files != nil {
		t.Errorf("ExtractModifiedFilesFromOffset() files = %v, want nil", files)
	}
	if want := len(sampleTranscriptLines()); pos != want {
		t.Errorf("ExtractModifiedFilesFromOffset() pos = %d, want %d", pos, want)
	}
}

// TestCursorAgent_ExtractModifiedFilesFromOffset_NonexistentFile pins the fail-open
// contract: Cursor reports transcript_path as null in CLI mode, so the resolved path
// may not exist yet on a capture path that must not fail.
func TestCursorAgent_ExtractModifiedFilesFromOffset_NonexistentFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	files, pos, err := ag.ExtractModifiedFilesFromOffset("/nonexistent/path.jsonl", 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v, want nil", err)
	}
	if files != nil {
		t.Errorf("ExtractModifiedFilesFromOffset() files = %v, want nil", files)
	}
	if pos != 0 {
		t.Errorf("ExtractModifiedFilesFromOffset() pos = %d, want 0", pos)
	}
}

func TestCursorAgent_ExtractModifiedFilesFromOffset_EmptyPath(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	files, pos, err := ag.ExtractModifiedFilesFromOffset("", 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
	}
	if files != nil {
		t.Errorf("ExtractModifiedFilesFromOffset() files = %v, want nil", files)
	}
	if pos != 0 {
		t.Errorf("ExtractModifiedFilesFromOffset() pos = %d, want 0", pos)
	}
}

// TestCursorAgent_MustNotImplementToolInvocationScanner pins that Cursor stays
// out of the tool-invocation probe until its shell-command input key is
// confirmed.
//
// Not because Cursor is unprobeable. It shares this JSONL shape and does record
// tool_use blocks; the "contains no tool_use blocks" claims elsewhere in this
// package date to 2026-03, are pinned by no test, and are stale — they also
// make ExtractModifiedFilesFromOffset give up entirely, which deserves its own
// look. The narrow blocker is that ToolInvocation.Command assumes Claude's
// `command` input key; guessing it for Cursor would produce the silent false
// negative the interface exists to prevent. Confirm the mapping, implement the
// scanner, and delete this test.
func TestCursorAgent_MustNotImplementToolInvocationScanner(t *testing.T) {
	t.Parallel()
	if _, ok := agent.AsToolInvocationScanner(&CursorAgent{}); ok {
		t.Fatal("CursorAgent must not implement ToolInvocationScanner until its shell-command " +
			"input key is confirmed: reusing Claude's `command` key would fabricate negatives")
	}
}

// --- Real captured session ---

// realSessionFixture is a verbatim 9-line excerpt of a real Cursor session,
// captured 2026-08-24 from
// ~/.cursor/projects/home-coder-src-entirecli/agent-transcripts/
//
//	7896c168-45c1-42d4-841a-c8c731c24f45/7896c168-45c1-42d4-841a-c8c731c24f45.jsonl
//
// (Cursor's nested IDE layout). Nothing was edited; the lines were selected to
// cover one call of each tool the session used, against a throwaway repo.
const realSessionFixture = "testdata/real_session_tool_use.jsonl"

// Line indices within the fixture (0-based), for offset assertions.
const (
	fixtureWriteLine      = 2 // Write notes.md
	fixtureStrReplaceLine = 4 // StrReplace notes.md
	fixtureTotalLines     = 9
)

const fixtureModifiedFile = "/tmp/cursor-probe/notes.md"

// TestCursorAgent_RealSessionContainsToolUseBlocks pins the premise, not the
// consequence.
//
// From 2026-03 to 2026-08 this package asserted in five places that "Cursor
// transcripts do not contain tool_use blocks", and ExtractModifiedFilesFromOffset
// returned nothing because of it. No test checked the claim -- the one test in the
// area asserted only that ModifiedFiles came back empty, which the stub guaranteed
// regardless of what Cursor actually wrote. This test reads the real transcript and
// fails if the tool_use blocks it depends on are absent, so the reverse mistake
// cannot go unnoticed either.
func TestCursorAgent_RealSessionContainsToolUseBlocks(t *testing.T) {
	t.Parallel()

	lines, err := transcript.ParseFromFileAtLine(realSessionFixture, 0)
	if err != nil {
		t.Fatalf("ParseFromFileAtLine() error = %v", err)
	}

	// Tool name -> the input keys observed for it in the real session.
	got := map[string]map[string]bool{}
	for i := range lines {
		if lines[i].Type != transcript.TypeAssistant {
			continue
		}
		var msg transcript.AssistantMessage
		if err := json.Unmarshal(lines[i].Message, &msg); err != nil {
			t.Fatalf("unmarshal assistant message: %v", err)
		}
		for _, block := range msg.Content {
			if block.Type != transcript.ContentTypeToolUse {
				continue
			}
			var input map[string]json.RawMessage
			if err := json.Unmarshal(block.Input, &input); err != nil {
				t.Fatalf("unmarshal tool input for %q: %v", block.Name, err)
			}
			if got[block.Name] == nil {
				got[block.Name] = map[string]bool{}
			}
			for k := range input {
				got[block.Name][k] = true
			}
		}
	}

	if len(got) == 0 {
		t.Fatal("real Cursor transcript contains no tool_use blocks: the 2026-03 claim " +
			"would be true again and ExtractModifiedFiles cannot work -- re-verify before " +
			"changing the implementation")
	}

	// Every tool this package reasons about, with the input key it is read through.
	for _, want := range []struct {
		tool string
		key  string
	}{
		{"Write", "path"},      // creates/overwrites -- FileModificationTools
		{"StrReplace", "path"}, // in-place edit -- FileModificationTools
		{"Read", "path"},
		{"Grep", "pattern"},
		{"Glob", "glob_pattern"},
		{"Shell", "command"}, // see AGENT.md: unblocks a tool-invocation scanner
	} {
		keys, ok := got[want.tool]
		if !ok {
			t.Errorf("tool %q absent from fixture; tools present: %v", want.tool, mapKeys(got))
			continue
		}
		if !keys[want.key] {
			t.Errorf("tool %q input lacks key %q, has %v", want.tool, want.key, mapKeys(keys))
		}
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCursorAgent_ExtractModifiedFilesFromOffset_RealSession(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	files, pos, err := ag.ExtractModifiedFilesFromOffset(realSessionFixture, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
	}
	// Write and StrReplace both target notes.md; the result is deduplicated.
	want := []string{fixtureModifiedFile}
	if !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	if pos != fixtureTotalLines {
		t.Errorf("pos = %d, want %d", pos, fixtureTotalLines)
	}
}

func TestCursorAgent_ExtractModifiedFilesFromOffset_RealSessionOffsets(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	tests := []struct {
		name      string
		offset    int
		wantFiles []string
	}{
		{"from Write", fixtureWriteLine, []string{fixtureModifiedFile}},
		{"after Write, from StrReplace", fixtureStrReplaceLine, []string{fixtureModifiedFile}},
		{"after both writes", fixtureStrReplaceLine + 1, nil},
		{"past end", fixtureTotalLines, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			files, pos, err := ag.ExtractModifiedFilesFromOffset(realSessionFixture, tt.offset)
			if err != nil {
				t.Fatalf("ExtractModifiedFilesFromOffset() error = %v", err)
			}
			if !slices.Equal(files, tt.wantFiles) {
				t.Errorf("files = %v, want %v", files, tt.wantFiles)
			}
			// The position is the whole file's line count, never the slice's.
			if pos != fixtureTotalLines {
				t.Errorf("pos = %d, want %d", pos, fixtureTotalLines)
			}
		})
	}
}

// TestCursorAgent_ExtractModifiedFiles_IgnoresReadOnlyTools pins that a session
// which only read and searched attributes no files, so Read/Grep/Glob/Shell cannot
// drift into FileModificationTools unnoticed.
func TestCursorAgent_ExtractModifiedFiles_IgnoresReadOnlyTools(t *testing.T) {
	t.Parallel()

	lines, err := transcript.ParseFromFileAtLine(realSessionFixture, fixtureStrReplaceLine+1)
	if err != nil {
		t.Fatalf("ParseFromFileAtLine() error = %v", err)
	}
	if files := ExtractModifiedFiles(lines); files != nil {
		t.Errorf("ExtractModifiedFiles() = %v, want nil (Glob/Grep/Shell modify nothing we can attribute)", files)
	}
}

func TestReadSession_PopulatesModifiedFilesFromRealSession(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}

	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "sess-real",
		SessionRef: realSessionFixture,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}
	want := []string{fixtureModifiedFile}
	if !slices.Equal(session.ModifiedFiles, want) {
		t.Errorf("ModifiedFiles = %v, want %v", session.ModifiedFiles, want)
	}
}
