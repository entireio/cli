//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestSessionEnd_DetachedCondense exercises the production SessionEnd flow:
// the hook marks the session ENDED inline (fast enough for agents' short
// SessionEnd budgets — Claude Code cancels the hook after ~1.5s) and hands
// the eager condense to a detached __condense_session child that survives
// the hook process being killed. The ENDED mark must be observable as soon
// as the hook returns; the condense lands asynchronously.
func TestSessionEnd_DetachedCondense(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()

	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("detached.go", "package main\n\nfunc Detached() {}\n")
	sess.CreateTranscript("Create detached.go", []FileChange{
		{Path: "detached.go", Content: "package main\n\nfunc Detached() {}\n"},
	})

	// Commit the file BEFORE stopping so FilesTouched is empty at session end
	// and the eager condense actually runs (sessions with FilesTouched defer
	// to PostCommit for carry-forward tracking).
	env.GitAdd("detached.go")
	env.GitCommitWithShadowHooks("Add detached.go", "detached.go")

	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	if err := env.SimulateSessionEndDetached(sess.ID); err != nil {
		t.Fatalf("SimulateSessionEndDetached failed: %v", err)
	}

	// The ENDED mark is synchronous — it must hold the moment the hook returns.
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("session state missing after session-end")
	}
	if state.Phase != session.PhaseEnded {
		t.Fatalf("phase = %s immediately after session-end, want ended", state.Phase)
	}
	if state.EndedAt == nil {
		t.Fatal("EndedAt should be set immediately after session-end")
	}

	// The condense is detached — poll until the child lands it.
	deadline := time.Now().Add(15 * time.Second)
	for {
		state, err = env.GetSessionState(sess.ID)
		if err != nil {
			t.Fatalf("GetSessionState failed while polling: %v", err)
		}
		if state != nil && state.FullyCondensed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached condense did not mark session FullyCondensed within 15s (phase=%s)", state.Phase)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
