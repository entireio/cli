package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"     // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"          // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"         // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid" // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"      // register agent
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestAttach_MissingSessionID(t *testing.T) {
	t.Parallel()

	cmd := newAttachCmd()
	cmd.SetArgs([]string{})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected help output, got error: %v", err)
	}
	if !strings.Contains(out.String(), "attach <session-id>") {
		t.Errorf("expected help output containing usage, got: %s", out.String())
	}
}

func TestAttach_TranscriptNotFound(t *testing.T) {
	setupAttachTestRepo(t)

	// Set up a fake Claude project dir that's empty
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", t.TempDir())
	// Redirect HOME so the fallback search doesn't walk real ~/.claude/projects
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, "nonexistent-session-id", agent.AgentNameClaudeCode, true)
	if err == nil {
		t.Fatal("expected error for missing transcript")
	}
}

func TestAttach_Success(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-session-001"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"create a hello world file"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{"file_path":"hello.txt","content":"world"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"wrote file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done! I created hello.txt."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	// Verify session state was created
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.SessionID != sessionID {
		t.Errorf("session ID = %q, want %q", state.SessionID, sessionID)
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after attach")
	}

	// Verify output message
	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}
	if !strings.Contains(output, "Created checkpoint") {
		t.Errorf("expected 'Created checkpoint' in output, got: %s", output)
	}
}

func TestAttach_SessionAlreadyTracked_NoCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-duplicate"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"explain the auth module"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The auth module handles user authentication via JWT tokens."}]},"uuid":"uuid-2"}
`)

	// Pre-create session state without a checkpoint ID (simulates hooks tracking
	// the session but condensation never happening).
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), &session.State{
		SessionID: sessionID,
		AgentType: agent.AgentTypeClaudeCode,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, true)
	if err != nil {
		t.Fatalf("expected attach to handle already-tracked session, got error: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}

	// Verify checkpoint was created
	reloadedState, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedState.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after re-attach")
	}
}

func TestAttach_OutputContainsCheckpointID(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-checkpoint-output"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"add error handling"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Edit","input":{"file_path":"main.go","old_string":"return nil","new_string":"return fmt.Errorf(\"failed: %w\", err)"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"edited file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Added error wrapping."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	output := out.String()

	// Must contain Entire-Checkpoint trailer with 12-hex-char ID
	re := regexp.MustCompile(`Entire-Checkpoint: [0-9a-f]{12}`)
	if !re.MatchString(output) {
		t.Errorf("expected 'Entire-Checkpoint: <12-hex-id>' in output, got:\n%s", output)
	}
}

func TestAttach_PopulatesTokenUsage(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-token-usage"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"uuid":"a1"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, true); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.TokenUsage == nil {
		t.Fatal("expected TokenUsage to be set")
	}
	if state.TokenUsage.OutputTokens == 0 {
		t.Error("expected OutputTokens > 0")
	}
}

func TestAttach_SetsSessionTurnCount(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-turn-count"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"first prompt"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"response 1"},"uuid":"a1"}
{"type":"user","message":{"role":"user","content":"second prompt"},"uuid":"u2"}
{"type":"assistant","message":{"role":"assistant","content":"response 2"},"uuid":"a2"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, true); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionTurnCount != 2 {
		t.Errorf("SessionTurnCount = %d, want 2", state.SessionTurnCount)
	}
}

func TestCountUserTurns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want int
	}{
		{
			name: "gemini format",
			data: []byte(`{"messages":[{"type":"user","content":"first"},{"type":"gemini","content":"ok"},{"type":"user","content":"second"},{"type":"gemini","content":"done"}]}`),
			want: 2,
		},
		{
			name: "jsonl with tool_result should not double count",
			data: []byte(`{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{}}]},"uuid":"a1"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]},"uuid":"u2"}
{"type":"user","message":{"role":"user","content":"next prompt"},"uuid":"u3"}
`),
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTranscriptMetadata(tt.data).TurnCount
			if got != tt.want {
				t.Errorf("extractTranscriptMetadata().TurnCount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExtractModelFromTranscript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "claude code with model",
			data: []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hello"}]},"uuid":"a1"}
`),
			want: "claude-sonnet-4-20250514",
		},
		{
			name: "no model field",
			data: []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"hello"},"uuid":"a1"}
`),
			want: "",
		},
		{
			name: "gemini format (no model in transcript)",
			data: []byte(`{"messages":[{"type":"user","content":"hi"},{"type":"gemini","content":"hello"}]}`),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTranscriptMetadata(tt.data).Model
			if got != tt.want {
				t.Errorf("extractTranscriptMetadata().Model = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractFirstPromptFromTranscript_GeminiFormat(t *testing.T) {
	t.Parallel()

	data := []byte(`{"messages":[{"type":"user","content":"fix the login bug"},{"type":"gemini","content":"I'll look at that"}]}`)
	got := extractTranscriptMetadata(data).FirstPrompt
	if got != "fix the login bug" {
		t.Errorf("extractTranscriptMetadata(gemini).FirstPrompt = %q, want %q", got, "fix the login bug")
	}
}

func TestExtractFirstPromptFromTranscript_JSONLFormat(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","message":{"role":"user","content":"hello world"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"uuid":"a1"}
`)
	got := extractTranscriptMetadata(data).FirstPrompt
	if got != "hello world" {
		t.Errorf("extractTranscriptMetadata(jsonl).FirstPrompt = %q, want %q", got, "hello world")
	}
}

