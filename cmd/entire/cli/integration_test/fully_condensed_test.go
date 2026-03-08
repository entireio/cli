//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestFullyCondensed_ReactivationClearsFlag tests that when a fully-condensed
// ENDED session is reactivated (new UserPromptSubmit), the FullyCondensed flag
// is cleared so the session is processed normally on future commits.
//
// This is the critical safety test: if FullyCondensed isn't cleared on
// reactivation, the session's new work would be silently skipped in PostCommit.
//
// State machine transitions tested:
//   - IDLE + SessionStop -> ENDED
//   - ENDED + GitCommit -> ENDED + ActionCondenseIfFilesTouched (sets FullyCondensed)
//   - ENDED + TurnStart -> ACTIVE + ActionClearEndedAt (clears FullyCondensed)
func TestFullyCondensed_ReactivationClearsFlag(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// ========================================
	// Phase 1: Create session, do work, stop, end, then commit → FullyCondensed
	// ========================================
	t.Log("Phase 1: Run a full session lifecycle and commit after ending")

	sess := env.NewSession()

	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")
	sess.CreateTranscript("Create feature function", []FileChange{
		{Path: "feature.go", Content: "package main\n\nfunc Feature() {}\n"},
	})

	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify IDLE with files touched
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseIdle {
		t.Fatalf("Expected IDLE after stop, got %s", state.Phase)
	}
	if len(state.FilesTouched) == 0 {
		t.Fatal("FilesTouched should be non-empty after agent work")
	}

	// End the session BEFORE committing — FullyCondensed is only set for ENDED sessions
	if err := env.SimulateSessionEnd(sess.ID); err != nil {
		t.Fatalf("SimulateSessionEnd failed: %v", err)
	}

	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseEnded {
		t.Fatalf("Expected ENDED after session end, got %s", state.Phase)
	}

	// Commit the work — PostCommit condenses the ENDED session with files touched,
	// all files are committed so no carry-forward remains → FullyCondensed = true
	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	// Verify ENDED with FullyCondensed
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseEnded {
		t.Fatalf("Expected ENDED after commit, got %s", state.Phase)
	}
	if !state.FullyCondensed {
		t.Fatal("Session should be FullyCondensed after condensation with no carry-forward")
	}

	// ========================================
	// Phase 2: Reactivate the session → FullyCondensed should be cleared
	// ========================================
	t.Log("Phase 2: Reactivate the ended session")

	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("user-prompt-submit (reactivation) failed: %v", err)
	}

	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Expected ACTIVE after reactivation, got %s", state.Phase)
	}
	if state.FullyCondensed {
		t.Error("FullyCondensed must be cleared on reactivation — " +
			"otherwise new work would be silently skipped in PostCommit")
	}
}
