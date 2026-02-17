//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestOldIdleSession_BaseCommitNotUpdated verifies that when an old IDLE session
// exists and a new ACTIVE session makes a commit, the old IDLE session's BaseCommit
// is NOT updated to the new HEAD.
//
// This is a regression test for the bug where old sessions (IDLE/ENDED) would
// have their BaseCommit incorrectly updated, causing them to be condensed on
// future commits because their BaseCommit matched the new shadow branch.
//
// Scenario:
// 1. Create an old session (session1), run full workflow, set to IDLE
// 2. Make an unrelated commit to move HEAD forward (simulating time passing)
// 3. Create a new session (session2), make changes
// 4. Commit from session2
// 5. Verify session1's BaseCommit was NOT updated
func TestOldIdleSession_BaseCommitNotUpdated(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// ========================================
	// Setup
	// ========================================
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test-base-commit")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// ========================================
	// Phase 1: Create first session (will become IDLE)
	// ========================================
	t.Log("Phase 1: Creating first session that will become IDLE")

	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for session1 failed: %v", err)
	}

	// Create a file and checkpoint
	env.WriteFile("file1.txt", "content from session 1")
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "file1.txt", Content: "content from session 1"}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for session1 failed: %v", err)
	}

	// Verify session1 is IDLE
	state1, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 failed: %v", err)
	}
	if state1.Phase != session.PhaseIdle {
		t.Fatalf("Expected session1 to be IDLE, got %s", state1.Phase)
	}

	// Record session1's BaseCommit BEFORE the unrelated commit
	session1OriginalBaseCommit := state1.BaseCommit
	t.Logf("Session1 original BaseCommit: %s", session1OriginalBaseCommit[:7])

	// ========================================
	// Phase 2: Make an unrelated commit to move HEAD forward
	// ========================================
	t.Log("Phase 2: Making unrelated commit to move HEAD forward")

	env.WriteFile("unrelated.txt", "unrelated file")
	env.GitAdd("unrelated.txt")
	env.GitCommit("Unrelated commit without checkpoint trailer")

	newHeadAfterUnrelated := env.GetHeadHash()
	t.Logf("HEAD after unrelated commit: %s", newHeadAfterUnrelated[:7])

	// ========================================
	// Phase 3: Create second session (ACTIVE)
	// ========================================
	t.Log("Phase 3: Creating second session")

	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for session2 failed: %v", err)
	}

	// Create a file and checkpoint for session2
	env.WriteFile("file2.txt", "content from session 2")
	session2.CreateTranscript(
		"Create file2",
		[]FileChange{{Path: "file2.txt", Content: "content from session 2"}},
	)
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for session2 failed: %v", err)
	}

	// Set session2 to ACTIVE (simulating agent mid-turn)
	state2, err := env.GetSessionState(session2.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session2 failed: %v", err)
	}
	state2.Phase = session.PhaseActive
	if err := env.WriteSessionState(session2.ID, state2); err != nil {
		t.Fatalf("WriteSessionState for session2 failed: %v", err)
	}

	// ========================================
	// Phase 4: Commit from session2 (triggers PostCommit)
	// ========================================
	t.Log("Phase 4: Committing from session2")

	env.GitAdd("file2.txt")
	env.GitCommitWithShadowHooks("Commit from session2", "file2.txt")

	finalHead := env.GetHeadHash()
	t.Logf("Final HEAD after session2 commit: %s", finalHead[:7])

	// ========================================
	// Phase 5: Verify session1's BaseCommit was NOT updated
	// ========================================
	t.Log("Phase 5: Verifying session1's BaseCommit was NOT updated")

	state1After, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 after commit failed: %v", err)
	}

	if state1After.BaseCommit != session1OriginalBaseCommit {
		t.Errorf("OLD IDLE session's BaseCommit was incorrectly updated!\n"+
			"Expected: %s (original)\n"+
			"Got:      %s (new HEAD)",
			session1OriginalBaseCommit[:7], state1After.BaseCommit[:7])
	}

	if state1After.BaseCommit == finalHead {
		t.Errorf("OLD IDLE session's BaseCommit incorrectly matches new HEAD: %s", finalHead[:7])
	}

	// Session2's BaseCommit SHOULD be updated (it was the active session)
	state2After, err := env.GetSessionState(session2.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session2 after commit failed: %v", err)
	}
	if state2After.BaseCommit != finalHead {
		t.Errorf("NEW ACTIVE session's BaseCommit should be updated to new HEAD\n"+
			"Expected: %s\n"+
			"Got:      %s",
			finalHead[:7], state2After.BaseCommit[:7])
	}

	t.Log("Test completed")
}

