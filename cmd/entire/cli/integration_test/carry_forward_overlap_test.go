//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestCarryForward_EndedSession_NotCondensedOnUnrelatedCommit verifies that an
// ENDED session with carry-forward files is NOT condensed when an unrelated file
// is committed (GitHub issue #591).
//
// Without eager-condense, sessions with FilesTouched must wait for those specific
// files to be committed. They should NOT be condensed into unrelated commits.
// The eager-condense-on-stop path (CondenseAndMarkFullyCondensed) handles sessions
// with no FilesTouched; sessions with FilesTouched wait for the overlap path.
//
// Scenario:
// 1. Session 1 creates file1.txt and file2.txt
// 2. User commits only file1.txt (partial commit) — session 1 carries forward file2.txt
// 3. Session 1 ends (FilesTouched = ["file2.txt"])
// 4. Session 2 commits unrelated file6.txt — session 1 NOT condensed (no overlap)
// 5. Session 2 IS condensed normally
func TestCarryForward_EndedSession_NotCondensedOnUnrelatedCommit(t *testing.T) {
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
	env.GitCheckoutNewBranch("feature/multi-session-carry-forward")
	env.InitEntire()

	// ========================================
	// Phase 1: Session 1 creates files, partial commit, ends with carry-forward
	// ========================================
	t.Log("Phase 1: Session 1 creates file1.txt and file2.txt")

	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for session1 failed: %v", err)
	}

	env.WriteFile("file1.txt", "content from session 1 - file 1")
	env.WriteFile("file2.txt", "content from session 1 - file 2")
	session1.CreateTranscript(
		"Create file1 and file2",
		[]FileChange{
			{Path: "file1.txt", Content: "content from session 1 - file 1"},
			{Path: "file2.txt", Content: "content from session 1 - file 2"},
		},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for session1 failed: %v", err)
	}

	// Partial commit - only file1.txt
	t.Log("Phase 1b: Partial commit - only file1.txt")
	env.GitAdd("file1.txt")
	env.GitCommitWithShadowHooks("Partial commit: only file1", "file1.txt")

	// End session 1 (simulating user ending session while file2.txt is uncommitted)
	state1, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 failed: %v", err)
	}
	state1.Phase = session.PhaseEnded
	endedAt := time.Now().Add(-2 * time.Hour)
	state1.EndedAt = &endedAt
	if err := env.WriteSessionState(session1.ID, state1); err != nil {
		t.Fatalf("WriteSessionState for session1 failed: %v", err)
	}

	state1, err = env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 failed: %v", err)
	}
	t.Logf("Session1 (ENDED) FilesTouched: %v", state1.FilesTouched)
	originalStepCount := state1.StepCount

	// ========================================
	// Phase 2: NEW session 2 starts and creates file6.txt
	// ========================================
	t.Log("Phase 2: Session 2 starts and creates file6.txt")

	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit for session2 failed: %v", err)
	}

	env.WriteFile("file6.txt", "content from session 2")
	session2.CreateTranscript(
		"Create file6",
		[]FileChange{{Path: "file6.txt", Content: "content from session 2"}},
	)
	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop for session2 failed: %v", err)
	}

	// Set session2 to ACTIVE so PostCommit condenses it
	state2, err := env.GetSessionState(session2.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session2 failed: %v", err)
	}
	state2.Phase = session.PhaseActive
	if err := env.WriteSessionState(session2.ID, state2); err != nil {
		t.Fatalf("WriteSessionState for session2 failed: %v", err)
	}

	// ========================================
	// Phase 3: Commit file6.txt (session 2's file, unrelated to session 1)
	// ========================================
	t.Log("Phase 3: Committing file6.txt from session 2")

	env.GitAdd("file6.txt")
	env.GitCommitWithShadowHooks("Add file6 from session 2", "file6.txt")

	// ========================================
	// Phase 4: Verify session 1 was NOT condensed (no overlap with file2.txt)
	// ========================================
	t.Log("Phase 4: Verifying session 1 (ENDED, no overlap) was NOT condensed")

	state1After, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session1 after session2 commit failed: %v", err)
	}
	if state1After == nil {
		t.Fatal("session1 state file should still exist — was not condensed")
	}

	// Session 1 should still have its carry-forward FilesTouched intact
	if len(state1After.FilesTouched) == 0 {
		t.Errorf("Session 1 FilesTouched should still contain carry-forward files (file2.txt was not committed)")
	}

	// Session 1 should NOT be FullyCondensed
	if state1After.FullyCondensed {
		t.Errorf("Session 1 should NOT be FullyCondensed — file2.txt has not been committed")
	}

	// StepCount should be unchanged — session was not condensed
	if state1After.StepCount != originalStepCount {
		t.Errorf("Session 1 StepCount changed from %d to %d — should be unchanged (not condensed)",
			originalStepCount, state1After.StepCount)
	}

	t.Logf("Session 1 correctly not condensed: StepCount=%d, FilesTouched=%v, FullyCondensed=%v",
		state1After.StepCount, state1After.FilesTouched, state1After.FullyCondensed)

	// ========================================
	// Phase 5: Verify session 2 WAS condensed
	// ========================================
	t.Log("Phase 5: Verifying session 2 WAS condensed")

	finalHead := env.GetHeadHash()
	state2After, err := env.GetSessionState(session2.ID)
	if err != nil {
		t.Fatalf("GetSessionState for session2 after commit failed: %v", err)
	}

	if state2After.BaseCommit != finalHead {
		t.Errorf("Session 2 BaseCommit should be updated. Expected %s, got %s",
			finalHead[:7], state2After.BaseCommit[:7])
	}

	t.Log("Test completed successfully:")
	t.Log("  - Session 1 NOT condensed when unrelated file6.txt committed (carry-forward intact)")
	t.Log("  - Session 2 condensed normally")
}
