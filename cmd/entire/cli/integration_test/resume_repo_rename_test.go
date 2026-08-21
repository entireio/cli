//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResume_RepoRenamed_StillLinksCommitTrailer reproduces issue #1890.
//
// A session is started at one repo location, the whole repo directory is then
// renamed (a folder rename == a repo relocation), and the agent resumes the
// same session from the new location (Claude Code's own /resume fires a fresh
// turn with the same session ID but a new cwd). A commit made from the renamed
// repo must still carry its Entire-Checkpoint trailer.
//
// The bug: SessionState.WorktreePath is recorded once at session start and the
// commit-time matcher (findSessionsForWorktree) uses exact string equality with
// no fallback once the recorded path no longer exists. On a resumed turn the
// turn-start handler never reconciles WorktreePath to the current worktree, so
// the commit silently loses its trailer.
//
// The production change that makes this pass: reconcile WorktreePath on a
// resumed turn when the current worktree differs from the recorded one but
// shares the same git common dir (same store == same repo).
func TestResume_RepoRenamed_StillLinksCommitTrailer(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	// 1. Start a session at the original repo location.
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("first user-prompt-submit failed: %v", err)
	}

	// Sanity: WorktreePath was recorded, anchored at the original location.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("failed to read session state: %v", err)
	}
	if state == nil {
		t.Fatal("session state should exist after first turn")
	}
	oldResolved := resolvePath(t, env.RepoDir)
	if resolvePath(t, state.WorktreePath) != oldResolved {
		t.Fatalf("recorded WorktreePath %q should resolve to original repo %q", state.WorktreePath, env.RepoDir)
	}

	// 2. Rename the whole repo directory. Nothing else changes; the session
	//    store moves with it (it lives inside .git).
	oldDir := env.RepoDir
	newDir := filepath.Join(filepath.Dir(oldDir), filepath.Base(oldDir)+"-renamed")
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatalf("failed to rename repo directory: %v", err)
	}
	env.RepoDir = newDir

	// 3. Resume: a fresh turn fires from the new location with the same session
	//    ID and a transcript that now lives under the renamed repo.
	newTranscript := filepath.Join(env.RepoDir, ".entire", "tmp", session.ID+".jsonl")
	session.TranscriptPath = newTranscript
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(session.ID, newTranscript); err != nil {
		t.Fatalf("resumed user-prompt-submit failed: %v", err)
	}

	// 4. The agent writes a file after the move; the transcript records it.
	env.WriteFile("resumed.txt", "written after the repo moved")
	session.CreateTranscript("add a line after the repo moved", []FileChange{
		{Path: "resumed.txt", Content: "written after the repo moved"},
	})

	// 5. Commit from the new location.
	env.GitCommitWithShadowHooks("commit after the repo moved", "resumed.txt")

	// 6. The commit must be linked back to the session.
	commit := env.GetHeadHash()
	if checkpointID := env.GetCheckpointIDFromCommitMessage(commit); checkpointID == "" {
		t.Fatal("commit after repo rename lost its Entire-Checkpoint trailer: " +
			"session WorktreePath was not reconciled to the new location on resume")
	}
}

func resolvePath(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return resolved
}
