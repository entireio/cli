//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// TestAttach_NewSession_NoHooks tests attaching a session that was never tracked by hooks.
// Scenario: agent ran outside of Entire's hooks, user wants to import the session.
func TestAttach_NewSession_NoHooks(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Simulate an agent session that happened without hooks:
	// create a transcript file in the Claude project dir.
	sessionID := "attach-new-session-001"
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("explain the auth module")
	tb.AddAssistantMessage("The auth module handles user authentication.")
	transcriptPath := filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")
	if err := tb.WriteToFile(transcriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Run attach
	output := env.RunCLI("session", "attach", sessionID, "-a", "claude-code", "-f")

	// Verify output
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Created checkpoint") {
		t.Errorf("expected 'Created checkpoint' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Entire-Checkpoint") {
		t.Errorf("expected checkpoint trailer in output, got:\n%s", output)
	}

	// Verify the commit was amended with the checkpoint trailer
	headMsg := env.GetCommitMessage(env.GetHeadHash())
	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if cpID == "" {
		t.Errorf("expected Entire-Checkpoint trailer on HEAD, commit message:\n%s", headMsg)
	}

	// Verify session state was created
	sessionStateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	if _, err := os.Stat(sessionStateFile); err != nil {
		t.Errorf("expected session state file at %s: %v", sessionStateFile, err)
	}
}

// TestAttach_ResearchSession_NoFileChanges tests attaching a research/exploration session
// that didn't modify any files — transcript only.
func TestAttach_ResearchSession_NoFileChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Create a research session — only questions and answers, no tool use.
	sessionID := "attach-research-session"
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("how does the rate limiter work?")
	tb.AddAssistantMessage("The rate limiter uses a token bucket algorithm...")
	tb.AddUserMessage("what about the retry logic?")
	tb.AddAssistantMessage("Retries use exponential backoff with jitter...")
	transcriptPath := filepath.Join(env.ClaudeProjectDir, sessionID+".jsonl")
	if err := tb.WriteToFile(transcriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	output := env.RunCLI("session", "attach", sessionID, "-a", "claude-code", "-f")

	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got:\n%s", output)
	}

	// Verify checkpoint was created and linked
	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if cpID == "" {
		t.Error("expected Entire-Checkpoint trailer on HEAD")
	}
}

// TestAttach_ExistingCheckpoint_AddSession tests attaching a session to a commit
// that already has a checkpoint from a different session. The new session should be
// added to the existing checkpoint (same checkpoint ID, not a second trailer).
func TestAttach_ExistingCheckpoint_AddSession(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// First: run a normal session through hooks to create a checkpoint.
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session1.ID, "add login endpoint"); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("src/login.go", "package main\n\nfunc Login() {}")
	session1.CreateTranscript("add login endpoint", []FileChange{
		{Path: "src/login.go", Content: "package main\n\nfunc Login() {}"},
	})

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit with hooks to trigger condensation and get checkpoint trailer.
	env.GitCommitWithShadowHooks("add login endpoint", "src/login.go")

	// Verify first checkpoint exists
	firstCpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if firstCpID == "" {
		t.Fatal("expected checkpoint on commit after first session")
	}

	// Second: create a research session transcript (no hooks, no file changes).
	session2ID := "attach-second-session"
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("explain the login flow")
	tb.AddAssistantMessage("The login endpoint validates credentials and issues a JWT.")
	transcriptPath := filepath.Join(env.ClaudeProjectDir, session2ID+".jsonl")
	if err := tb.WriteToFile(transcriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Attach the second session
	output := env.RunCLI("session", "attach", session2ID, "-a", "claude-code")

	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Added to existing checkpoint") {
		t.Errorf("expected 'Added to existing checkpoint' in output, got:\n%s", output)
	}

	// Verify only one checkpoint trailer on the commit (same ID reused).
	headMsg := env.GetCommitMessage(env.GetHeadHash())
	allCpIDs := trailers.ParseAllCheckpoints(headMsg)
	if len(allCpIDs) != 1 {
		t.Errorf("expected 1 checkpoint trailer, got %d: %v\nCommit message:\n%s", len(allCpIDs), allCpIDs, headMsg)
	}
	if len(allCpIDs) > 0 && allCpIDs[0].String() != firstCpID {
		t.Errorf("checkpoint ID changed: was %s, now %s", firstCpID, allCpIDs[0].String())
	}
}