// TestOldEndedSession_BaseCommitNotUpdated verifies that when an old ENDED session
// exists and a new ACTIVE session makes a commit, the old ENDED session's BaseCommit
// is NOT updated to the new HEAD.
func TestOldEndedSession_BaseCommitNotUpdated(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// ========================================
	// Setup
	// ========================================
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test-ended-base-commit")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// ========================================
	// Phase 1: Create first session and END it
	// ========================================
	t.Log("Phase 1: Creating first session that will be ENDED")

	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for session1 failed: %v", err)
	}

	// Create a file and checkpoint
	env.WriteFile("file1.txt", "content from session 1")
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "file1.txt", Content: "content from session 1"}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for session1 failed: %v", err)
	}

	// Set session1 to ENDED with no new content (simulating already-condensed session)
	state1, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 failed: %v", err)
	}
	state1.Phase = session.PhaseEnded
	// Mark transcript as fully condensed (no new content since last checkpoint).
	// Set to a high value to ensure transcriptLines <= CheckpointTranscriptStart.
	state1.CheckpointTranscriptStart = 1000
	// Clear FilesTouched to simulate a session that was fully condensed
	state1.FilesTouched = nil
	if err := env.WriteSessionState(session1.ID, state1); err != nil {
		t.Fatalf("WriteSessionState for session1 failed: %v", err)
	}

	// Record session1's BaseCommit BEFORE the unrelated commit
	session1OriginalBaseCommit := state1.BaseCommit
	t.Logf("Session1 original BaseCommit: %s", session1OriginalBaseCommit[:7])

	// ========================================
	// Phase 2: Make an unrelated commit to move HEAD forward
	// ========================================
	t.Log("Phase 2: Making unrelated commit to move HEAD forward")

	env.WriteFile("unrelated.txt", "unrelated file")
	env.GitAdd("unrelated.txt")
	env.GitCommit("Unrelated commit without checkpoint trailer")

	// ========================================
	// Phase 3: Create second session (ACTIVE)
	// ========================================
	t.Log("Phase 3: Creating second session")

	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for session2 failed: %v", err)
	}

	// Create a file and checkpoint for session2
	env.WriteFile("file2.txt", "content from session 2")
	session2.CreateTranscript(
		"Create file2",
		[]FileChange{{Path: "file2.txt", Content: "content from session 2"}},
	)
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for session2 failed: %v", err)
	}

	// Set session2 to ACTIVE
	state2, err := env.GetSessionState(session2.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session2 failed: %v", err)
	}
	state2.Phase = session.PhaseActive
	if err := env.WriteSessionState(session2.ID, state2); err != nil {
		t.Fatalf("WriteSessionState for session2 failed: %v", err)
	}

	// ========================================
	// Phase 4: Commit from session2
	// ========================================
	t.Log("Phase 4: Committing from session2")

	env.GitAdd("file2.txt")
	env.GitCommitWithShadowHooks("Commit from session2", "file2.txt")

	finalHead := env.GetHeadHash()

	// ========================================
	// Phase 5: Verify session1's BaseCommit was NOT updated
	// ========================================
	t.Log("Phase 5: Verifying session1's BaseCommit was NOT updated")

	state1After, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 after commit failed: %v", err)
	}

	if state1After.BaseCommit != session1OriginalBaseCommit {
		t.Errorf("OLD ENDED session's BaseCommit was incorrectly updated!\n"+
			"Expected: %s (original)\n"+
			"Got:      %s (new HEAD)",
			session1OriginalBaseCommit[:7], state1After.BaseCommit[:7])
	}

	if state1After.BaseCommit == finalHead {
		t.Errorf("OLD ENDED session's BaseCommit incorrectly matches new HEAD: %s", finalHead[:7])
	}

	t.Log("Test completed")
}
