package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Plugin now only sends session_id, not transcript_path
	input := `{"session_id": "sess-abc123"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.SessionID != "sess-abc123" {
		t.Errorf("expected session_id 'sess-abc123', got %q", event.SessionID)
	}
	// SessionRef is now empty for session-start (no transcript path from plugin)
	if event.SessionRef != "" {
		t.Errorf("expected empty session ref, got %q", event.SessionRef)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Plugin now only sends session_id and prompt, not transcript_path
	input := `{"session_id": "sess-1", "prompt": "Fix the bug in login.ts"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnStart {
		t.Errorf("expected TurnStart, got %v", event.Type)
	}
	if event.Prompt != "Fix the bug in login.ts" {
		t.Errorf("expected prompt 'Fix the bug in login.ts', got %q", event.Prompt)
	}
	if event.SessionID != "sess-1" {
		t.Errorf("expected session_id 'sess-1', got %q", event.SessionID)
	}
	// SessionRef is computed from session_id, should end with .json
	if !strings.HasSuffix(event.SessionRef, "sess-1.json") {
		t.Errorf("expected session ref to end with 'sess-1.json', got %q", event.SessionRef)
	}
}

func TestParseHookEvent_TurnStart_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-model", "prompt": "hello", "model": "claude-sonnet-4-20250514"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Model field absent — should parse as empty string
	input := `{"session_id": "sess-no-model", "prompt": "hello"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "" {
		t.Errorf("expected empty model, got %q", event.Model)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-2"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnEnd {
		t.Errorf("expected TurnEnd, got %v", event.Type)
	}
	if event.SessionID != "sess-2" {
		t.Errorf("expected session_id 'sess-2', got %q", event.SessionID)
	}
	// SessionRef is computed from session_id, should end with .json (same pattern as TurnStart)
	if !strings.HasSuffix(event.SessionRef, "sess-2.json") {
		t.Errorf("expected session ref to end with 'sess-2.json', got %q", event.SessionRef)
	}
}

func TestHookRuntimePathUnroutableSkipsTurn(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.name", "Entire Test"},
		{"config", "user.email", "test@entire.io"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repo)
	paths.ClearWorktreeRootCache()
	paths.SetInvisibleProbeFailureForTesting(true)
	t.Cleanup(func() {
		paths.SetInvisibleProbeFailureForTesting(false)
		paths.ClearWorktreeRootCache()
	})

	ag := &OpenCodeAgent{}
	for _, hookName := range []string{HookNameTurnStart, HookNameTurnEnd} {
		event, err := ag.ParseHookEvent(t.Context(), hookName, strings.NewReader(`{"session_id":"sess-unroutable"}`))
		if err != nil || event != nil {
			t.Fatalf("ParseHookEvent(%s) = %+v, %v; want skipped event", hookName, event, err)
		}
	}

	if err := ag.PrepareTranscript(t.Context(), filepath.Join(repo, "sess-unroutable.json")); err != nil {
		t.Fatalf("PrepareTranscript() = %v; unroutable hook runtime must skip", err)
	}
}

func TestParseHookEvent_Compaction(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-3"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameCompaction, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.Compaction {
		t.Errorf("expected Compaction, got %v", event.Type)
	}
	if event.SessionID != "sess-3" {
		t.Errorf("expected session_id 'sess-3', got %q", event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Plugin now only sends session_id
	input := `{"session_id": "sess-4"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionEnd {
		t.Errorf("expected SessionEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader(`{}`))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader("not json"))

	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	cmd := ag.FormatResumeCommand("sess-abc123")

	expected := "opencode -s sess-abc123"
	if cmd != expected {
		t.Errorf("expected %q, got %q", expected, cmd)
	}
}

func TestFormatResumeCommand_Empty(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	cmd := ag.FormatResumeCommand("")

	if cmd != "opencode" {
		t.Errorf("expected %q, got %q", "opencode", cmd)
	}
}

func TestHookNames(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	names := ag.HookNames()

	expected := []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
		HookNameCompaction,
	}

	if len(names) != len(expected) {
		t.Fatalf("expected %d hook names, got %d", len(expected), len(names))
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	for _, e := range expected {
		if !nameSet[e] {
			t.Errorf("missing expected hook name: %s", e)
		}
	}
}

func TestPrepareTranscript_AlwaysRefreshesTranscript(t *testing.T) {
	// t.Chdir, not just t.TempDir: fetchAndCacheExport resolves the repo root from
	// CWD, so without this the export is staged in the developer's own repo.
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	transcriptPath := filepath.Join(t.TempDir(), "sess-123.json")

	// Create an existing file with stale data
	if err := os.WriteFile(transcriptPath, []byte(`{"info":{},"messages":[]}`), 0o600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	wantErr := errors.New("refresh attempted")
	stubExport(t, func(context.Context, *os.Root, string, string) error { return wantErr })

	err := (&OpenCodeAgent{}).PrepareTranscript(context.Background(), transcriptPath)
	if !errors.Is(err, wantErr) {
		t.Fatalf("PrepareTranscript error = %v, want %v", err, wantErr)
	}
}

func TestPrepareTranscript_ErrorOnInvalidPath(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}

	// Path without .json extension
	err := ag.PrepareTranscript(context.Background(), "/tmp/not-a-json-file")
	if err == nil {
		t.Fatal("expected error for path without .json extension")
	}
	if !strings.Contains(err.Error(), "invalid OpenCode transcript path") {
		t.Errorf("expected 'invalid OpenCode transcript path' error, got: %v", err)
	}
}

// TestPrepareTranscript_BrokenSymlinkFallsThroughToExport documents what a broken
// symlink actually does. The old name and comment claimed os.Stat returns a
// non-IsNotExist error that PrepareTranscript surfaces as a stat error, but Stat
// follows the link and reports ENOENT, so the guard does not fire and the path
// falls through to the export — which is why this test used to spawn the real
// `opencode` binary inside the developer's repository (and, per OpenCode's own
// bootstrap behavior, could rewrite .opencode/package-lock.json there).
func TestPrepareTranscript_BrokenSymlinkFallsThroughToExport(t *testing.T) {
	// Not parallel: t.Chdir. See TestPrepareTranscript_AlwaysRefreshesTranscript.
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	transcriptPath := filepath.Join(t.TempDir(), "broken-link.json")
	if err := os.Symlink("/nonexistent/target", transcriptPath); err != nil {
		t.Skipf("cannot create symlink (permission denied?): %v", err)
	}

	wantErr := errors.New("export attempted")
	stubExport(t, func(context.Context, *os.Root, string, string) error { return wantErr })

	err := (&OpenCodeAgent{}).PrepareTranscript(context.Background(), transcriptPath)
	if !errors.Is(err, wantErr) {
		t.Fatalf("PrepareTranscript error = %v, want the export error %v", err, wantErr)
	}
}

func TestPrepareTranscript_ErrorOnEmptySessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}

	// Path with empty session ID (.json with no basename)
	err := ag.PrepareTranscript(context.Background(), "/tmp/.json")
	if err == nil {
		t.Fatal("expected error for empty session ID")
	}
	if !strings.Contains(err.Error(), "empty session ID") {
		t.Errorf("expected 'empty session ID' error, got: %v", err)
	}
}

