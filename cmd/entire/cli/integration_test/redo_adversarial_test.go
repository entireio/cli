//go:build integration

package integration

// Adversarial review of PR #2574: what PostCommit condenses after trailers are
// inherited from squashed or redone commits. Each test asserts what the user
// would WANT; a failing test means the PR does something the user did not ask
// for. Tests marked GUARD document workflows the PR already handles correctly.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func keepAll(string) bool { return true }

// GUARD (seed A, redo, own checkpoint). Workflow: the agent's turn is
// committed (checkpoint T1). The user runs `git reset --soft HEAD~1` to rework
// the commit, the agent does another turn, and the user commits both files
// with -m, answering "n" to linking.
// User wants: the decline is honoured; T1 keeps describing what it described.
// PR does: T1 is inherited and is the lone trailer, but it pre-exists and the
// next TurnStart cleared LastCheckpointID, so stampedByAnotherCommit refuses
// the write ("refusing to condense into a checkpoint this session did not
// stamp"). Correct. Not covered: a human redo while the agent's turn that
// made T1 is still ACTIVE (LastCheckpointID still T1) would pass the guard.
func TestAdversarial_Guard_RedoDeclinedDoesNotCondenseIntoOwnEarlierCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	turn := func(prompt, file, content string) {
		require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, prompt, sess.TranscriptPath))
		env.WriteFile(file, content)
		sess.CreateTranscript(prompt, []FileChange{{Path: file, Content: content}})
		require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))
	}
	turn("part one", "f1.txt", "one\n")
	env.GitCommitWithShadowHooks("part one", "f1.txt")
	t1 := env.LatestCheckpointID()
	state, err := env.GetSessionState(sess.ID)
	require.NoError(t, err)
	require.Equal(t, t1, state.LastCheckpointID.String(), "precondition: T1 is the session's LastCheckpointID")
	before := checkpointFingerprint(env, t1)

	testutil.RunGit(t, env.RepoDir, "reset", "-q", "--soft", "HEAD~1")
	turn("part two", "f2.txt", "two\n")

	prepared, final := advCommit(env, "feat: reworked\n", "message", onlyID(t1), "f1.txt", "f2.txt")
	require.Len(t, trailers.ParseAllCheckpoints(prepared), 2, "precondition: prepare inherited T1 and stamped one: %q", prepared)
	require.Equal(t, []string{t1}, idsOf(final))

	require.Equal(t, before, checkpointFingerprint(env, t1),
		"declining the link must not rewrite the redone commit's checkpoint with new work")
}

// Editor-flow ordering. Workflow: a commit carries a trailer with no local
// checkpoint (dangling, or authored on another machine whose metadata is not
// fetched). The user runs `git reset --soft HEAD~1`, the agent adds a turn, and
// the user runs plain `git commit` (editor) and accepts the stamped trailer.
// User wants: the session's work goes into the freshly stamped checkpoint.
// PR does: inheritReplacedCommitsTrailers appends the inherited trailer AFTER
// git's comment block, while addCheckpointTrailerWithComment inserts the stamp
// BEFORE it, so the committed order is [stamp, inherited]. PostCommit picks the
// LAST trailer without a checkpoint: the inherited one. The work is written
// under the foreign ID and the stamped trailer dangles.
func TestAdversarial_RedoEditorFlowCondensesIntoStampedCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.WriteFile("f1.txt", "one\n")
	env.GitAdd("f1.txt")
	env.GitCommitWithCheckpointID("part one (elsewhere)", advForeignCheckpoint)
	testutil.RunGit(t, env.RepoDir, "reset", "-q", "--soft", "HEAD~1")

	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "part two", sess.TranscriptPath))
	env.WriteFile("f2.txt", "two\n")
	sess.CreateTranscript("part two", []FileChange{{Path: "f2.txt", Content: "two\n"}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

	// What git hands prepare-commit-msg before the editor opens: an empty
	// first line and git's comment block. The user types the subject later.
	editorTemplate := "\n# Please enter the commit message for your changes. Lines starting\n# with '#' will be ignored.\n#\n"
	prepared, final := advCommit(env, editorTemplate, "", keepAll, "f2.txt")
	ids := idsOf(final)
	require.Len(t, ids, 2, "precondition: inherited + stamped trailer: prepared %q", prepared)
	require.Contains(t, ids, advForeignCheckpoint)
	var stamped string
	for _, cpID := range ids {
		if cpID != advForeignCheckpoint {
			stamped = cpID
		}
	}

	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"session work must not be written under the inherited foreign ID (committed %q)", final)
	require.True(t, advCheckpointStored(env, stamped),
		"the stamped trailer must get its checkpoint (committed %q)", final)
}

// GUARD for the test above: the same redo with `commit -m` appends the stamp
// after the inherited trailer, so the stamp is condensed.
func TestAdversarial_Guard_RedoMessageFlowCondensesIntoStampedCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.WriteFile("f1.txt", "one\n")
	env.GitAdd("f1.txt")
	env.GitCommitWithCheckpointID("part one (elsewhere)", advForeignCheckpoint)
	testutil.RunGit(t, env.RepoDir, "reset", "-q", "--soft", "HEAD~1")

	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "part two", sess.TranscriptPath))
	env.WriteFile("f2.txt", "two\n")
	sess.CreateTranscript("part two", []FileChange{{Path: "f2.txt", Content: "two\n"}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

	prepared, final := advCommit(env, "feat: redo\n", "message", keepAll, "f2.txt")
	ids := idsOf(final)
	require.Len(t, ids, 2, "precondition: inherited + stamped trailer: prepared %q", prepared)
	require.Equal(t, advForeignCheckpoint, ids[0])

	require.False(t, advCheckpointStored(env, advForeignCheckpoint), "committed %q", final)
	require.True(t, advCheckpointStored(env, ids[1]), "committed %q", final)
}
