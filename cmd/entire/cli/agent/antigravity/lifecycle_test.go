package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestHookNames(t *testing.T) {
	t.Parallel()
	a := &AntigravityAgent{}
	names := a.HookNames()
	want := []string{
		HookNamePreToolUse,
		HookNamePostToolUse,
		HookNamePreInvocation,
		HookNamePostInvocation,
		HookNameStop,
	}
	if len(names) != len(want) {
		t.Fatalf("HookNames() returned %d names, want %d: %v", len(names), len(want), names)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("HookNames()[%d] = %q, want %q", i, names[i], n)
		}
	}
}

func TestParseHookEvent_PreInvocation_FirstInvocationEmitsTurnStart(t *testing.T) {
	t.Parallel()
	// Payload values mirror real agy 1.0.0 wire format captured from the agy
	// binary: the first PreInvocation of a fresh conversation has
	// invocationNum=0 (yes, zero — agy is 0-indexed despite the docs reading
	// like 1-based) and initialNumSteps=1 (agy inserts the user prompt as
	// step 0 before the first model call fires). See the comment block on
	// parsePreInvocation for the captured stdin samples.
	payload := InvocationPayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		InvocationNum:   0,
		InitialNumSteps: 1,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreInvocation, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for invocationNum=0 pre-invocation")
	}
	if ev.Type != agent.TurnStart {
		t.Errorf("Type = %v, want TurnStart", ev.Type)
	}
	if ev.SessionID != testConversationID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, testConversationID)
	}
	if ev.SessionRef != testTranscriptPath {
		t.Errorf("SessionRef = %q, want %q", ev.SessionRef, testTranscriptPath)
	}
}

// TestParseHookEvent_PreInvocation_FollowUpReturnsNil verifies that agy's
// per-model-call PreInvocations (invocationNum > 0) do NOT re-fire TurnStart.
// This is the bug that produced "no files modified during session, skipping
// checkpoint" in real-agy testing: each PreInvocation that emits TurnStart
// causes the framework to re-capture pre-prompt state, clobbering the
// baseline used by TurnEnd's file-diff.
//
// agy 1.0.0 ships invocationNum **0-indexed** (the first call is 0, the
// second is 1, etc.) — captured from real stdin, not from the docs which
// describe invocationNum ambiguously. The fixture
// testdata/hook_stdin_pre_invocation.json carries invocationNum=1 for this
// reason — that's a follow-up under the real-agy numbering.
func TestParseHookEvent_PreInvocation_FollowUpReturnsNil(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_pre_invocation.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreInvocation, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for invocationNum>1 pre-invocation, got %+v", ev)
	}
}

func TestParseHookEvent_PostInvocationReturnsNil(t *testing.T) {
	t.Parallel()
	// Antigravity writes its transcript AFTER Stop fires, not before
	// PostInvocation. Emitting TurnEnd here would trigger a transcript-read
	// in handleLifecycleTurnEnd and fail with "transcript file not found",
	// terminating agy's agent turn. parsePostInvocation must return nil so
	// the framework treats it as a no-op.
	data, err := os.ReadFile("testdata/hook_stdin_post_invocation.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePostInvocation, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for post-invocation, got %+v", ev)
	}
}

func TestParseHookEvent_Stop_FullyIdleTrueEmitsTurnEnd(t *testing.T) {
	t.Parallel()
	// Stop with fullyIdle=true must emit TurnEnd (not SessionEnd) so the
	// framework's TurnEnd handler invokes SaveStep — which increments
	// step_count, writes a checkpoint to the shadow branch, and lets the
	// eventual `git commit` produce a real checkpoint on entire/checkpoints/v1.
	// Emitting SessionEnd here would skip SaveStep entirely, leaving the
	// session without a shadow branch and getting it garbage-collected at
	// commit time.
	data, err := os.ReadFile("testdata/hook_stdin_stop.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNameStop, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for stop with fullyIdle=true")
	}
	if ev.Type != agent.TurnEnd {
		t.Errorf("Type = %v, want TurnEnd", ev.Type)
	}
	if ev.SessionID != testConversationID {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, testConversationID)
	}
}

func TestParseHookEvent_Stop_FullyIdleFalseReturnsNil(t *testing.T) {
	t.Parallel()
	// Synthesize a stop payload with fullyIdle=false
	payload := StopPayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ExecutionNum:      1,
		TerminationReason: "background_tasks",
		FullyIdle:         false,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNameStop, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for stop with fullyIdle=false, got %+v", ev)
	}
}

