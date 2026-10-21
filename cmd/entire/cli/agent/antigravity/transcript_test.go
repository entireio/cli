package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestChunkAndReassemble_RoundTrip(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	original := []byte(`{"role":"user","content":"hi"}` + "\n" + `{"role":"assistant","content":"hello"}` + "\n")
	chunks, err := a.ChunkTranscript(context.Background(), original, 1024)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, original) {
		t.Errorf("round-trip mismatch:\n  in:  %q\n  out: %q", original, out)
	}
}

// brainTranscriptPath points the agent's session store (GetSessionDir) at a
// fresh brain directory and returns a transcript path inside it, in agy's
// layout. PrepareTranscript materialises a placeholder only inside that store
// (a write on a hook-supplied path), so every test that expects one has to
// place the path where agy would. Uses t.Setenv, so callers cannot t.Parallel.
func brainTranscriptPath(t *testing.T) string {
	t.Helper()
	brain := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "brain")
	t.Setenv(antigravityTestBrainDirEnv, brain)
	return (&AntigravityAgent{}).ResolveSessionFile(brain, "conv")
}

// TestPrepareTranscript_AbsentFileCreatesPlaceholder verifies the
// TranscriptPreparer creates an empty file when agy hasn't flushed its
// transcript yet (the common case at Stop hook time). Without this, the
// framework's fileExists check in handleLifecycleTurnEnd would fail and our
// hook would exit non-zero, aborting agy's turn.
func TestPrepareTranscript_AbsentFileCreatesPlaceholder(t *testing.T) {
	// Non-existent parent dirs (the brain directory itself included) also
	// exercise directory creation through the store.
	path := brainTranscriptPath(t)
	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("placeholder not created: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("placeholder size = %d, want 0 (empty)", info.Size())
	}
}

// TestPrepareTranscript_PresentFilePreserved verifies PrepareTranscript leaves
// an already-written transcript untouched. This is the case when agy's writer
// races ahead of the Stop hook.
func TestPrepareTranscript_PresentFilePreserved(t *testing.T) {
	path := brainTranscriptPath(t)
	original := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE"}` + "\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("PrepareTranscript should not overwrite an existing transcript\n  before: %q\n  after:  %q", original, got)
	}
}

func TestPrepareTranscript_WaitsForDelayedTranscript(t *testing.T) {
	path := brainTranscriptPath(t)
	original := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE"}` + "\n")

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		writeErr <- os.WriteFile(path, original, 0o600)
	}()

	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err != nil {
		t.Fatalf("PrepareTranscript: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("delayed write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("PrepareTranscript() should wait for delayed transcript, got %q want %q", got, original)
	}
}

// TestPrepareTranscript_RefusesPathOutsideBrainDir: the placeholder is a write
// on a hook-supplied path, so it lands inside agy's brain directory or nowhere.
// Anchoring on the path's own parent instead would contain nothing.
func TestPrepareTranscript_RefusesPathOutsideBrainDir(t *testing.T) {
	brainTranscriptPath(t) // pins the store somewhere else
	outside := filepath.Join(t.TempDir(), "elsewhere", "transcript_full.jsonl")

	a := &AntigravityAgent{}
	err := a.PrepareTranscript(context.Background(), outside)
	if !errors.Is(err, agent.ErrOutsideSessionStore) {
		t.Fatalf("PrepareTranscript() error = %v, want agent.ErrOutsideSessionStore", err)
	}
	if _, statErr := os.Lstat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("a placeholder must not be created outside the store; Lstat err = %v", statErr)
	}
}

// TestPrepareTranscript_EmptyRefIsNoOp verifies an empty transcript path is
// a graceful no-op (defensive — the framework probably never passes empty,
// but agents have been bitten by empty refs in the past).
func TestPrepareTranscript_EmptyRefIsNoOp(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), ""); err != nil {
		t.Errorf("PrepareTranscript(\"\") should not error, got %v", err)
	}
}

func TestExtractPrompts_StripsUserRequestWrapper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nread a.txt and exit\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: x.\n</ADDITIONAL_METADATA>"}`,
		`{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE"}`,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"ok"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	prompts, err := a.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("want 1 prompt, got %d: %#v", len(prompts), prompts)
	}
	if prompts[0] != "read a.txt and exit" {
		t.Errorf("want stripped request, got %q", prompts[0])
	}
}

func TestExtractPrompts_RespectsOffsetAndMissingFile(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	got, err := a.ExtractPrompts(filepath.Join(t.TempDir(), "nope.jsonl"), 0)
	if err != nil || got != nil {
		t.Fatalf("missing file: want (nil,nil), got (%#v,%v)", got, err)
	}
}

