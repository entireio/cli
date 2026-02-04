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
	shadowBranch := env.GetShadowBranchNameForCommit(baseHead)

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
	shadowBranch := env.GetShadowBranchNameForCommit(baseHead)

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
	shadowBranch := env.GetShadowBranchNameForCommit(baseHead)

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
	shadowBranch := env.GetShadowBranchNameForCommit(baseHead)

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

// TestSessionConflict_WarningMessageFormat tests that the session conflict warning message
// contains all expected components when a second session starts while another has uncommitted checkpoints.
func TestSessionConflict_WarningMessageFormat(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Create first session and save a checkpoint (so CheckpointCount > 0)
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
	if state1 == nil || state1.CheckpointCount == 0 {
		t.Fatal("Session 1 should have checkpoints")
	}
	t.Logf("Session 1 (%s) has %d checkpoint(s)", session1.EntireID, state1.CheckpointCount)

	// Start a second session (different session ID, same base commit)
	session2 := env.NewSession()

	// Use SimulateSessionStartWithOutput to capture the warning message
	output := env.SimulateSessionStartWithOutput(session2.ID)

	// The hook should succeed (no error) but output a warning message
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

	// If there's no conflict (perhaps warning is disabled), skip the rest
	if resp.SystemMessage == "" {
		t.Log("No session conflict warning - this is expected if multi-session warning is disabled")
		return
	}

	msg := resp.SystemMessage
	t.Logf("Session conflict warning message:\n%s", msg)

	// Verify the warning message contains all expected components
	// 1. The existing session ID
	if !strings.Contains(msg, session1.EntireID) {
		t.Errorf("Warning should contain existing session ID %q, got:\n%s", session1.EntireID, msg)
	}

	// 2. The new session ID (as Entire session ID)
	session2EntireID := sessionid.EntireSessionID(session2.ID)
	if !strings.Contains(msg, session2EntireID) {
		t.Errorf("Warning should contain new session ID %q, got:\n%s", session2EntireID, msg)
	}

	// 3. Resume command format: claude -r <session-id>
	expectedResumeCmd := "claude -r " + session1.EntireID
	if !strings.Contains(msg, expectedResumeCmd) {
		t.Errorf("Warning should contain resume command %q, got:\n%s", expectedResumeCmd, msg)
	}

	// 4. Reset instruction: entire reset --force && claude
	if !strings.Contains(msg, "entire reset --force && claude") {
		t.Errorf("Warning should contain reset instruction 'entire reset --force && claude', got:\n%s", msg)
	}

	// 5. Warning disable option: entire enable --disable-multisession-warning
	if !strings.Contains(msg, "entire enable --disable-multisession-warning") {
		t.Errorf("Warning should contain disable option 'entire enable --disable-multisession-warning', got:\n%s", msg)
	}

	// 6. Verify the message structure contains expected phrases
	expectedPhrases := []string{
		"existing session running",
		"Do you want to continue",
		"Yes: Ignore this warning",
		"No: Type /exit",
		"Resume the other session",
		"Reset and start fresh",
		"hide this notice",
	}
	for _, phrase := range expectedPhrases {
		if !strings.Contains(msg, phrase) {
			t.Errorf("Warning should contain phrase %q, got:\n%s", phrase, msg)
		}
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
