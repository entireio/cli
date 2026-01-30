//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/sessionid"
	"entire.io/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestSessionIDConflict_OrphanedBranchIsReset tests that starting a new session
// resets an orphaned shadow branch (one with no session state file).
func TestSessionIDConflict_OrphanedBranchIsReset(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7] + "-1"

	// Create a session and checkpoint (this creates the shadow branch)
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session1) failed: %v", err)
	}

	env.WriteFile("test.txt", "content")
	session1.CreateTranscript("Add test file", []FileChange{{Path: "test.txt", Content: "content"}})
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session1) failed: %v", err)
	}

	// Verify shadow branch exists
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should exist after first session", shadowBranch)
	}
	t.Logf("Created shadow branch: %s", shadowBranch)

	// Clear the session state file but keep the shadow branch
	// This simulates an orphaned shadow branch scenario
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

	// Try to start a new session - should succeed by resetting the orphaned branch
	session2 := env.NewSession()
	err = env.SimulateUserPromptSubmit(session2.ID)
	// Expect success - orphaned branch is reset
	if err != nil {
		t.Errorf("Expected success when starting new session with orphaned shadow branch, got: %v", err)
	}

	// Verify the new session can create checkpoints
	env.WriteFile("test2.txt", "content from session 2")
	session2.CreateTranscript("Add test2 file", []FileChange{{Path: "test2.txt", Content: "content from session 2"}})
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session2) failed: %v", err)
	}

	// Verify shadow branch now has session2's checkpoint
	state2, _ := env.GetSessionState(session2.ID)
	if state2 == nil || state2.CheckpointCount == 0 {
		t.Error("Session 2 should have checkpoints after orphaned branch was reset")
	} else {
		t.Logf("Session 2 has %d checkpoint(s)", state2.CheckpointCount)
	}
}

// TestSessionIDConflict_NoConflictWithSameSession tests that resuming the same session
// (same session ID) does not trigger a conflict error.
func TestSessionIDConflict_NoConflictWithSameSession(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

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

// TestSessionIDConflict_NoShadowBranch tests that starting a new session succeeds
// when no shadow branch exists (fresh start).
func TestSessionIDConflict_NoShadowBranch(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7]

	// Verify no shadow branch exists
	if env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should not exist before first session", shadowBranch)
	}

	// Create a new session - should succeed without conflict
	session := env.NewSession()
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Errorf("Starting new session with no shadow branch should succeed, got: %v", err)
	}
}

