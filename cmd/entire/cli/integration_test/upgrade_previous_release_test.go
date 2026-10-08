//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpgrade_PreviousReleaseSessionCommitsAndLinks starts from what a
// previous release leaves behind and runs the new CLI over it:
//
//   - session state with a turn-end step (StepCount > 0) and FilesTouched but
//     no touched_file_hashes key, as the old format stored it;
//   - a legacy-shape shadow branch entire/<commit>-<worktree>;
//   - the session's existing .entire/metadata (full.jsonl and prompt.txt).
//
// A new turn ends on the upgraded CLI, then the user commits the old turn's
// file first (a partial commit) and the new turn's file second. Both commits
// must link the session with distinct checkpoints, the second checkpoint must
// carry the file still pending after the first, and the legacy branch must be
// left alone: nothing deletes it automatically.
func TestUpgrade_PreviousReleaseSessionCommitsAndLinks(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()

	// --- Previous release: one turn that created old.go, then a Stop. ---
	if err := env.SimulateUserPromptSubmitWithPrompt(sess.ID, "Create the old file"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}
	env.WriteFile("old.go", "package main\n\nfunc Old() {}\n")
	sess.CreateTranscript("Create the old file", []FileChange{
		{Path: "old.go", Content: "package main\n\nfunc Old() {}\n"},
	})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Rewrite the state in the previous release's format: no recorded hashes.
	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState failed: state=%v err=%v", state, err)
	}
	if state.StepCount == 0 || len(state.FilesTouched) == 0 {
		t.Fatalf("fixture: expected a recorded turn, got StepCount=%d FilesTouched=%v", state.StepCount, state.FilesTouched)
	}
	state.TouchedFileHashes = nil
	if err := env.WriteSessionState(sess.ID, state); err != nil {
		t.Fatalf("WriteSessionState failed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "entire-sessions", sess.ID+".json"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(raw), "touched_file_hashes") {
		t.Fatal("fixture: the previous-release state must not carry touched_file_hashes")
	}

	// The previous release's local metadata stays where it left it.
	metadataDir := filepath.Join(env.RepoDir, ".entire", "metadata", sess.ID)
	for _, name := range []string{"full.jsonl", "prompt.txt"} {
		if _, statErr := os.Stat(filepath.Join(metadataDir, name)); statErr != nil {
			t.Fatalf("fixture: expected existing %s: %v", name, statErr)
		}
	}

	// And its shadow branch for the session's base commit.
	legacyBranch := "entire/" + env.GetHeadHash()[:7] + "-e3b0c4"
	createLegacyShadowBranch(t, env.RepoDir, legacyBranch, sess.ID)

	// --- Upgraded CLI: a new turn creates new.go. ---
	if err := env.SimulateUserPromptSubmitWithPrompt(sess.ID, "Create the new file"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (upgraded) failed: %v", err)
	}
	env.WriteFile("new.go", "package main\n\nfunc New() {}\n")
	sess.CreateTranscript("Create the new file", []FileChange{
		{Path: "new.go", Content: "package main\n\nfunc New() {}\n"},
	})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (upgraded) failed: %v", err)
	}

	// Partial commit: only the previous release's file.
	env.GitCommitWithHooks("Add old file", "old.go")
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if firstCheckpointID == "" {
		t.Fatal("first commit (the previous release's file) should link the session")
	}

	state, err = env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState after first commit failed: state=%v err=%v", state, err)
	}
	if !containsString(state.FilesTouched, "new.go") {
		t.Fatalf("new.go should still be pending after the partial commit, FilesTouched=%v", state.FilesTouched)
	}

	// Final commit: the upgraded turn's file.
	env.GitCommitWithHooks("Add new file", "new.go")
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if secondCheckpointID == "" {
		t.Fatal("final commit should link the session")
	}
	if secondCheckpointID == firstCheckpointID {
		t.Fatalf("the final commit reused the first checkpoint ID %s", firstCheckpointID)
	}

	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID: firstCheckpointID,
		SessionID:    sess.ID,
		FilesTouched: []string{"old.go"},
	})
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID: secondCheckpointID,
		SessionID:    sess.ID,
		FilesTouched: []string{"new.go"},
	})

	if !env.BranchExists(legacyBranch) {
		t.Errorf("legacy shadow branch %s must be left alone", legacyBranch)
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
