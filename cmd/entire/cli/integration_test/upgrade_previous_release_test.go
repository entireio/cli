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

	legacyBranch := setupPreviousReleaseSession(t, env, sess)

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

	state, err := env.GetSessionState(sess.ID)
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

// setupPreviousReleaseSession leaves the repository the way a previous
// release left it after one turn that created old.go: session state with a
// turn-end step and FilesTouched but no touched_file_hashes key, the session's
// .entire/metadata (full.jsonl and prompt.txt), and a legacy-shape shadow
// branch for the base commit, whose name it returns.
func setupPreviousReleaseSession(t *testing.T, env *TestEnv, sess *Session) string {
	t.Helper()
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

	return legacyBranch
}

// TestUpgrade_PreviousReleaseSessionCommitsWithoutTurnEnd commits straight
// after the upgrade, with no turn end on the new CLI to record hashes: the
// commit is judged on the previous release's state alone (name matching for a
// path with no recorded hash). It must link the session, leave nothing
// pending, and leave the legacy branch alone.
func TestUpgrade_PreviousReleaseSessionCommitsWithoutTurnEnd(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	legacyBranch := setupPreviousReleaseSession(t, env, sess)

	env.GitCommitWithHooks("Add old file", "old.go")
	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("a commit of the previous release's file with no turn end since the upgrade should link the session")
	}
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID: checkpointID,
		SessionID:    sess.ID,
		FilesTouched: []string{"old.go"},
	})

	state, err := env.GetSessionState(sess.ID)
	if err != nil || state == nil {
		t.Fatalf("GetSessionState after commit failed: state=%v err=%v", state, err)
	}
	if len(state.FilesTouched) != 0 || state.StepCount != 0 {
		t.Errorf("nothing should be pending after committing the only file: StepCount=%d FilesTouched=%v", state.StepCount, state.FilesTouched)
	}
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
