//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestSessionIDConflict_ClearedSessionDoesNotBlockNewSession tests that a new
// session starts and records turn-end work after an earlier session's state
// file was removed.
func TestSessionIDConflict_ClearedSessionDoesNotBlockNewSession(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

	// Create a session and checkpoint
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session1) failed: %v", err)
	}

	env.WriteFile("test.txt", "content")
	session1.CreateTranscript("Add test file", []FileChange{{Path: "test.txt", Content: "content"}})
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session1) failed: %v", err)
	}

	env.AssertTurnEndRecorded(session1.ID, "test.txt")

	// Clear the session state file
	sessionStateDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	entries, err := os.ReadDir(sessionStateDir)
	if err != nil {
		t.Fatalf("Failed to read session state dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			if err := os.Remove(filepath.Join(sessionStateDir, entry.Name())); err != nil {
				t.Fatalf("Failed to remove session state file: %v", err)
			}
		}
	}

	// Start a new session
	session2 := env.NewSession()
	err = env.SimulateUserPromptSubmit(session2.ID)
	if err != nil {
		t.Errorf("Expected success when starting a new session, got: %v", err)
	}

	// Verify the new session can create checkpoints
	env.WriteFile("test2.txt", "content from session 2")
	session2.CreateTranscript("Add test2 file", []FileChange{{Path: "test2.txt", Content: "content from session 2"}})
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session2) failed: %v", err)
	}

	// Verify session2 recorded its turn end
	env.AssertTurnEndRecorded(session2.ID, "test2.txt")
}

// TestSessionIDConflict_NoConflictWithSameSession tests that resuming the same session
// (same session ID) does not trigger a conflict error.
func TestSessionIDConflict_NoConflictWithSameSession(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

	// Create a session and checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("test.txt", "content")
	session.CreateTranscript("Add test file", []FileChange{{Path: "test.txt", Content: "content"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Try to "resume" the same session (same ID) - should not error
	// This simulates Claude resuming with the same session ID
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Errorf("Resuming same session should not error, got: %v", err)
	}
}

// TestSessionStart_RemovesLegacyShadowBranches verifies the one-time upgrade
// cleanup: the first session start deletes the per-session shadow branches
// older CLIs wrote (entire/<commit>-<worktree-hash>), leaves anything that only
// looks similar (the bare entire/<hex> form, the metadata branch) alone, and
// records a marker so later session starts skip the ref scan.
func TestSessionStart_RemovesLegacyShadowBranches(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

	baseHead := env.GetHeadHash()
	legacy := "entire/" + baseHead[:7] + "-e3b0c4"
	bareLookalike := "entire/" + baseHead[:7]
	createLegacyShadowBranch(t, env.RepoDir, legacy, "legacy-session-id")
	createLegacyShadowBranch(t, env.RepoDir, bareLookalike, "not-ours")

	session := env.NewSession()
	if out := env.SimulateSessionStartWithOutput(session.ID); out.Err != nil {
		t.Fatalf("session-start failed: %v\n%s", out.Err, out.Stderr)
	}

	if env.BranchExists(legacy) {
		t.Errorf("legacy shadow branch %s should be removed at session start", legacy)
	}
	if !env.BranchExists(bareLookalike) {
		t.Errorf("bare-format branch %s must be left for `entire clean --all` to confirm", bareLookalike)
	}
	marker := filepath.Join(env.RepoDir, ".git", "entire-legacy-shadow-branches-removed")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("expected the one-time cleanup marker at %s: %v", marker, err)
	}

	// One-time: a branch appearing later is not touched by the next start.
	createLegacyShadowBranch(t, env.RepoDir, legacy, "legacy-session-id")
	if out := env.SimulateSessionStartWithOutput(session.ID); out.Err != nil {
		t.Fatalf("second session-start failed: %v\n%s", out.Err, out.Stderr)
	}
	if !env.BranchExists(legacy) {
		t.Errorf("legacy cleanup should run once; %s was deleted again", legacy)
	}
}

// createLegacyShadowBranch creates a branch shaped like the per-session shadow
// branches older CLIs wrote, with no corresponding session state file.
func createLegacyShadowBranch(t *testing.T, repoDir, branchName, sessionID string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("Failed to open repo: %v", err)
	}

	// Get HEAD commit to use as parent/tree
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Failed to get HEAD: %v", err)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("Failed to get HEAD commit: %v", err)
	}

	// Create commit message with Entire-Session trailer
	commitMsg := "Orphaned checkpoint\n\n" +
		"Entire-Session: " + sessionID + "\n" +
		"Entire-Strategy: manual-commit\n"

	// Create the commit
	commit := &object.Commit{
		Author: object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
		Committer: object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
		Message:  commitMsg,
		TreeHash: headCommit.TreeHash,
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("Failed to encode commit: %v", err)
	}

	commitHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("Failed to store commit: %v", err)
	}

	// Create the branch reference
	refName := plumbing.NewBranchReferenceName(branchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("Failed to create branch reference: %v", err)
	}
}