// TestSessionIDConflict_ManuallyCreatedOrphanedBranch tests that a manually created
// orphaned shadow branch (simulating a crash scenario) is reset when a new session starts.
func TestSessionIDConflict_ManuallyCreatedOrphanedBranch(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7]

	// Manually create a shadow branch with a different session ID
	// This simulates a shadow branch that was left behind (e.g., from a crash)
	createOrphanedShadowBranch(t, env.RepoDir, shadowBranch, "orphaned-session-id")

	// Verify shadow branch exists
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should exist after manual creation", shadowBranch)
	}

	// Try to start a new session - should succeed by resetting the orphaned branch
	session := env.NewSession()
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Errorf("Expected success when orphaned shadow branch is reset, got: %v", err)
	}

	// Verify the new session can create checkpoints
	env.WriteFile("new_file.txt", "new content")
	session.CreateTranscript("Add new file", []FileChange{{Path: "new_file.txt", Content: "new content"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify session has checkpoints
	state, _ := env.GetSessionState(session.ID)
	if state == nil || state.CheckpointCount == 0 {
		t.Error("Session should have checkpoints after orphaned branch was reset")
	} else {
		t.Logf("New session has %d checkpoint(s)", state.CheckpointCount)
	}
}

// TestSessionIDConflict_ExistingSessionWithState tests that when a shadow branch exists
// from a different session AND that session has a state file (not orphaned), a blocking
// hook response is returned. This simulates the cross-worktree scenario.
func TestSessionIDConflict_ExistingSessionWithState(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	// Enable multi-session warning to test conflict detection
	env.InitEntireWithOptions(strategy.StrategyNameManualCommit, map[string]any{
		"enable_multisession_warning": true,
	})

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7]

	// Create a shadow branch with a specific session ID
	otherSessionID := "other-session-id"
	createOrphanedShadowBranch(t, env.RepoDir, shadowBranch, otherSessionID)

	// Verify shadow branch exists
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should exist after creation", shadowBranch)
	}

	// Manually create a state file for the other session (simulating cross-worktree scenario)
	// This makes the shadow branch NOT orphaned
	entireOtherSessionID := sessionid.EntireSessionID(otherSessionID)
	otherState := &strategy.SessionState{
		SessionID:       entireOtherSessionID,
		BaseCommit:      baseHead,
		WorktreePath:    "/some/other/worktree", // Different worktree
		CheckpointCount: 1,
	}
	// Write state file directly to test repo (can't use strategy.SaveSessionState as it uses cwd)
	stateDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("Failed to create session state dir: %v", err)
	}
	stateData, err := json.Marshal(otherState)
	if err != nil {
		t.Fatalf("Failed to marshal session state: %v", err)
	}
	stateFile := filepath.Join(stateDir, entireOtherSessionID+".json")
	if err := os.WriteFile(stateFile, stateData, 0o644); err != nil {
		t.Fatalf("Failed to write session state file: %v", err)
	}

	// Verify state file exists
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("State file should exist: %v", err)
	}

	// Try to start a new session - should return blocking response (not error)
	session := env.NewSession()
	hookResp, err := env.SimulateUserPromptSubmitWithResponse(session.ID)
	// After the fix, the hook should succeed (no error) but return blocking response
	if err != nil {
		t.Errorf("Hook should not error (should block via JSON response), got: %v", err)
	}

	// Verify the hook response blocks and contains expected message
	if hookResp == nil {
		t.Fatal("Expected hook response, got nil")
	}
	if hookResp.Continue {
		t.Error("Expected hook to block (Continue: false)")
	}
	if !strings.Contains(hookResp.StopReason, "Session ID conflict") {
		t.Errorf("Expected 'Session ID conflict' in stop reason, got: %s", hookResp.StopReason)
	}
	if !strings.Contains(hookResp.StopReason, shadowBranch) {
		t.Errorf("Expected shadow branch %s in message, got: %s", shadowBranch, hookResp.StopReason)
	}
	t.Logf("Got expected blocking response: %s", hookResp.StopReason)
}

// createOrphanedShadowBranch creates a shadow branch with a specific session ID
// without creating a corresponding session state file.
func createOrphanedShadowBranch(t *testing.T, repoDir, branchName, sessionID string) {
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

	// Create an Entire session ID with date prefix
	entireSessionID := sessionid.EntireSessionID(sessionID)

	// Create commit message with Entire-Session trailer
	commitMsg := "Orphaned checkpoint\n\n" +
		"Entire-Session: " + entireSessionID + "\n" +
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

// TestSessionIDConflict_ShadowBranchWithoutTrailer tests that a shadow branch without
// an Entire-Session trailer does not cause a conflict (backwards compatibility).
func TestSessionIDConflict_ShadowBranchWithoutTrailer(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7]

	// Create a shadow branch without Entire-Session trailer (simulating old format)
	createShadowBranchWithoutTrailer(t, env.RepoDir, shadowBranch)

	// Verify shadow branch exists
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should exist", shadowBranch)
	}

	// Starting a new session should succeed (no trailer = no conflict)
	session := env.NewSession()
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Errorf("Starting session with shadow branch without trailer should succeed, got: %v", err)
	}
}

// createShadowBranchWithoutTrailer creates a shadow branch without an Entire-Session trailer.
func createShadowBranchWithoutTrailer(t *testing.T, repoDir, branchName string) {
	t.Helper()

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("Failed to open repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Failed to get HEAD: %v", err)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("Failed to get HEAD commit: %v", err)
	}

	// Create commit without Entire-Session trailer
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
		Message:  "Legacy checkpoint without session trailer",
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

	refName := plumbing.NewBranchReferenceName(branchName)
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("Failed to create branch reference: %v", err)
	}
}
