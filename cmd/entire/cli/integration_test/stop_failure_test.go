//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestStopFailure_EndsTurn verifies that a Claude Code turn ending on an API
// error (StopFailure, fired instead of Stop) ends the turn: the session leaves
// ACTIVE for IDLE and the turn's work is saved as a step, instead of staying
// ACTIVE until the next prompt.
func TestStopFailure_EndsTurn(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState after prompt: state=%v err=%v", state, err)
	}
	if state.Phase != session.PhaseActive {
		t.Fatalf("Phase after prompt = %q, want %q", state.Phase, session.PhaseActive)
	}

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")
	sess.CreateTranscript("Create feature function", []FileChange{
		{Path: "feature.go", Content: "package main\n\nfunc Feature() {}\n"},
	})
	if err := env.SimulateStopFailure(sess.ID, sess.TranscriptPath, "rate_limit"); err != nil {
		t.Fatalf("stop-failure failed: %v", err)
	}

	state, err = env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState after stop-failure: state=%v err=%v", state, err)
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Phase after stop-failure = %q, want %q", state.Phase, session.PhaseIdle)
	}
	if state.StepCount != 1 {
		t.Errorf("StepCount after stop-failure = %d, want 1", state.StepCount)
	}
}