// TestSessionStart_InformationalMessage tests that the session start informational message
// contains the expected base text and concurrent session count when another session has uncommitted checkpoints.
func TestSessionStart_InformationalMessage(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

	// Create first session and save a checkpoint (so StepCount > 0)
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session1) failed: %v", err)
	}

	env.WriteFile("test.txt", "content from session 1")
	session1.CreateTranscript("Add test file", []FileChange{{Path: "test.txt", Content: "content from session 1"}})
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session1) failed: %v", err)
	}

	// Verify session1 has checkpoints
	state1, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("Failed to get session1 state: %v", err)
	}
	if state1 == nil || state1.StepCount == 0 {
		t.Fatal("Session 1 should have checkpoints")
	}
	t.Logf("Session 1 (%s) has %d checkpoint(s)", session1.ID, state1.StepCount)

	// Start a second session (different session ID, same base commit)
	session2 := env.NewSession()

	// Use SimulateSessionStartWithOutput to capture the informational message
	output := env.SimulateSessionStartWithOutput(session2.ID)

	// The hook should succeed (no error) and output an informational message
	if output.Err != nil {
		t.Fatalf("SimulateSessionStart (session2) failed: %v\nStderr: %s", output.Err, output.Stderr)
	}

	// Parse the JSON response
	type sessionStartResponse struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}
	var resp sessionStartResponse
	if len(output.Stdout) > 0 {
		if err := json.Unmarshal(output.Stdout, &resp); err != nil {
			t.Fatalf("Failed to parse session-start response: %v\nStdout: %s", err, output.Stdout)
		}
	}

	if resp.SystemMessage == "" {
		t.Fatal("Expected informational message in systemMessage, got empty")
	}

	msg := resp.SystemMessage
	t.Logf("Session start message:\n%s", msg)

	// Verify base informational message is present
	if !strings.Contains(msg, "Entire CLI") {
		t.Errorf("Message should contain 'Entire CLI', got:\n%s", msg)
	}
	if !strings.Contains(msg, "link this conversation to your next commit") {
		t.Errorf("Message should contain 'link this conversation to your next commit', got:\n%s", msg)
	}

	// Verify concurrent session count is shown
	if !strings.Contains(msg, "1 other active conversation(s) in this workspace") {
		t.Errorf("Message should contain '1 other active conversation(s) in this workspace', got:\n%s", msg)
	}

	// Verify old warning phrases are NOT present
	oldPhrases := []string{
		"existing session running",
		"Do you want to continue",
		"Ignore this warning",
		"/exit",
		"Resume the other session",
		"Reset and start fresh",
		"disable-multisession-warning",
	}
	for _, phrase := range oldPhrases {
		if strings.Contains(msg, phrase) {
			t.Errorf("Message should NOT contain old warning phrase %q, got:\n%s", phrase, msg)
		}
	}
}

// TestSessionStart_InformationalMessageNoConcurrentSessions tests that the base informational message
// is shown even when there are no concurrent sessions.
func TestSessionStart_InformationalMessageNoConcurrentSessions(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

	// Start a single session (no other sessions)
	session1 := env.NewSession()

	// Use SimulateSessionStartWithOutput to capture the informational message
	output := env.SimulateSessionStartWithOutput(session1.ID)

	// The hook should succeed
	if output.Err != nil {
		t.Fatalf("SimulateSessionStart failed: %v\nStderr: %s", output.Err, output.Stderr)
	}

	// Parse the JSON response
	type sessionStartResponse struct {
		SystemMessage string `json:"systemMessage,omitempty"`
	}
	var resp sessionStartResponse
	if len(output.Stdout) > 0 {
		if err := json.Unmarshal(output.Stdout, &resp); err != nil {
			t.Fatalf("Failed to parse session-start response: %v\nStdout: %s", err, output.Stdout)
		}
	}

	if resp.SystemMessage == "" {
		t.Fatal("Expected informational message in systemMessage, got empty")
	}

	msg := resp.SystemMessage
	t.Logf("Session start message:\n%s", msg)

	// Verify base informational message is present
	if !strings.Contains(msg, "Entire CLI") {
		t.Errorf("Message should contain 'Entire CLI', got:\n%s", msg)
	}
	if !strings.Contains(msg, "link this conversation to your next commit") {
		t.Errorf("Message should contain 'link this conversation to your next commit', got:\n%s", msg)
	}

	// Verify concurrent session info is NOT shown (no other sessions)
	if strings.Contains(msg, "other active conversation") {
		t.Errorf("Message should NOT mention other active conversations when none exist, got:\n%s", msg)
	}
}