// TestAttach_AlreadyTracked_NoCheckpoint tests attaching a session that was tracked
// by hooks (session state exists) but never got a checkpoint (e.g., no file changes
// during the session, so condensation never happened).
func TestAttach_AlreadyTracked_NoCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Start a session through hooks (creates session state).
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session1.ID, "what does this code do?"); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a transcript but don't modify any files.
	session1.CreateTranscript("what does this code do?", nil)

	// Stop — no file changes, so no checkpoint is created.
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Write the transcript to the Claude project dir so attach can find it.
	claudeTranscriptPath := filepath.Join(env.ClaudeProjectDir, session1.ID+".jsonl")
	transcriptData, err := os.ReadFile(session1.TranscriptPath)
	if err != nil {
		t.Fatalf("failed to read transcript: %v", err)
	}
	if err := os.WriteFile(claudeTranscriptPath, transcriptData, 0o600); err != nil {
		t.Fatalf("failed to copy transcript: %v", err)
	}

	// Commit something (unrelated, no hooks) to have a HEAD to amend.
	env.WriteFile("notes.txt", "research notes")
	env.GitAdd("notes.txt")
	env.GitCommit("add research notes")

	// Now attach — session state exists but has no checkpoint.
	output := env.RunCLI("session", "attach", session1.ID, "-a", "claude-code", "-f")

	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Created checkpoint") {
		t.Errorf("expected 'Created checkpoint' in output, got:\n%s", output)
	}

	// Verify session state was updated with checkpoint ID.
	sessionStateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", session1.ID+".json")
	if _, err := os.Stat(sessionStateFile); err != nil {
		t.Errorf("expected session state file: %v", err)
	}
}

// TestAttach_AlreadyTracked_HasCheckpoint tests that re-attaching a session that already
// has a checkpoint just offers to link it (no duplicate checkpoint created).
func TestAttach_AlreadyTracked_HasCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Run a full session: hooks → file changes → commit → condensation.
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session1.ID, "add config parser"); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("config.go", "package config\n\nfunc Parse() {}")
	session1.CreateTranscript("add config parser", []FileChange{
		{Path: "config.go", Content: "package config\n\nfunc Parse() {}"},
	})

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit with hooks to get checkpoint trailer.
	env.GitCommitWithShadowHooks("add config parser", "config.go")

	firstCpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if firstCpID == "" {
		t.Fatal("expected checkpoint on commit")
	}

	// Write transcript to Claude project dir for attach resolution.
	claudeTranscriptPath := filepath.Join(env.ClaudeProjectDir, session1.ID+".jsonl")
	transcriptData, err := os.ReadFile(session1.TranscriptPath)
	if err != nil {
		t.Fatalf("failed to read transcript: %v", err)
	}
	if err := os.WriteFile(claudeTranscriptPath, transcriptData, 0o600); err != nil {
		t.Fatalf("failed to copy transcript: %v", err)
	}

	// Re-attach the same session
	output := env.RunCLI("session", "attach", session1.ID, "-a", "claude-code")

	if !strings.Contains(output, "already has checkpoint") {
		t.Errorf("expected 'already has checkpoint' in output, got:\n%s", output)
	}

	// Verify no duplicate trailer was added
	headMsg := env.GetCommitMessage(env.GetHeadHash())
	allCpIDs := trailers.ParseAllCheckpoints(headMsg)
	if len(allCpIDs) != 1 {
		t.Errorf("expected exactly 1 checkpoint trailer, got %d\nCommit message:\n%s", len(allCpIDs), headMsg)
	}
}

