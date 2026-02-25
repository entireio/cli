//go:build integration

package integration

import (
	"testing"
)

func TestPiStopHook_PersistsLeafIDToSessionState(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	session := env.NewPiSession()

	if err := env.SimulatePiSessionStart(session.ID); err != nil {
		t.Fatalf("SimulatePiSessionStart failed: %v", err)
	}
	if err := env.SimulatePiUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulatePiUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("leaf.txt", "content for leaf test\n")
	session.CreatePiTranscript(
		"create leaf file",
		[]FileChange{{Path: "leaf.txt", Content: "content for leaf test\n"}},
	)

	const leafID = "leaf-active-123"
	if err := env.SimulatePiStopWithLeaf(session.ID, session.TranscriptPath, leafID); err != nil {
		t.Fatalf("SimulatePiStopWithLeaf failed: %v", err)
	}

	state := loadSessionStateFromRepo(t, env.RepoDir, session.ID)
	if state == nil {
		t.Fatal("expected session state to exist")
	}
	if state.TranscriptLeafID != leafID {
		t.Fatalf("TranscriptLeafID = %q, want %q", state.TranscriptLeafID, leafID)
	}
}
