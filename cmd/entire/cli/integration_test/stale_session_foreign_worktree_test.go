//go:build integration && (linux || darwin)

package integration

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A session that did its work in one worktree and then went stale must never
// be condensed from another worktree in a way that writes a checkpoint
// without its transcript. The tests below set up such a session in a linked
// worktree ("home"), with a real transcript-bearing checkpoint from a commit
// there, then exercise every path that can condense it from a fresh linked
// worktree with no session of its own: commit linking (prepare-commit-msg /
// post-commit) and the background sweep's commit-less salvage.

// staleHomeSession runs one committed turn and one uncommitted turn for a new
// session in home, so the session has a transcript-bearing checkpoint and
// pending work (StepCount > 0, pending.go in FilesTouched). Returns the
// session and the committed checkpoint's ID.
func staleHomeSession(t *testing.T, caller, home *TestEnv) (*Session, string) {
	t.Helper()
	sess := home.NewSession()
	require.NoError(t, home.SimulateUserPromptSubmit(sess.ID))
	home.WriteFile("committed.go", "package committed\n")
	sess.CreateTranscript("Create committed work", []FileChange{{Path: "committed.go", Content: "package committed\n"}})
	require.NoError(t, home.SimulateStop(sess.ID, sess.TranscriptPath))
	home.GitCommitWithHooksAsAgent("Committed work in home", "committed.go")

	first := caller.GetCheckpointIDFromCommitMessage(home.GetHeadHash())
	require.NotEmpty(t, first, "the home commit must link the session")
	_, found := caller.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(first, paths.TranscriptFileName))
	require.True(t, found, "the home commit's checkpoint must carry the transcript")

	require.NoError(t, home.SimulateUserPromptSubmit(sess.ID))
	home.WriteFile("pending.go", "package pending\n")
	sess.CreateTranscript("Create pending work", []FileChange{{Path: "pending.go", Content: "package pending\n"}})
	require.NoError(t, home.SimulateStop(sess.ID, sess.TranscriptPath))

	state, err := caller.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, home.RepoDir, state.WorktreePath)
	require.Equal(t, "wt/home", state.Branch)
	require.True(t, state.HasPendingWork())
	return sess, first
}

// requireTranscriptBearing fails when checkpointID exists without a
// transcript in its first session slot.
func requireTranscriptBearing(t *testing.T, env *TestEnv, checkpointID string) {
	t.Helper()
	_, hasMeta := env.ReadFileFromBranch(paths.MetadataBranchName, SessionMetadataPath(checkpointID))
	_, hasTranscript := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpointID, paths.TranscriptFileName))
	require.True(t, !hasMeta || hasTranscript,
		"checkpoint %s was written without its transcript (session metadata present, %s absent)", checkpointID, paths.TranscriptFileName)
}

// TestStaleSession_CommitInFreshWorktree_NotLinked: a commit in a fresh linked
// worktree with no session of its own must not link or condense the stale
// session homed elsewhere, whether its home still exists (common-dir fallback
// finds it as the single candidate worktree) or was removed. A stale ACTIVE
// session (agent killed without a Stop hook) is not in the table: the no-TTY
// fast path stamps it and post-commit declines it for lack of overlap, which
// is a dangling trailer rather than a checkpoint.
func TestStaleSession_CommitInFreshWorktree_NotLinked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		phase   session.Phase
		ended   bool
		removed bool
	}{
		{name: "idle home kept", phase: session.PhaseIdle},
		{name: "idle home removed", phase: session.PhaseIdle, removed: true},
		{name: "ended home kept", phase: session.PhaseEnded, ended: true},
		{name: "ended home removed", phase: session.PhaseEnded, ended: true, removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := NewFeatureBranchEnv(t)
			home := worktreeEnv(t, caller, "home")
			sess, first := staleHomeSession(t, caller, home)

			state, err := caller.GetSessionState(sess.ID)
			require.NoError(t, err)
			stale := time.Now().Add(-3 * 24 * time.Hour).UTC()
			state.Phase = tc.phase
			state.LastInteractionTime = &stale
			if tc.ended {
				state.EndedAt = &stale
			}
			require.NoError(t, caller.WriteSessionState(sess.ID, state))
			if tc.removed {
				testutil.RunGit(t, caller.RepoDir, "worktree", "remove", "--force", home.RepoDir)
			}

			fresh := worktreeEnv(t, caller, "fresh")
			fresh.WriteFile("fresh.txt", "work in the fresh worktree\n")
			fresh.GitCommitWithHooksAsAgent("Commit in the fresh worktree", "fresh.txt")

			head := fresh.GetHeadHash()
			assert.Empty(t, caller.GetCheckpointIDFromCommitMessage(head),
				"a commit in a worktree with no session must not carry the stale session's trailer")

			after, err := caller.GetSessionState(sess.ID)
			require.NoError(t, err)
			require.NotNil(t, after, "the stale session must survive another worktree's commit")
			assert.True(t, after.LastCheckpointID.IsEmpty(), "the stale session must not be condensed by the fresh worktree's commit")
			assert.Equal(t, first, caller.TryGetLatestCheckpointID(), "no checkpoint may be written for the fresh worktree's commit")
		})
	}
}

