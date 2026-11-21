//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	env.GitCommitWithHooks("Add feature", "feature.go")

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

// TestFullyCondensed_SweptSessionStaysEnded covers a session that ends with
// files no commit picks up. A day later the background sweep condenses it. It
// must stay ENDED and become FullyCondensed, the pair PrepareCommitMsg and
// PostCommit skip, not turn IDLE, which they would check on every commit.
// Resuming it must clear the flag and EndedAt, as leaving ENDED does and
// leaving IDLE does not, so a commit links the new turn's work.
func TestFullyCondensed_SweptSessionStaysEnded(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()

	require.NoError(t, env.SimulateUserPromptSubmit(sess.ID))
	env.WriteFile("draft.go", "package main\n")
	sess.CreateTranscript("Draft a file", []FileChange{{Path: "draft.go", Content: "package main\n"}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
	require.NoError(t, env.SimulateSessionEnd(sess.ID))

	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseEnded, state.Phase)
	require.False(t, state.FullyCondensed, "fixture: files waiting for a commit keep the session from condensing at its end")

	// The sweep leaves a session alone for a day after it ends.
	endedAt := time.Now().Add(-48 * time.Hour)
	state.EndedAt = &endedAt
	require.NoError(t, env.WriteSessionState(sess.ID, state))

	env.RunCLI("__sweep_sessions")

	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.False(t, state.LastCheckpointID.IsEmpty(), "the sweep should have condensed the session")
	require.Equal(t, session.PhaseEnded, state.Phase, "a swept session must stay ENDED")
	require.True(t, state.FullyCondensed, "a swept session must be FullyCondensed")
	require.NotNil(t, state.EndedAt)
	require.False(t, state.HasPendingWork())
	sweepCheckpoint := state.LastCheckpointID

	// Resume: the next turn's work must reach the commit that carries it.
	require.NoError(t, env.SimulateUserPromptSubmit(sess.ID))
	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActive, state.Phase)
	require.False(t, state.FullyCondensed, "resuming must clear FullyCondensed")
	require.Nil(t, state.EndedAt, "resuming must clear EndedAt, or the live session still counts as ended")

	env.WriteFile("resumed.go", "package main\n")
	sess.CreateTranscript("Add another file", []FileChange{{Path: "resumed.go", Content: "package main\n"}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
	env.GitCommitWithHooks("Add resumed.go", "resumed.go")

	commitCheckpoint := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, commitCheckpoint, "the resumed turn's commit should carry a checkpoint trailer")
	state, err = env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, commitCheckpoint, state.LastCheckpointID.String(), "the commit should have condensed the resumed session")
	require.NotEqual(t, sweepCheckpoint, state.LastCheckpointID)
}