func TestParseHookEvent_TurnStart_InvalidSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "../escape", "prompt": "hello"}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err == nil {
		t.Fatal("expected error for path-traversal session ID")
	}
	if !strings.Contains(err.Error(), "contains path separators") {
		t.Errorf("expected 'contains path separators' error, got: %v", err)
	}
}

func TestParseHookEvent_TurnEnd_InvalidSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "../escape"}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameTurnEnd, strings.NewReader(input))

	if err == nil {
		t.Fatal("expected error for path-traversal session ID")
	}
	if !strings.Contains(err.Error(), "contains path separators") {
		t.Errorf("expected 'contains path separators' error, got: %v", err)
	}
}

func TestFetchAndCacheExport_WritesAndValidatesExportFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(_ context.Context, root *os.Root, sessionID, outputName string) error {
		if sessionID != "ses_abc123" {
			return fmt.Errorf("unexpected session id: %s", sessionID)
		}
		return root.WriteFile(outputName, []byte(`{"info":{"id":"ses_abc123"},"messages":[]}`), 0o600)
	}
	t.Cleanup(func() {
		runOpenCodeExportToFileFn = original
	})

	ag := &OpenCodeAgent{}
	transcriptPath, err := ag.fetchAndCacheExport(context.Background(), "ses_abc123")
	require.NoError(t, err)

	content, err := os.ReadFile(transcriptPath)
	require.NoError(t, err)
	require.True(t, json.Valid(content), "expected cached transcript to be valid JSON")
	require.Contains(t, string(content), "\"ses_abc123\"")
}