func TestAttach_GeminiSubdirectorySession(t *testing.T) {
	setupAttachTestRepo(t)

	// Redirect HOME so searchTranscriptInProjectDirs searches our fake Gemini dir
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Create a Gemini transcript in a *different* project hash directory,
	// simulating a session started from a subdirectory (different CWD hash).
	differentProjectDir := filepath.Join(fakeHome, ".gemini", "tmp", "different-hash", "chats")
	if err := os.MkdirAll(differentProjectDir, 0o750); err != nil {
		t.Fatal(err)
	}

	sessionID := "abcd1234-gemini-subdir-test"
	transcriptContent := `{"messages":[{"type":"user","content":"hello"},{"type":"gemini","content":"hi"}]}`
	// Gemini names files as session-<date>-<shortid>.json where shortid = sessionID[:8]
	transcriptFile := filepath.Join(differentProjectDir, "session-2026-01-01T10-00-abcd1234.json")
	if err := os.WriteFile(transcriptFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Set the expected project dir to an empty directory so the primary lookup fails
	// and the fallback search kicks in.
	emptyProjectDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", emptyProjectDir)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameGemini, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}

	store, storeErr := session.NewStateStore(context.Background())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	state, loadErr := store.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeGemini)
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after attach")
	}
}

func TestAttach_GeminiSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	// Create Gemini transcript in expected project dir
	geminiDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", geminiDir)

	sessionID := "abcd1234-gemini-success-test"
	transcriptContent := `{"messages":[{"type":"user","content":"fix the login bug"},{"type":"gemini","content":"I will fix the login bug now."}]}`
	transcriptFile := filepath.Join(geminiDir, "session-2026-01-01T10-00-abcd1234.json")
	if err := os.WriteFile(transcriptFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameGemini, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}

	// Verify session state
	store, storeErr := session.NewStateStore(context.Background())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	state, loadErr := store.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeGemini)
	}
	if state.SessionTurnCount != 1 {
		t.Errorf("SessionTurnCount = %d, want 1", state.SessionTurnCount)
	}
}

func TestAttach_CursorSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)

	sessionID := "test-attach-cursor-session"
	// Cursor uses JSONL format, same as Claude Code
	transcriptContent := `{"type":"user","message":{"role":"user","content":"add dark mode"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"I'll add dark mode support."},"uuid":"a1"}
`
	// Cursor flat layout: <dir>/<id>.jsonl
	if err := os.WriteFile(filepath.Join(cursorDir, sessionID+".jsonl"), []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameCursor, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeCursor {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeCursor)
	}
	if state.SessionTurnCount != 1 {
		t.Errorf("SessionTurnCount = %d, want 1", state.SessionTurnCount)
	}
}

func TestAttach_CodexSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	codexDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", codexDir)

	sessionID := "019d6c43-1537-7343-9691-1f8cee04fe59"
	transcriptContent := `{"timestamp":"2026-04-08T10:43:48.000Z","type":"session_meta","payload":{"id":"019d6c43-1537-7343-9691-1f8cee04fe59","timestamp":"2026-04-08T10:43:48.000Z"}}
{"timestamp":"2026-04-08T10:43:49.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"investigate attach failure"}]}}
{"timestamp":"2026-04-08T10:43:50.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking into it."}]}}
`
	sessionFile := filepath.Join(codexDir, "2026", "04", "08", "rollout-2026-04-08T10-43-48-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameCodex, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeCodex {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeCodex)
	}
	if state.TranscriptPath != sessionFile {
		t.Errorf("TranscriptPath = %q, want %q", state.TranscriptPath, sessionFile)
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after attach")
	}
}

func TestAttach_FactoryAIDroidSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	droidDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_DROID_PROJECT_DIR", droidDir)

	sessionID := "test-attach-droid-session"
	// Factory AI Droid uses JSONL format
	transcriptContent := `{"type":"user","message":{"role":"user","content":"deploy to staging"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"Deploying to staging now."},"uuid":"a1"}
`
	// Factory AI Droid: flat <dir>/<id>.jsonl
	if err := os.WriteFile(filepath.Join(droidDir, sessionID+".jsonl"), []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameFactoryAIDroid, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeFactoryAIDroid {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeFactoryAIDroid)
	}
	if state.SessionTurnCount != 1 {
		t.Errorf("SessionTurnCount = %d, want 1", state.SessionTurnCount)
	}
}

func TestAttach_CursorNestedLayout(t *testing.T) {
	setupAttachTestRepo(t)

	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)

	sessionID := "test-cursor-nested-layout"
	transcriptContent := `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
`
	// Cursor IDE nested layout: <dir>/<id>/<id>.jsonl
	nestedDir := filepath.Join(cursorDir, sessionID)
	if err := os.MkdirAll(nestedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, sessionID+".jsonl"), []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameCursor, true)
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}
}

// setupAttachTestRepo creates a temp git repo with one commit and enables Entire.
// Returns the repo directory. Caller must not use t.Parallel() (uses t.Chdir).
func setupAttachTestRepo(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	enableEntire(t, tmpDir)
}

// setupClaudeTranscript creates a fake Claude transcript file.
// The file's mtime is backdated so that waitForTranscriptFlush treats it as
// stale and skips the 3-second poll loop.
func setupClaudeTranscript(t *testing.T, sessionID, content string) {
	t.Helper()
	claudeDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)
	fpath := filepath.Join(claudeDir, sessionID+".jsonl")
	if err := os.WriteFile(fpath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(fpath, stale, stale); err != nil {
		t.Fatal(err)
	}
}

// enableEntire creates the .entire/settings.json file to mark Entire as enabled.
func enableEntire(t *testing.T, repoDir string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	settingsContent := `{"enabled": true}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsContent), 0o600); err != nil {
		t.Fatal(err)
	}
}
