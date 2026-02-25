//go:build integration

package integration

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/session"
)

func TestPiSessionSwitch_EndsPreviousSessionAndStartsNext(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	previous := env.NewPiSession()
	next := env.NewPiSession()

	if err := env.SimulatePiSessionStart(previous.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart(previous) failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(previous.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit(previous) failed: %v", err)
	}
	env.WriteFile("switch.txt", "switch test content\n")
	previous.CreatePiTranscript(
		"create switch file",
		[]FileChange{{Path: "switch.txt", Content: "switch test content\n"}},
	)
	if err := env.SimulatePiStop(previous.ID, previous.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiStop(previous) failed: %v", err)
	}
	if err := env.SimulatePiSessionSwitch(previous.ID, next.ID, previous.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiSessionSwitch failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(next.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit(next) failed: %v", err)
	}

	previousState := loadSessionStateFromRepo(t, env.RepoDir, previous.ID)
	if previousState == nil {
		t.Fatal("expected previous session state to exist")
	}
	if previousState.Phase != session.PhaseEnded {
		t.Fatalf("previous session phase = %q, want %q", previousState.Phase, session.PhaseEnded)
	}
	if previousState.EndedAt == nil {
		t.Fatal("expected previous session EndedAt to be set")
	}

	nextState := loadSessionStateFromRepo(t, env.RepoDir, next.ID)
	if nextState == nil {
		t.Fatal("expected next session state to exist")
	}
	if nextState.Phase != session.PhaseActive {
		t.Fatalf("next session phase = %q, want %q", nextState.Phase, session.PhaseActive)
	}
}

func TestPiSessionFork_EndsParentSessionAndStartsChild(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	parent := env.NewPiSession()
	child := env.NewPiSession()

	if err := env.SimulatePiSessionStart(parent.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart(parent) failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(parent.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit(parent) failed: %v", err)
	}
	env.WriteFile("fork.txt", "fork test content\n")
	parent.CreatePiTranscript(
		"create fork file",
		[]FileChange{{Path: "fork.txt", Content: "fork test content\n"}},
	)
	if err := env.SimulatePiStop(parent.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiStop(parent) failed: %v", err)
	}
	if err := env.SimulatePiSessionFork(parent.ID, child.ID, parent.TranscriptPath); err != nil {
		t.Fatalf("SimulatePiSessionFork failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(child.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit(child) failed: %v", err)
	}

	parentState := loadSessionStateFromRepo(t, env.RepoDir, parent.ID)
	if parentState == nil {
		t.Fatal("expected parent session state to exist")
	}
	if parentState.Phase != session.PhaseEnded {
		t.Fatalf("parent session phase = %q, want %q", parentState.Phase, session.PhaseEnded)
	}
	if parentState.EndedAt == nil {
		t.Fatal("expected parent session EndedAt to be set")
	}

	childState := loadSessionStateFromRepo(t, env.RepoDir, child.ID)
	if childState == nil {
		t.Fatal("expected child session state to exist")
	}
	if childState.Phase != session.PhaseActive {
		t.Fatalf("child session phase = %q, want %q", childState.Phase, session.PhaseActive)
	}
}

func TestPiRewind_ContinueSessionPreservesLatestLeaf(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	piSession := env.NewPiSession()

	if err := env.SimulatePiSessionStart(piSession.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart failed: %v", err)
	}

	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit checkpoint1 failed: %v", err)
	}
	env.WriteFile("rewind-leaf.txt", "v1\n")
	piSession.CreatePiTranscript("create version 1", []FileChange{{Path: "rewind-leaf.txt", Content: "v1\n"}})
	if err := env.SimulatePiStopWithLeaf(piSession.ID, piSession.TranscriptPath, "leaf-1"); err != nil {
		t.Fatalf("SimulatePiStopWithLeaf checkpoint1 failed: %v", err)
	}

	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit checkpoint2 failed: %v", err)
	}
	env.WriteFile("rewind-leaf.txt", "v2\n")
	piSession.CreatePiTranscript("update to version 2", []FileChange{{Path: "rewind-leaf.txt", Content: "v2\n"}})
	if err := env.SimulatePiStopWithLeaf(piSession.ID, piSession.TranscriptPath, "leaf-2"); err != nil {
		t.Fatalf("SimulatePiStopWithLeaf checkpoint2 failed: %v", err)
	}

	points := env.GetRewindPoints()
	if len(points) < 2 {
		t.Fatalf("expected at least 2 rewind points, got %d", len(points))
	}
	oldest := points[len(points)-1]
	if err := env.Rewind(oldest.ID); err != nil {
		t.Fatalf("Rewind failed: %v", err)
	}
	if got := env.ReadFile("rewind-leaf.txt"); got != "v1\n" {
		t.Fatalf("rewind-leaf.txt after rewind = %q, want %q", got, "v1\n")
	}

	if err := env.SimulatePiUserPromptSubmit(piSession.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit after rewind failed: %v", err)
	}
	env.WriteFile("rewind-leaf.txt", "v3\n")
	piSession.CreatePiTranscript("update to version 3", []FileChange{{Path: "rewind-leaf.txt", Content: "v3\n"}})
	if err := env.SimulatePiStopWithLeaf(piSession.ID, piSession.TranscriptPath, "leaf-3"); err != nil {
		t.Fatalf("SimulatePiStopWithLeaf after rewind failed: %v", err)
	}

	stateAfterRewind := loadSessionStateFromRepo(t, env.RepoDir, piSession.ID)
	if stateAfterRewind == nil {
		t.Fatal("expected session state to exist")
	}
	if stateAfterRewind.TranscriptLeafID != "leaf-3" {
		t.Fatalf("TranscriptLeafID = %q, want %q", stateAfterRewind.TranscriptLeafID, "leaf-3")
	}
}