func TestExtractPrompts_RealFixture(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	prompts, err := a.ExtractPrompts("testdata/transcript_sample.jsonl", 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0] != "read a.txt and tell me what it says, then exit" {
		t.Fatalf("unexpected prompts: %#v", prompts)
	}
}

func TestExtractPrompts_SkipsLinesAtOrBelowOffset(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>first</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","content":"ok"}`,
		`{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>second</USER_REQUEST>"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}

	// offset 0 → both prompts
	all, err := a.ExtractPrompts(path, 0)
	if err != nil {
		t.Fatalf("ExtractPrompts(0): %v", err)
	}
	if len(all) != 2 || all[0] != "first" || all[1] != "second" {
		t.Fatalf("offset 0: want [first second], got %#v", all)
	}

	// offset 1 → first non-blank line consumed, so only the second USER_INPUT remains
	rest, err := a.ExtractPrompts(path, 1)
	if err != nil {
		t.Fatalf("ExtractPrompts(1): %v", err)
	}
	if len(rest) != 1 || rest[0] != "second" {
		t.Fatalf("offset 1: want [second], got %#v", rest)
	}
}

func TestGetTranscriptPosition_CountsLines(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	pos, err := a.GetTranscriptPosition("testdata/transcript_sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if pos <= 0 {
		t.Fatalf("want > 0 lines, got %d", pos)
	}
	if p, e := a.GetTranscriptPosition(filepath.Join(t.TempDir(), "no.jsonl")); p != 0 || e != nil {
		t.Fatalf("missing: want (0,nil) got (%d,%v)", p, e)
	}
}

func TestExtractModifiedFiles_FromToolCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"\"/repo/a.txt\"","Overwrite":"true"}}]}`,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"replace_file_content","args":{"TargetFile":"\"/repo/b.txt\""}}]}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"list_dir","args":{"DirectoryPath":"\"/repo\""}}]}`,
		// Re-mutate /repo/a.txt on a later step: must be deduplicated, not double-counted.
		`{"step_index":4,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"\"/repo/a.txt\"","Overwrite":"true"}}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	files, pos, err := a.ExtractModifiedFilesFromOffset(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 5 {
		t.Errorf("want pos 5, got %d", pos)
	}
	want := map[string]bool{"/repo/a.txt": true, "/repo/b.txt": true}
	if len(files) != 2 {
		t.Fatalf("want 2 modified files (deduped), got %#v", files)
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected modified file %q", f)
		}
	}
}

// TestExtractModifiedFiles_PathConvention pins the path convention: the
// analyzer returns ABSOLUTE, symlink-resolved paths (the same shape
// lifecycle.go's parsePreToolUse records into FilesTouched). The framework
// relativizes downstream via FilterAndNormalizePaths -> paths.ToRelativePath
// against the worktree root, so returning absolute here is correct and must
// NOT be pre-relativized. This test creates a real file under a temp dir and
// asserts the returned path is the absolute, symlink-resolved location.
func TestExtractModifiedFiles_PathConvention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Resolve symlinks on the temp dir itself (macOS /tmp -> /private/tmp) so
	// our expectation matches what resolveAgySymlinks produces.
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(resolvedDir, "sub", "real.txt")
	if mkErr := os.MkdirAll(filepath.Dir(target), 0o750); mkErr != nil {
		t.Fatal(mkErr)
	}
	if wErr := os.WriteFile(target, []byte("x"), 0o600); wErr != nil {
		t.Fatal(wErr)
	}

	transcript := filepath.Join(dir, "t.jsonl")
	// Note the double-encoded TargetFile arg, mirroring agy's wire format.
	line := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"write_to_file","args":{"TargetFile":` +
		jsonQuote(t, jsonQuote(t, target)) + `,"Overwrite":"true"}}]}`
	if wErr := os.WriteFile(transcript, []byte(line+"\n"), 0o600); wErr != nil {
		t.Fatal(wErr)
	}

	a := &AntigravityAgent{}
	files, _, err := a.ExtractModifiedFilesFromOffset(context.Background(), transcript, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %#v", files)
	}
	if !filepath.IsAbs(files[0]) {
		t.Errorf("expected an absolute path, got %q", files[0])
	}
	if files[0] != target {
		t.Errorf("want absolute symlink-resolved path %q, got %q", target, files[0])
	}
}

// jsonQuote returns s wrapped as a JSON string literal (used to build the
// double-encoded TargetFile arg in the path-convention test).
func jsonQuote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("jsonQuote(%q): %v", s, err)
	}
	return string(b)
}

// TestExtractModifiedFiles_TruncatedStepDoesNotPanicAndKeepsOthers pins the
// degrade path for agy's `truncated_fields`: a mutating tool call whose
// TargetFile was trimmed away (observed live, ~0.8% of replace_file_content
// calls in a daily-driver corpus) must not abort extraction or poison the
// other files in the same range. The dropped call is reported via a WARN log
// (not asserted here; the logging package has no capture hook) — the
// contract this test locks in is "no error, other files intact, position
// still advances".
func TestExtractModifiedFiles_TruncatedStepDoesNotPanicAndKeepsOthers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"<USER_REQUEST>go</USER_REQUEST>"}`,
		// TargetFile trimmed by agy; every other arg survived.
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","truncated_fields":["tool_calls[0].args.TargetFile"],"tool_calls":[{"name":"replace_file_content","args":{"ReplacementChunks":"\"[]\"","TargetContent":"\"x\""}}]}`,
		`{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"\"/repo/b.txt\""}}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	files, pos, err := a.ExtractModifiedFilesFromOffset(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("truncated step must degrade, not error: %v", err)
	}
	if pos != 3 {
		t.Errorf("want pos 3, got %d", pos)
	}
	if len(files) != 1 || files[0] != "/repo/b.txt" {
		t.Fatalf("want only the intact file, got %#v", files)
	}
}

