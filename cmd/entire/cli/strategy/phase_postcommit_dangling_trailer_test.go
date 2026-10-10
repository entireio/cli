package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestPostCommit_IdleSession_NewLiveTranscriptEdits_DanglingTrailer is a
// regression test (currently RED — it proves an open bug) for the silent
// prepare/post-commit mismatch that leaves a commit carrying an
// Entire-Checkpoint trailer that points at a checkpoint which was never
// materialized locally.
//
// Scenario (reconstructed from a real Pi session):
//
//  1. A session was condensed, which cleared state.FilesTouched, then went IDLE.
//  2. A new agent turn arrived WITHOUT a TurnStart event (rapidly submitted
//     prompt), so the session stayed IDLE and SaveStep never repopulated
//     state.FilesTouched. There is no shadow branch for the new edits yet — the
//     ephemeral shadow checkpoint is only created later, at turn-end.
//  3. The agent edited files and committed them.
//
// PrepareCommitMsg detects the new work by re-extracting the modified files
// from the *live* transcript (sessionHasNewContentFromLiveTranscript) and
// overlapping them with the staged files — so it adds the trailer.
//
// PostCommit, however, computes filesTouchedBefore only from the *persisted*
// state.FilesTouched for non-ACTIVE sessions (manual_commit_hooks.go, the
// active-only fallback), and never passes the committed files as an overlap
// signal. With FilesTouched empty and no staged files, both hasNew collapses to
// false and the overlap guard in shouldCondenseWithOverlapCheck rejects
// condensation. The result: the commit keeps the trailer but no checkpoint is
// ever written for it — a dangling trailer.
//
// The invariant this test asserts (and which the fix must restore): every
// checkpoint trailer PrepareCommitMsg writes must correspond to a materialized
// checkpoint after PostCommit. Fix direction per the investigation: re-extract
// files during post-commit for IDLE sessions, or persist the positive
// prepare-hook association, and log a warning whenever a commit carries a
// trailer with no materialized checkpoint.
func TestPostCommit_IdleSession_NewLiveTranscriptEdits_DanglingTrailer(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	ctx := context.Background()

	// Canonical worktree root (symlink-resolved). On macOS t.TempDir() lives
	// under /var/... but the hooks resolve to /private/var/..., so the live
	// transcript's absolute paths must be built from this root or path
	// normalization would drop them.
	root, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "idle-live-transcript-dangling"

	// Initialize the session; InitializeSession stamps BaseCommit + WorktreeID
	// from the current HEAD/worktree. We reconfigure the phase below.
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeGemini, "", "", ""))

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	// The two files the (untracked) turn edited. Absolute paths in the live
	// transcript; the extractor normalizes them to repo-relative.
	fileA := "src_a.txt"
	fileB := "src_b.txt"

	// Live transcript containing the new file edits — this is the ONLY record of
	// the turn's work (no shadow branch, no persisted FilesTouched).
	transcriptPath := filepath.Join(root, "gemini-session.json")
	transcript := `{
  "messages": [
    {"type": "user", "content": [{"text": "edit two files"}]},
    {"type": "gemini", "content": "", "toolCalls": [
      {"name": "write_file", "args": {"file_path": "` + filepath.Join(root, fileA) + `"}},
      {"name": "write_file", "args": {"file_path": "` + filepath.Join(root, fileB) + `"}}
    ]}
  ]
}`
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))

	now := time.Now()
	state.Phase = session.PhaseIdle
	state.FilesTouched = nil // cleared by the prior condensation
	state.AgentType = agent.AgentTypeGemini
	state.TranscriptPath = transcriptPath
	state.CheckpointTranscriptStart = 0 // whole live transcript is "new"
	state.LastInteractionTime = &now    // recently active IDLE session
	// The prior condensation (which cleared FilesTouched) stamped a
	// LastCheckpointID. This is load-bearing for the scenario: without it,
	// listAllSessionStates treats the shadow-less IDLE session as orphaned and
	// deletes it, so no hook would ever see it.
	priorCheckpointID := id.MustCheckpointID("a1b2c3d4e5f6")
	state.LastCheckpointID = priorCheckpointID
	require.NoError(t, s.saveSessionState(ctx, state))

	// Guarantee the scenario's premise: there is no shadow branch carrying the
	// new edits at commit time (matches "the persistent checkpoint was never
	// written locally"). Delete any ref InitializeSession may have created.
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	if _, refErr := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true); refErr == nil {
		require.NoError(t, repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(shadowBranch)))
	}

	// The turn's edits land on disk and get staged.
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileA), []byte("content a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileB), []byte("content b"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add(fileA)
	require.NoError(t, err)
	_, err = wt.Add(fileB)
	require.NoError(t, err)

	// --- PrepareCommitMsg: adds a trailer via live-transcript inspection ---
	commitMsgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("edit two files\n"), 0o644))
	require.NoError(t, s.PrepareCommitMsg(ctx, commitMsgFile, "message"))

	msgBytes, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	preparedMsg := string(msgBytes)

	checkpointID, found := trailers.ParseCheckpoint(preparedMsg)
	// Premise of the bug: the prepare hook DID link this commit to the session
	// based purely on the live transcript. If this ever stops holding, the
	// scenario's setup has drifted and the test below is meaningless.
	require.True(t, found,
		"premise: PrepareCommitMsg must add a checkpoint trailer from the live transcript")

	// --- Complete the commit carrying that trailer ---
	_, err = wt.Commit(preparedMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// --- PostCommit: must materialize a checkpoint for the trailer ---
	require.NoError(t, s.PostCommit(ctx))

	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	// The invariant: the trailer must not dangle. PostCommit must have condensed
	// the session into a checkpoint for exactly the ID the trailer points at, and
	// the permanent metadata branch must exist to hold it.
	//
	// On current code this FAILS: PostCommit skips condensation (no re-extraction
	// of the live-transcript files for the IDLE session), so LastCheckpointID
	// stays pinned to the PRIOR checkpoint and entire/checkpoints/v1 is never
	// created — the fresh trailer on HEAD points at a checkpoint that was never
	// materialized.
	_, metaErr := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, metaErr,
		"dangling trailer: entire/checkpoints/v1 must exist because HEAD carries a checkpoint trailer")
	require.Equal(t, checkpointID.String(), state.LastCheckpointID.String(),
		"dangling trailer: PostCommit must materialize a checkpoint for the trailer PrepareCommitMsg added")
}