// cachedTranscriptFixture sets up a repo whose .entire/tmp already holds a
// hook-cached transcript for sessionID, and returns its path. This is the file
// every export has to protect: nothing condenses it into a checkpoint until the
// user commits, so until then it is the only local copy of the session.
func cachedTranscriptFixture(t *testing.T, sessionID, content string) string {
	t.Helper()

	repo := t.TempDir()
	t.Chdir(repo)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	tmpDir := filepath.Join(repo, paths.EntireTmpDir)
	require.NoError(t, os.MkdirAll(tmpDir, 0o750))
	cached := filepath.Join(tmpDir, sessionID+".json")
	require.NoError(t, os.WriteFile(cached, []byte(content), 0o600))
	return cached
}

// stubExport replaces the export runner for the duration of the test. The stub
// receives the .entire root and the staging name within it, exactly as the real
// runner does.
func stubExport(t *testing.T, fn func(ctx context.Context, root *os.Root, sessionID, outputName string) error) {
	t.Helper()

	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = fn
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })
}

// assertNoStagedExports fails if a staged export was left behind next to cached.
func assertNoStagedExports(t *testing.T, cached string) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Dir(cached))
	require.NoError(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), exportStagePrefix) {
			t.Errorf("staged export left behind: %s", entry.Name())
		}
	}
}

// TestFetchAndCacheExport_PartialFailurePreservesCachedTranscript covers an export
// that writes some output and then fails — a rejected session mid-stream, or the
// 30s timeout firing.
func TestFetchAndCacheExport_PartialFailurePreservesCachedTranscript(t *testing.T) {
	const (
		sessionID = "ses_partial"
		cached    = `{"info":{"id":"ses_partial"},"messages":[1,2,3]}`
	)
	cachedPath := cachedTranscriptFixture(t, sessionID, cached)

	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		if err := root.WriteFile(outputName, []byte(`{"info":{"id":"ses_par`), 0o600); err != nil {
			return err
		}
		return errors.New("export timed out")
	})

	_, err := (&OpenCodeAgent{}).fetchAndCacheExport(context.Background(), sessionID)
	require.Error(t, err)

	got, readErr := os.ReadFile(cachedPath)
	require.NoError(t, readErr, "cached transcript was destroyed by a failed export")
	require.JSONEq(t, cached, string(got), "cached transcript was modified by a failed export")
	assertNoStagedExports(t, cachedPath)
}

