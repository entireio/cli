//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// checkpointFingerprint identifies a checkpoint's stored content, so a test can
// tell whether a later commit rewrote it.
func checkpointFingerprint(env *TestEnv, checkpointID string) string {
	env.T.Helper()
	if env.usingGitRefs() {
		return strings.TrimSpace(testutil.RunGit(env.T, env.RepoDir, "rev-parse", checkpointRefName(checkpointID)))
	}
	content, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	require.True(env.T, found, "checkpoint %s must exist", checkpointID)
	return content
}

// A squash commit inherits the squashed commits' trailers. Work the session
// still holds must become its own checkpoint, never be written into an
// inherited one.
func TestSquashCommit_PendingWorkGetsItsOwnCheckpoint(t *testing.T) {
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
	turn("part two", "f2.txt", "two\n")
	env.GitCommitWithShadowHooks("part two", "f2.txt")
	t2 := env.LatestCheckpointID()
	require.NotEqual(t, t1, t2)
	// Turn three is still uncommitted when the squash happens.
	turn("part three", "f3.txt", "three\n")
	before1, before2 := checkpointFingerprint(env, t1), checkpointFingerprint(env, t2)

	env.GitCheckoutBranch(masterBranch)
	testutil.RunGit(t, env.RepoDir, "merge", "--squash", "feature/test-branch")
	env.GitCommitWithShadowHooks("Feature (squashed)", "f3.txt")

	var got []string
	for _, cpID := range trailers.ParseAllCheckpoints(testutil.RunGit(t, env.RepoDir, "log", "-1", "--format=%B")) {
		got = append(got, cpID.String())
	}
	require.Equal(t, before1, checkpointFingerprint(env, t1), "an inherited checkpoint was rewritten")
	require.Equal(t, before2, checkpointFingerprint(env, t2), "an inherited checkpoint was rewritten")
	require.Contains(t, got, t1)
	require.Contains(t, got, t2)
	require.Len(t, got, 3, "the pending turn must get a fresh checkpoint next to the inherited trailers, got %v", got)
}