// TestStaleSession_SweepFromFreshWorktree_HomeRemoved: an ENDED session with
// pending files, whose home worktree was removed after it ended (so both its
// live transcript and the stored turn-end copy are gone), is nominated to the
// background sweep by the next session start, which happens in a fresh
// worktree. The sweep must not write a transcript-less checkpoint for it.
func TestStaleSession_SweepFromFreshWorktree_HomeRemoved(t *testing.T) {
	t.Parallel()
	caller := NewFeatureBranchEnv(t)
	home := worktreeEnv(t, caller, "home")
	sess, first := staleHomeSession(t, caller, home)

	// A clean session end leaves the pending files to PostCommit.
	require.NoError(t, home.SimulateSessionEnd(sess.ID))
	state, err := caller.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, session.PhaseEnded, state.Phase)
	require.False(t, state.FullyCondensed)
	require.True(t, state.LastCheckpointID.IsEmpty(), "session end must leave pending files to PostCommit")
	require.Equal(t, first, caller.TryGetLatestCheckpointID())

	// Two days later, the home worktree is gone and a session starts elsewhere.
	endedAt := time.Now().Add(-48 * time.Hour).UTC()
	state.EndedAt = &endedAt
	require.NoError(t, caller.WriteSessionState(sess.ID, state))
	testutil.RunGit(t, caller.RepoDir, "worktree", "remove", "--force", home.RepoDir)
	_, statErr := os.Stat(sess.TranscriptPath)
	require.True(t, os.IsNotExist(statErr), "precondition: the live transcript went with the home worktree")

	fresh := worktreeEnv(t, caller, "fresh")
	fresh.RunCLI("__sweep_sessions")

	after, err := caller.GetSessionState(sess.ID)
	require.NoError(t, err)
	if after != nil && !after.LastCheckpointID.IsEmpty() {
		requireTranscriptBearing(t, caller, after.LastCheckpointID.String())
	}
	latest := caller.TryGetLatestCheckpointID()
	if latest != first {
		requireTranscriptBearing(t, caller, latest)
	}
	if after != nil {
		assert.False(t, after.Phase == session.PhaseEnded && !after.FullyCondensed && after.HasPendingWork(),
			"the sweep must settle the session so it is not nominated again")
	}
}

// TestStaleSession_SweepFinalizesDeadOwner_FromFreshWorktree: a session whose
// agent died without a session end (IDLE, dead owner) is finalized by the
// sweep from whichever worktree the next session starts in. Its checkpoint
// must record the session's own branch, not the sweeping worktree's HEAD.
func TestStaleSession_SweepFinalizesDeadOwner_FromFreshWorktree(t *testing.T) {
	t.Parallel()
	caller := NewFeatureBranchEnv(t)
	home := worktreeEnv(t, caller, "home")
	sess := home.NewSession()
	require.NoError(t, home.SimulateUserPromptSubmit(sess.ID))
	home.WriteFile("committed.go", "package committed\n")
	sess.CreateTranscript("Create committed work", []FileChange{{Path: "committed.go", Content: "package committed\n"}})
	require.NoError(t, home.SimulateStop(sess.ID, sess.TranscriptPath))
	home.GitCommitWithHooksAsAgent("Committed work in home", "committed.go")
	first := caller.GetCheckpointIDFromCommitMessage(home.GetHeadHash())
	require.NotEmpty(t, first)

	// One more turn whose edit was later dropped from the worktree, so the
	// session carries a turn-end step but no pending files (the shape the
	// dead-owner finalization condenses), then the agent dies.
	require.NoError(t, home.SimulateUserPromptSubmit(sess.ID))
	home.WriteFile("scratch.go", "package scratch\n")
	sess.CreateTranscript("Try something", []FileChange{{Path: "scratch.go", Content: "package scratch\n"}})
	require.NoError(t, home.SimulateStop(sess.ID, sess.TranscriptPath))
	state, err := caller.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, session.PhaseIdle, state.Phase)
	require.Positive(t, state.StepCount)
	state.FilesTouched, state.TouchedFileHashes = nil, nil
	// A live PID with a mismatched start fingerprint reads as PID reuse: dead.
	state.Owner = &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}
	require.NoError(t, caller.WriteSessionState(sess.ID, state))

	fresh := worktreeEnv(t, caller, "fresh")
	fresh.RunCLI("__sweep_sessions")

	after, err := caller.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, session.PhaseEnded, after.Phase, "the sweep must finalize the dead-owner session")
	require.NotEqual(t, first, after.LastCheckpointID.String(), "the final turn must be condensed")
	cpID := after.LastCheckpointID.String()
	requireTranscriptBearing(t, caller, cpID)
	data, found := caller.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpID))
	require.True(t, found)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(data), &summary))
	assert.Equal(t, "wt/home", summary.Branch, "the checkpoint must record the session's branch, not the sweeping worktree's HEAD")
}