// TestAttach_DifferentWorkingDirectory tests attaching a session whose transcript
// lives under a different project directory (e.g., the agent was started from a
// subdirectory). The fallback search in searchTranscriptInProjectDirs should find it.
func TestAttach_DifferentWorkingDirectory(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// Create a fake HOME with a Claude projects dir containing the transcript
	// under a different project hash than what the CLI would compute for env.RepoDir.
	fakeHome := t.TempDir()
	differentProjectDir := filepath.Join(fakeHome, ".claude", "projects", "different-project-hash")
	if err := os.MkdirAll(differentProjectDir, 0o750); err != nil {
		t.Fatal(err)
	}

	sessionID := "attach-different-dir-session"
	tb := NewTranscriptBuilder()
	tb.AddUserMessage("what is the project structure?")
	tb.AddAssistantMessage("The project has the following structure...")
	if err := tb.WriteToFile(filepath.Join(differentProjectDir, sessionID+".jsonl")); err != nil {
		t.Fatal(err)
	}

	// Set ENTIRE_TEST_CLAUDE_PROJECT_DIR to an empty dir so the primary lookup fails,
	// and set HOME to fakeHome so the fallback search finds our transcript.
	emptyProjectDir := t.TempDir()
	cmd := exec.Command(getTestBinary(), "attach", sessionID, "-a", "claude-code", "-f")
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(),
		"HOME="+fakeHome,
		"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+emptyProjectDir,
	)
	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)
	t.Logf("attach output:\n%s", output)
	if err != nil {
		t.Fatalf("attach failed: %v\nOutput: %s", err, output)
	}

	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Created checkpoint") {
		t.Errorf("expected 'Created checkpoint' in output, got:\n%s", output)
	}
}

// TestAttach_CodexSessionTreeLayout tests attaching a Codex session from the
// CODEX_HOME/sessions/YYYY/MM/DD/ rollout tree using only the session ID.
func TestAttach_CodexSessionTreeLayout(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	codexDir := t.TempDir()
	sessionID := "019d6c43-1537-7343-9691-1f8cee04fe59"
	sessionFile := filepath.Join(codexDir, "2026", "04", "08", "rollout-2026-04-08T10-43-48-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		t.Fatal(err)
	}

	transcript := `{"timestamp":"2026-04-08T10:43:48.000Z","type":"session_meta","payload":{"id":"019d6c43-1537-7343-9691-1f8cee04fe59","timestamp":"2026-04-08T10:43:48.000Z"}}
{"timestamp":"2026-04-08T10:43:49.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"investigate attach failure"}]}}
{"timestamp":"2026-04-08T10:43:50.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking into it."}]}}
`
	if err := os.WriteFile(sessionFile, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(getTestBinary(), "attach", sessionID, "-a", "codex", "-f")
	cmd.Dir = env.RepoDir
	cmd.Env = append(env.cliEnv(),
		"ENTIRE_TEST_CODEX_SESSION_DIR="+codexDir,
	)

	outputBytes, err := cmd.CombinedOutput()
	output := string(outputBytes)
	if err != nil {
		t.Fatalf("attach failed: %v\nOutput: %s", err, output)
	}

	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Created checkpoint") {
		t.Errorf("expected 'Created checkpoint' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Entire-Checkpoint") {
		t.Errorf("expected checkpoint trailer in output, got:\n%s", output)
	}

	sessionStateFile := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	if _, statErr := os.Stat(sessionStateFile); statErr != nil {
		t.Errorf("expected session state file at %s: %v", sessionStateFile, statErr)
	}
}

// TestAttach_InvalidSessionID tests that an invalid session ID is rejected.
func TestAttach_InvalidSessionID(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	_, err := env.RunCLIWithError("session", "attach", "../path-traversal", "-a", "claude-code")
	if err == nil {
		t.Error("expected error for invalid session ID")
	}
}