func TestParseHookEvent_PreToolUse_WriteToFileExtractsModifiedFiles(t *testing.T) {
	t.Parallel()
	// Synthesize a PreToolUse payload with write_to_file (Overwrite=true → ModifiedFiles)
	type writeArgs struct {
		TargetFile string `json:"TargetFile"`
		Overwrite  bool   `json:"Overwrite"`
	}
	argsJSON, err := json.Marshal(writeArgs{TargetFile: "src/main.go", Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ToolCall: ToolCall{
			Name: "write_to_file",
			Args: json.RawMessage(argsJSON),
		},
		StepIdx: 1,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for write_to_file tool")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	if len(ev.ModifiedFiles) != 1 || ev.ModifiedFiles[0] != "src/main.go" {
		t.Errorf("ModifiedFiles = %v, want [src/main.go]", ev.ModifiedFiles)
	}
	if len(ev.NewFiles) != 0 {
		t.Errorf("NewFiles = %v, want empty (Overwrite=true → ModifiedFiles)", ev.NewFiles)
	}
}

func TestParseHookEvent_PreToolUse_WriteToFileNewFile(t *testing.T) {
	t.Parallel()
	// Overwrite=false → NewFiles
	type writeArgs struct {
		TargetFile string `json:"TargetFile"`
		Overwrite  bool   `json:"Overwrite"`
	}
	argsJSON, err := json.Marshal(writeArgs{TargetFile: "src/new.go", Overwrite: false})
	if err != nil {
		t.Fatal(err)
	}
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ToolCall: ToolCall{
			Name: "write_to_file",
			Args: json.RawMessage(argsJSON),
		},
		StepIdx: 2,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for write_to_file (new file)")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	if len(ev.NewFiles) != 1 || ev.NewFiles[0] != "src/new.go" {
		t.Errorf("NewFiles = %v, want [src/new.go]", ev.NewFiles)
	}
	if len(ev.ModifiedFiles) != 0 {
		t.Errorf("ModifiedFiles = %v, want empty (Overwrite=false → NewFiles)", ev.ModifiedFiles)
	}
}

func TestParseHookEvent_PreToolUse_ReplaceFileContent(t *testing.T) {
	t.Parallel()
	type replaceArgs struct {
		TargetFile string `json:"TargetFile"`
	}
	argsJSON, err := json.Marshal(replaceArgs{TargetFile: "src/foo.go"})
	if err != nil {
		t.Fatal(err)
	}
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
		},
		ToolCall: ToolCall{Name: "replace_file_content", Args: json.RawMessage(argsJSON)},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for replace_file_content")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	if len(ev.ModifiedFiles) != 1 || ev.ModifiedFiles[0] != "src/foo.go" {
		t.Errorf("ModifiedFiles = %v, want [src/foo.go]", ev.ModifiedFiles)
	}
}

func TestParseHookEvent_PreToolUse_NonMutatingToolReturnsNil(t *testing.T) {
	t.Parallel()
	// Use the testdata fixture which uses run_command (non-mutating)
	data, err := os.ReadFile("testdata/hook_stdin_pre_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for non-mutating tool run_command, got %+v", ev)
	}
}

// TestParseHookEvent_PreToolUse_AgyDoubleEncodedArgs verifies the file
// extraction tolerates agy 1.0.0's quirky wire format where every tool arg
// value is itself a JSON-encoded string. Without this resilience, the
// json.Unmarshal of the args struct fails silently on the Overwrite type
// mismatch (string "true" vs Go bool), TargetFile stays empty, and no
// ToolUse event is emitted — meaning no files_touched gets recorded and
// no checkpoint is created when the user commits. This is the bug that
// agy smoke testing surfaced.
// TestResolveAgySymlinks_ParentSymlink verifies the symlink-resolution helper
// handles macOS-style /tmp → /private/tmp parent-dir symlinks. This is the
// exact bug that broke files_touched capture on macOS: agy sends paths under
// /tmp/foo/bar.md, but paths.WorktreeRoot returns /private/tmp/foo, and the
// framework's filepath.Rel against the unresolved path yields ../../tmp/...
// which gets filtered as "outside repo".
func TestResolveAgySymlinks_ParentSymlink(t *testing.T) {
	t.Parallel()
	// Set up a directory and a symlink that points to it.
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	// File doesn't need to exist (write_to_file is creating it).
	through := filepath.Join(linkDir, "new.txt")
	resolved := resolveAgySymlinks(through)
	// On macOS, t.TempDir() returns /var/folders/... which itself is a symlink
	// to /private/var/folders/.... EvalSymlinks will resolve both layers, so
	// the want value must also be resolved for a fair comparison.
	wantParent, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(realDir): %v", err)
	}
	want := filepath.Join(wantParent, "new.txt")
	if resolved != want {
		t.Errorf("resolveAgySymlinks(%q) = %q, want %q", through, resolved, want)
	}
}