// TestFetchAndCacheExport_TruncatedZeroExitPreservesCachedTranscript covers the
// nastier variant: `opencode export` exits 0 having written truncated output. The
// install must not happen, because attach's os.Stat branch would accept the
// corrupt file and treat PrepareTranscript's failure as best-effort — using a
// truncated transcript silently, where a missing one would be re-fetched.
func TestFetchAndCacheExport_TruncatedZeroExitPreservesCachedTranscript(t *testing.T) {
	const (
		sessionID = "ses_truncated"
		cached    = `{"info":{"id":"ses_truncated"},"messages":[1,2,3]}`
	)
	cachedPath := cachedTranscriptFixture(t, sessionID, cached)

	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		// Exit 0, truncated payload.
		return root.WriteFile(outputName, []byte(`{"info":{"id":"ses_trun`), 0o600)
	})

	_, err := (&OpenCodeAgent{}).fetchAndCacheExport(context.Background(), sessionID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid transcript data")

	got, readErr := os.ReadFile(cachedPath)
	require.NoError(t, readErr, "cached transcript was destroyed by a zero-exit truncated export")
	require.JSONEq(t, cached, string(got), "cached transcript was replaced with truncated output")
	assertNoStagedExports(t, cachedPath)
}

func TestFetchAndCacheExport_EmptyZeroExitPreservesCachedTranscript(t *testing.T) {
	const (
		sessionID = "ses_empty"
		cached    = `{"info":{"id":"ses_empty"},"messages":[1,2,3]}`
	)
	cachedPath := cachedTranscriptFixture(t, sessionID, cached)

	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, nil, 0o600)
	})

	_, err := (&OpenCodeAgent{}).fetchAndCacheExport(context.Background(), sessionID)
	require.Error(t, err)

	got, readErr := os.ReadFile(cachedPath)
	require.NoError(t, readErr, "cached transcript was destroyed by an empty export")
	require.JSONEq(t, cached, string(got))
	assertNoStagedExports(t, cachedPath)
}

func TestFetchAndCacheExport_InstallsValidExportOverCachedTranscript(t *testing.T) {
	const (
		sessionID = "ses_replace"
		cached    = `{"info":{"id":"ses_replace"},"messages":[1]}`
		fresh     = `{"info":{"id":"ses_replace"},"messages":[1,2,3,4]}`
	)
	cachedPath := cachedTranscriptFixture(t, sessionID, cached)

	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		// The staging path is not the live transcript, so the cached copy must
		// still be intact at this point.
		current, err := os.ReadFile(cachedPath)
		if err != nil {
			return err
		}
		if string(current) != cached {
			return fmt.Errorf("export wrote through to the live transcript: %q", string(current))
		}
		return root.WriteFile(outputName, []byte(fresh), 0o600)
	})

	got, err := (&OpenCodeAgent{}).fetchAndCacheExport(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID+".json", filepath.Base(got),
		"should return the live transcript path, not the staging path")

	content, err := os.ReadFile(cachedPath)
	require.NoError(t, err)
	require.JSONEq(t, fresh, string(content))
	assertNoStagedExports(t, cachedPath)
}

func TestFetchTranscript_ValidatesSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	if _, err := ag.FetchTranscript(context.Background(), "bad/session-id"); err == nil {
		t.Fatal("expected error for session ID with path separator, got nil")
	}
}

func TestFetchTranscript_AttemptsExport(t *testing.T) {
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	wantErr := errors.New("export attempted")
	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(context.Context, *os.Root, string, string) error { return wantErr }
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	ag := &OpenCodeAgent{}
	_, err := ag.FetchTranscript(context.Background(), "test-fetch-transcript-no-such-session")
	if !errors.Is(err, wantErr) {
		t.Fatalf("FetchTranscript error = %v, want %v", err, wantErr)
	}
}

func TestFetchTranscript_ReportsInvalidJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, []byte("not json"), 0o600)
	}
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	const sessionID = "test-invalid-json"
	_, err := (&OpenCodeAgent{}).FetchTranscript(context.Background(), sessionID)
	want := `OpenCode returned invalid transcript data for session "test-invalid-json". Try updating OpenCode and running the command again.`
	if err == nil || err.Error() != want {
		t.Fatalf("FetchTranscript error = %v, want %q", err, want)
	}
}
