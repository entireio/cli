package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// No test in this file may call t.Parallel: they all set ENTIRE_CONFIG_DIR via
// t.Setenv for per-test record isolation and t.Chdir into a non-repo temp dir
// (the no-repo branch under test only fires when the hook cwd has no repo).

// chdirNonRepo moves the test into a temp dir that is not inside any git repo.
func chdirNonRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// claudeStopPayload builds the stdin for a claude-code stop hook.
func claudeStopPayload(t *testing.T, sessionID, transcriptPath string) *strings.Reader {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(payload))
}

// claudeWriteLine is one claude JSONL transcript line with a Write tool_use.
func claudeWriteLine(filePath string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"u","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":%q}}]}}`, filePath)
}

// writeClaudeTranscript writes lines as a claude JSONL transcript. Every line
// gets a trailing newline — the cursor convention counts complete lines, so a
// missing final newline would break the cursor-unchanged pins.
func writeClaudeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadNoRepoRecord loads the record for the fixed session ID every test in
// this file uses.
func loadNoRepoRecord(t *testing.T) *binding.SessionRecord {
	t.Helper()
	rec, err := binding.LoadRecord(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func assertNoSessionsDir(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(userdirs.Config(), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("no record may be created: %v", err)
	}
}

// Matrix row 1: Stop in a non-repo cwd with a transcript Write into an enabled
// repo B → record binds B and the cursor equals the transcript line count.
func TestRecordNoRepoEvidence_StopBindsTranscriptRepo(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)
	enableEntireAt(t, rootB)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine(filepath.Join(rootB, "f.go")))

	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo, got %+v", rec)
	}
	if rec.BoundRepos[0].WorktreeRoot != rootB {
		t.Errorf("bound root = %q, want %q", rec.BoundRepos[0].WorktreeRoot, rootB)
	}
	if !rec.BoundRepos[0].Enabled {
		t.Error("repo with .entire setup must record Enabled=true")
	}
	if rec.LastScannedTranscriptCursor != 1 {
		t.Errorf("cursor = %d, want 1 (transcript line count)", rec.LastScannedTranscriptCursor)
	}
	if rec.AgentType != string(agent.AgentTypeClaudeCode) {
		t.Errorf("agent type = %q, want %q", rec.AgentType, agent.AgentTypeClaudeCode)
	}
	if rec.TranscriptPath != transcriptPath {
		t.Errorf("transcript path = %q, want %q", rec.TranscriptPath, transcriptPath)
	}
	if rec.LaunchRoot != "" {
		t.Errorf("launch root = %q, want empty (no repo at cwd)", rec.LaunchRoot)
	}
}

// Matrix row 2: a second Stop over an UNCHANGED transcript is a cheap no-op —
// cursor unchanged, evidence count unchanged.
func TestRecordNoRepoEvidence_UnchangedTranscriptIsCheapNoOp(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine(filepath.Join(rootB, "f.go")))

	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))
	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo, got %+v", rec)
	}
	if rec.BoundRepos[0].EvidenceCount != 1 {
		t.Errorf("evidence count = %d, want 1 (already-scanned lines must not re-record)", rec.BoundRepos[0].EvidenceCount)
	}
	if rec.LastScannedTranscriptCursor != 1 {
		t.Errorf("cursor = %d, want 1", rec.LastScannedTranscriptCursor)
	}
}

// Matrix row 3: the transcript grew with a Write into repo C → C is added,
// B's evidence count is unchanged (its lines were not rescanned).
func TestRecordNoRepoEvidence_GrownTranscriptScansOnlyNewLines(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)
	rootC := newBindingRepo(t)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine(filepath.Join(rootB, "f.go")))
	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))

	writeClaudeTranscript(t, transcriptPath,
		claudeWriteLine(filepath.Join(rootB, "f.go")),
		claudeWriteLine(filepath.Join(rootC, "g.go")))
	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 2 {
		t.Fatalf("expected two bound repos, got %+v", rec)
	}
	counts := map[string]int{}
	for _, br := range rec.BoundRepos {
		counts[br.WorktreeRoot] = br.EvidenceCount
	}
	if counts[rootB] != 1 {
		t.Errorf("B evidence count = %d, want 1 (B's lines must not be rescanned)", counts[rootB])
	}
	if counts[rootC] != 1 {
		t.Errorf("C evidence count = %d, want 1", counts[rootC])
	}
	if rec.LastScannedTranscriptCursor != 2 {
		t.Errorf("cursor = %d, want 2", rec.LastScannedTranscriptCursor)
	}
}

// Matrix row 4: a truncated transcript (fewer lines than the cursor) rescans
// from 0 without crashing or duplicating the BoundRepo, and the STORED cursor
// equals the truncated file's line count afterwards — a stuck high-watermark
// cursor would re-trigger full rescans every turn.
func TestRecordNoRepoEvidence_TruncatedTranscriptRescansAndResetsCursor(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	line := claudeWriteLine(filepath.Join(rootB, "f.go"))
	writeClaudeTranscript(t, transcriptPath, line, line, line)
	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))
	if got := loadNoRepoRecord(t).LastScannedTranscriptCursor; got != 3 {
		t.Fatalf("setup cursor = %d, want 3", got)
	}

	writeClaudeTranscript(t, transcriptPath, line) // truncated/rotated
	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("truncation must not duplicate the bound repo, got %+v", rec)
	}
	if rec.BoundRepos[0].EvidenceCount != 2 {
		t.Errorf("evidence count = %d, want 2 (rescan re-records once)", rec.BoundRepos[0].EvidenceCount)
	}
	if rec.LastScannedTranscriptCursor != 1 {
		t.Errorf("cursor = %d, want 1 (reset must permit regression to the truncated count)", rec.LastScannedTranscriptCursor)
	}
}

// Matrix row 5: a codex post-tool-use payload with cwd-relative paths joins
// them onto the event CWD and records the foreign repo.
func TestRecordNoRepoEvidence_CodexToolUseJoinsRelativePaths(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	chdirNonRepo(t)
	rootB := newBindingRepo(t)

	// The tool ran in rootB's parent (the ~/dev/acme launch dir); the patch
	// names the file relative to it.
	relPath := filepath.Join(filepath.Base(rootB), "f.go")
	payload, err := json.Marshal(map[string]any{
		"session_id": "sess-1",
		"cwd":        filepath.Dir(rootB),
		"tool_name":  "apply_patch",
		"tool_input": map[string]string{
			"command": "*** Begin Patch\n*** Update File: " + relPath + "\n*** End Patch\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	recordNoRepoEvidence(context.Background(), agent.AgentNameCodex, codex.HookNamePostToolUse,
		strings.NewReader(string(payload)))

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo, got %+v", rec)
	}
	if rec.BoundRepos[0].WorktreeRoot != rootB {
		t.Errorf("bound root = %q, want %q", rec.BoundRepos[0].WorktreeRoot, rootB)
	}
}

// Matrix row 6: gemini after-agent goes through the analyzer fallback, the
// cursor stores the extractor-returned position (a MESSAGE INDEX, not a line
// or byte count), and a second unchanged call is a no-op. This is the
// gemini-coordinate pin: a generic line/byte cursor would silently skip or
// rescan gemini evidence.
func TestRecordNoRepoEvidence_GeminiAnalyzerFallbackUsesMessageIndexCursor(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)

	transcriptPath := filepath.Join(launchDir, "session.json")
	transcript := map[string]any{
		"messages": []map[string]any{
			{"id": "1", "type": "user", "content": []map[string]string{{"text": "write it"}}},
			{"id": "2", "type": "gemini", "content": "done", "toolCalls": []map[string]any{
				{"id": "t1", "name": "write_file", "args": map[string]string{"file_path": filepath.Join(rootB, "f.go")}},
			}},
		},
	}
	data, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{
		"session_id":      "sess-1",
		"transcript_path": transcriptPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	recordNoRepoEvidence(context.Background(), agent.AgentNameGemini, geminicli.HookNameAfterAgent,
		strings.NewReader(string(payload)))

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 1 {
		t.Fatalf("expected one bound repo via analyzer fallback, got %+v", rec)
	}
	if rec.BoundRepos[0].WorktreeRoot != rootB {
		t.Errorf("bound root = %q, want %q", rec.BoundRepos[0].WorktreeRoot, rootB)
	}
	if rec.LastScannedTranscriptCursor != 2 {
		t.Errorf("cursor = %d, want 2 (gemini's extractor position is a message index)", rec.LastScannedTranscriptCursor)
	}

	// Second unchanged call: no re-record, cursor unchanged.
	recordNoRepoEvidence(context.Background(), agent.AgentNameGemini, geminicli.HookNameAfterAgent,
		strings.NewReader(string(payload)))
	rec = loadNoRepoRecord(t)
	if rec.BoundRepos[0].EvidenceCount != 1 {
		t.Errorf("evidence count = %d, want 1 (unchanged transcript must not re-record)", rec.BoundRepos[0].EvidenceCount)
	}
	if rec.LastScannedTranscriptCursor != 2 {
		t.Errorf("cursor = %d, want 2 after no-op rescan", rec.LastScannedTranscriptCursor)
	}
}

// Matrix row 7: non-evidence events (session-start, user-prompt-submit) record
// nothing and do not error.
func TestRecordNoRepoEvidence_NonEvidenceEventsRecordNothing(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine(filepath.Join(launchDir, "f.go")))

	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameSessionStart,
		claudeStopPayload(t, "sess-1", transcriptPath))

	promptPayload, err := json.Marshal(map[string]string{
		"session_id":      "sess-1",
		"transcript_path": transcriptPath,
		"prompt":          "do things",
	})
	if err != nil {
		t.Fatal(err)
	}
	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameUserPromptSubmit,
		strings.NewReader(string(promptPayload)))

	assertNoSessionsDir(t)
}

// Matrix row 8: malformed stdin must not panic and must record nothing.
func TestRecordNoRepoEvidence_MalformedStdinIsSilent(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	chdirNonRepo(t)

	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		strings.NewReader("{not json"))

	assertNoSessionsDir(t)
}

// Matrix row 9: a nonexistent transcript path records nothing (not even a
// cursor-only record — there was nothing to scan).
func TestRecordNoRepoEvidence_MissingTranscriptRecordsNothing(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)

	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", filepath.Join(launchDir, "does-not-exist.jsonl")))

	assertNoSessionsDir(t)
}

// Matrix row 10: relative extracted transcript paths have no safe join base
// (the hook cwd is the launch dir, not necessarily where the tool ran) and are
// dropped — but the cursor still advances (always-advance semantics).
func TestRecordNoRepoEvidence_RelativeTranscriptPathsDropped(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine("rel/f.go"))

	recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
		claudeStopPayload(t, "sess-1", transcriptPath))

	rec := loadNoRepoRecord(t)
	if rec == nil {
		t.Fatal("always-advance: a cursor-only record must exist even when all paths are dropped")
	}
	if len(rec.BoundRepos) != 0 {
		t.Fatalf("relative paths must be dropped, got %+v", rec.BoundRepos)
	}
	if rec.LastScannedTranscriptCursor != 1 {
		t.Errorf("cursor = %d, want 1", rec.LastScannedTranscriptCursor)
	}
}

// Branch-level guard: an event without a real session ID records nothing.
// The unknownSessionID case matters independently of the tap's own guard: the
// cursor write (AdvanceTranscriptCursor) bypasses the tap, so without the
// branch-level guard a payload carrying the literal fallback ID would mint a
// sessions/unknown.json (plus its lock file) aggregating unrelated sessions.
func TestRecordNoRepoEvidence_EmptySessionIDRecordsNothing(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine(filepath.Join(rootB, "f.go")))

	for _, sessionID := range []string{"", unknownSessionID} {
		recordNoRepoEvidence(context.Background(), agent.AgentNameClaudeCode, claudecode.HookNameStop,
			claudeStopPayload(t, sessionID, transcriptPath))
	}

	assertNoSessionsDir(t)
	sessionsDir := filepath.Join(userdirs.Config(), "sessions")
	for _, name := range []string{unknownSessionID + ".json", unknownSessionID + ".json.lock"} {
		if _, err := os.Stat(filepath.Join(sessionsDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s must not be created for the fallback session ID: %v", name, err)
		}
	}
}

// Wiring pin: executeAgentHook in a non-repo cwd feeds the evidence path (and
// still exits 0 so the agent is never blocked).
func TestExecuteAgentHook_NonRepoCwdRecordsEvidence(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	launchDir := chdirNonRepo(t)
	rootB := newBindingRepo(t)
	enableEntireAt(t, rootB)

	transcriptPath := filepath.Join(launchDir, "transcript.jsonl")
	writeClaudeTranscript(t, transcriptPath, claudeWriteLine(filepath.Join(rootB, "f.go")))

	payload, err := json.Marshal(map[string]string{
		"session_id":      "sess-1",
		"transcript_path": transcriptPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := newAgentHookVerbCmdWithLogging(agent.AgentNameClaudeCode, claudecode.HookNameStop)
	cmd.SetIn(bytes.NewReader(payload))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hook in non-repo cwd must exit 0: %v", err)
	}

	rec := loadNoRepoRecord(t)
	if rec == nil || len(rec.BoundRepos) != 1 || rec.BoundRepos[0].WorktreeRoot != rootB {
		t.Fatalf("expected repo B bound through the hook entry point, got %+v", rec)
	}
}