func TestResolveAgySymlinks_RelativePathUnchanged(t *testing.T) {
	t.Parallel()
	if got := resolveAgySymlinks("foo/bar.txt"); got != "foo/bar.txt" {
		t.Errorf("relative path should pass through unchanged, got %q", got)
	}
}

// TestResolveAgySymlinks_NewNestedDirectory verifies the symlink resolver
// walks up to the deepest existing ancestor when agy's write_to_file is
// creating both a new directory AND a file inside it. The original
// implementation only EvalSymlinks'd the immediate parent and failed
// (lstat: no such file or directory), silently returning the unresolved
// path — which would then be filtered as "outside repo" on macOS due to
// the /tmp → /private/tmp symlink.
func TestResolveAgySymlinks_NewNestedDirectory(t *testing.T) {
	t.Parallel()
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	// Path: symlinked-dir/<missing-subdir>/<missing-file>
	// Both newdir and file.txt do not exist; resolver must walk up to
	// linkDir, resolve the symlink, then reattach newdir/file.txt.
	through := filepath.Join(linkDir, "newdir", "file.txt")
	resolved := resolveAgySymlinks(through)

	wantParent, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(realDir): %v", err)
	}
	want := filepath.Join(wantParent, "newdir", "file.txt")
	if resolved != want {
		t.Errorf("resolveAgySymlinks(%q) = %q, want %q", through, resolved, want)
	}
}

// TestResolveAgySymlinks_NoExistingAncestor verifies the resolver returns
// the input unchanged when no ancestor of the path exists at all (root is
// reached without finding a resolvable directory). This is the only path
// where the function gives up; the test pins that behavior so we don't
// accidentally return "" or a partially-resolved bogus path.
func TestResolveAgySymlinks_NoExistingAncestor(t *testing.T) {
	t.Parallel()
	// /<random>/<random>/<random> — extremely unlikely to exist.
	p := "/nonexistent-prefix-" + filepath.Base(t.TempDir()) + "/a/b/c.txt"
	if got := resolveAgySymlinks(p); got != p {
		t.Errorf("expected unchanged input %q, got %q", p, got)
	}
}

func TestParseHookEvent_PreToolUse_AgyDoubleEncodedArgs(t *testing.T) {
	t.Parallel()
	// Reproduce agy's actual wire format exactly:
	//   "TargetFile": "\"/tmp/hello.txt\"",   ← string-containing-JSON-string
	//   "Overwrite":  "true",                  ← string instead of bool
	argsRaw := []byte(`{"TargetFile":"\"hello.txt\"","Overwrite":"true","CodeContent":"\"hi\""}`)
	payload := PreToolUsePayload{
		CommonPayload: CommonPayload{
			ConversationID: testConversationID,
			TranscriptPath: testTranscriptPath,
			WorkspacePaths: []string{testWorkspacePath},
		},
		ToolCall: ToolCall{Name: "write_to_file", Args: json.RawMessage(argsRaw)},
		StepIdx:  1,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePreToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected non-nil event for write_to_file with agy-double-encoded args; got nil — file extraction silently failed")
	}
	if ev.Type != agent.ToolUse {
		t.Errorf("Type = %v, want ToolUse", ev.Type)
	}
	// Overwrite=true (as string) → file goes into ModifiedFiles, not NewFiles
	if len(ev.ModifiedFiles) != 1 || ev.ModifiedFiles[0] != "hello.txt" {
		t.Errorf("ModifiedFiles = %v, want [hello.txt]", ev.ModifiedFiles)
	}
	if len(ev.NewFiles) != 0 {
		t.Errorf("NewFiles = %v, want empty (Overwrite=true)", ev.NewFiles)
	}
}

func TestParseHookEvent_PostToolUseReturnsNil(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/hook_stdin_post_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &AntigravityAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNamePostToolUse, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev != nil {
		t.Errorf("expected nil event for post-tool-use, got %+v", ev)
	}
}