func TestAgyStepTruncated(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		``:                                  false,
		`null`:                              false,
		`[]`:                                false,
		`["tool_calls[0].args.TargetFile"]`: true,
		`{"tool_calls":["TargetFile"]}`:     true,
	}
	for raw, want := range cases {
		step := agyStep{TruncatedFields: json.RawMessage(raw)}
		if got := step.truncated(); got != want {
			t.Errorf("truncated_fields=%s: truncated() = %v, want %v", raw, got, want)
		}
	}
}

// A cancelled context must still leave the placeholder behind: the lifecycle
// only logs PrepareTranscript's error and then requires the file to exist, so
// an early return here would fail the Stop hook the placeholder exists to save.
func TestPrepareTranscript_CancelledContextStillCreatesPlaceholder(t *testing.T) {
	path := brainTranscriptPath(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(ctx, path); err != nil {
		t.Fatalf("PrepareTranscript with cancelled ctx: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("placeholder missing after cancelled ctx: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("placeholder size = %d, want 0", info.Size())
	}
}

// TestPrepareTranscript_RefusesSymlinkedTranscript: a symlink at the transcript
// path is refused rather than followed (filesystem-safety.md). A Stat would have
// reported the target's size and, for a dangling link, created a file at the far
// end of it; the store's Lstat reports the link itself and PrepareTranscript
// stops there, leaving the link's target untouched.
func TestPrepareTranscript_RefusesSymlinkedTranscript(t *testing.T) {
	path := brainTranscriptPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim.jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	a := &AntigravityAgent{}
	if err := a.PrepareTranscript(context.Background(), path); err == nil {
		t.Fatal("PrepareTranscript() error = nil, want refusal for a symlinked transcript")
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("nothing may be created at the link's target; Lstat err = %v", statErr)
	}
}

// TestReadTranscript_InsideBrainDirRefusesSymlink: a transcript inside agy's
// brain directory is read through the session store, so a symlink there is
// refused instead of followed to wherever it points. Outside the store the
// read is the ratcheted unconfined one (agent/transcript_read_guard_test.go).
func TestReadTranscript_InsideBrainDirRefusesSymlink(t *testing.T) {
	path := brainTranscriptPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "secret.jsonl")
	if err := os.WriteFile(target, []byte(`{"step_index":0,"type":"USER_INPUT","content":"leak"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	a := &AntigravityAgent{}
	if _, err := a.ReadTranscript(path); err == nil {
		t.Fatal("ReadTranscript() error = nil, want refusal for a symlinked transcript inside the store")
	}
	prompts, err := a.ExtractPrompts(path, 0)
	if err == nil || len(prompts) != 0 {
		t.Fatalf("ExtractPrompts() = %v, %v; want refusal and no prompts", prompts, err)
	}
	if _, posErr := a.GetTranscriptPosition(path); posErr == nil {
		t.Fatal("GetTranscriptPosition() error = nil, want refusal")
	}
}

// The bytes extractor is what condensation uses; it must agree with the
// path-based one line for line, including the offset metric (non-blank lines).
func TestExtractPromptsFromTranscript_MatchesPathBasedExtractor(t *testing.T) {
	t.Parallel()
	content := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nfirst ask\n</USER_REQUEST>"}

{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE"}
{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nadd another\n</USER_REQUEST>"}
`)
	a := &AntigravityAgent{}
	all, err := a.ExtractPromptsFromTranscript(content, 0)
	if err != nil || len(all) != 2 || all[0] != "first ask" || all[1] != "add another" {
		t.Fatalf("offset 0: got %v, %v", all, err)
	}
	later, err := a.ExtractPromptsFromTranscript(content, 2)
	if err != nil || len(later) != 1 || later[0] != "add another" {
		t.Fatalf("offset 2 (after two non-blank lines): got %v, %v", later, err)
	}

	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	fromPath, err := a.ExtractPrompts(path, 2)
	if err != nil || len(fromPath) != 1 || fromPath[0] != later[0] {
		t.Fatalf("path-based extractor disagrees: %v, %v", fromPath, err)
	}
}

// CondenseTranscript is what the summarizer sees. Shapes are the ones agy 1.2.7
// really writes: a wrapped USER_REQUEST, a PLANNER_RESPONSE carrying only
// tool_calls with double-encoded args, a GENERIC tool-output step (skipped),
// and a PLANNER_RESPONSE with the assistant's text.
func TestCondenseTranscript_RealStepShapes(t *testing.T) {
	t.Parallel()
	content := []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nCreate red.md\n</USER_REQUEST>"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"\"/ws/red.md\"","Overwrite":"false"}}]}
{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created At: ..."}
{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"Created [red.md](file:///ws/red.md)."}
{"step_index":4,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","content":"not from the user"}
not json
`)
	steps := CondenseTranscript(content)
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3: %+v", len(steps), steps)
	}
	if steps[0].Role != CondensedRoleUser || steps[0].Text != "Create red.md" {
		t.Errorf("step 0 = %+v, want the unwrapped user request", steps[0])
	}
	if steps[1].Role != CondensedRoleTool || steps[1].ToolName != "write_to_file" || steps[1].ToolArgs["TargetFile"] != "/ws/red.md" {
		t.Errorf("step 1 = %+v, want the tool call with its TargetFile decoded", steps[1])
	}
	if steps[2].Role != CondensedRoleAssistant || steps[2].Text != "Created [red.md](file:///ws/red.md)." {
		t.Errorf("step 2 = %+v, want the assistant text", steps[2])
	}
}

// agy double-encodes string args, so an empty string arrives as the raw value
// `"\"\""`. It must survive as "", not as the two-character literal `""` that
// a plain decode of the raw value produces — tool args go into generated
// summaries, where a corrupted value reads as real content.
func TestDecodeAgyArgs_PreservesEmptyStrings(t *testing.T) {
	t.Parallel()

	got := decodeAgyArgs(map[string]json.RawMessage{
		"doubleEmpty": json.RawMessage(`"\"\""`),
		"plainEmpty":  json.RawMessage(`""`),
		"doubleValue": json.RawMessage(`"\"hi\""`),
		"number":      json.RawMessage(`42`),
	})

	for key, want := range map[string]any{
		"doubleEmpty": "",
		"plainEmpty":  "",
		"doubleValue": "hi",
		"number":      float64(42),
	} {
		if got[key] != want {
			t.Errorf("decodeAgyArgs()[%q] = %#v, want %#v", key, got[key], want)
		}
	}
}

// The offset stored for a checkpoint counts non-blank lines, so the slice that
// scopes a summary to that checkpoint has to count them the same way. A raw
// \n-line slicer drifts by one for each interior blank line, which silently
// puts the summary's window over the wrong turns.
func TestSliceTranscriptFromPosition_IgnoresInteriorBlankLines(t *testing.T) {
	t.Parallel()

	const tail = `{"n":3}` + "\n" + `{"n":4}` + "\n"
	dense := `{"n":1}` + "\n" + `{"n":2}` + "\n" + tail
	sparse := `{"n":1}` + "\n\n" + `{"n":2}` + "\n   \n" + tail

	a := &AntigravityAgent{}
	if got := a.CountTranscriptPosition([]byte(sparse)); got != 4 {
		t.Fatalf("CountTranscriptPosition(sparse) = %d, want 4", got)
	}

	for name, content := range map[string]string{"dense": dense, "sparse": sparse} {
		if got := string(a.SliceTranscriptFromPosition([]byte(content), 2)); got != tail {
			t.Errorf("SliceTranscriptFromPosition(%s, 2) = %q, want %q", name, got, tail)
		}
	}

	if got := a.SliceTranscriptFromPosition([]byte(dense), 4); got != nil {
		t.Errorf("nothing past the end should slice to nil, got %q", got)
	}
}
