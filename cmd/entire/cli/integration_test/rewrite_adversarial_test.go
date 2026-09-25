//go:build integration

package integration

// Adversarial review of PR #2574: what PostCommit condenses after trailers are
// inherited from squashed or redone commits. Each test asserts what the user
// would WANT; a failing test means the PR does something the user did not ask
// for. Tests marked GUARD document workflows the PR already handles correctly.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// A checkpoint ID that no local store has: a teammate's commit whose metadata
// was never fetched, or a dangling trailer.
const advForeignCheckpoint = "a1b2c3d4e5f6"

// advCheckpointStored reports whether a committed checkpoint exists locally in
// either layout: a per-checkpoint ref (git-refs backend, and where ULID-format
// IDs land regardless of backend) or a summary on the v1 metadata branch.
func advCheckpointStored(env *TestEnv, checkpointID string) bool {
	env.T.Helper()
	cmd := exec.CommandContext(env.T.Context(), "git", "rev-parse", "--verify", "-q", checkpointRefName(checkpointID))
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return true
	}
	_, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(checkpointID))
	return found
}

// advCommit runs the real prepare-commit-msg hook (as a human at a TTY) with
// the given source, then edits the message the way the user would before git
// records it: `#` lines are dropped (git's default cleanup) and any
// Entire-Checkpoint line for which keep returns false is removed. Removing the
// trailer prepare just stamped models both answering "n" at the -m prompt and
// deleting the stamped line in the editor. Returns the message as prepare left
// it and the message committed.
func advCommit(env *TestEnv, message, source string, keep func(checkpointID string) bool, files ...string) (string, string) {
	env.T.Helper()
	for _, f := range files {
		env.GitAdd(f)
	}
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	require.NoError(env.T, os.WriteFile(msgFile, []byte(message), 0o644))
	args := []string{msgFile}
	if source != "" {
		args = append(args, source)
	}
	if out, err := env.prepareCommitMsgCmd(true, args...).CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg output: %s", out)
	}
	prepared, err := os.ReadFile(msgFile)
	require.NoError(env.T, err)

	var kept []string
	for _, line := range strings.Split(string(prepared), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if cpID, ok := strings.CutPrefix(strings.TrimSpace(line), trailers.CheckpointTrailerKey+": "); ok && !keep(cpID) {
			continue
		}
		kept = append(kept, line)
	}
	final := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"
	if strings.TrimSpace(strings.SplitN(final, "\n", 2)[0]) == "" {
		final = "typed in editor" + final // the subject the user types
	}

	repo, err := gitrepo.OpenPath(env.RepoDir)
	require.NoError(env.T, err)
	defer repo.Close()
	wt, err := repo.Worktree()
	require.NoError(env.T, err)
	_, err = wt.Commit(final, &git.CommitOptions{
		Author: &object.Signature{Name: testAuthorName, Email: testAuthorEmail, When: time.Now()},
	})
	require.NoError(env.T, err)

	postCmd := exec.CommandContext(env.T.Context(), getTestBinary(), "hooks", "git", "post-commit")
	postCmd.Dir = env.RepoDir
	postCmd.Env = env.gitHookEnv()
	if out, err := postCmd.CombinedOutput(); err != nil {
		env.T.Logf("post-commit output: %s", out)
	}
	return string(prepared), final
}

func onlyID(want string) func(string) bool {
	return func(cpID string) bool { return cpID == want }
}

// Seed A, squash. Workflow: `git merge --squash` of a branch whose commit
// carries a trailer this machine has no checkpoint for (a teammate's branch,
// or a dangling trailer). An agent session in the worktree has uncommitted
// work that is part of the commit. The user commits with -m and answers "n"
// to "Link this commit to session context?".
// User wants: the decline is honoured; nothing is condensed, and certainly not
// into the teammate's checkpoint ID.
// PR does: the inherited trailer is the only one left, it does not exist
// locally, so condensationTarget picks it with preexisting=false and the
// stampedByAnotherCommit guard never fires; the session's work is written as a
// new local checkpoint under the teammate's ID. On main, -m squash commits got
// no inherited trailer, so the decline left the commit unlinked.
func TestAdversarial_SquashDeclinedDoesNotCondenseIntoForeignCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.WriteFile("teammate.txt", "teammate work\n")
	env.GitAdd("teammate.txt")
	env.GitCommitWithCheckpointID("teammate: feature work", advForeignCheckpoint)
	require.False(t, advCheckpointStored(env, advForeignCheckpoint), "precondition: foreign checkpoint is not in the local store")

	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "my part", sess.TranscriptPath))
	env.WriteFile("mine.txt", "my agent work\n")
	sess.CreateTranscript("my part", []FileChange{{Path: "mine.txt", Content: "my agent work\n"}})
	require.NoError(t, env.SimulateStop(sess.ID, sess.TranscriptPath))

	env.GitCheckoutBranch(masterBranch)
	testutil.RunGit(t, env.RepoDir, "merge", "--squash", "feature/test-branch")
	prepared, final := advCommit(env, "Feature (squashed)\n", "message", onlyID(advForeignCheckpoint), "mine.txt")
	require.Len(t, trailers.ParseAllCheckpoints(prepared), 2, "precondition: prepare inherited the trailer and stamped one: %q", prepared)
	require.Equal(t, []string{advForeignCheckpoint}, idsOf(final))

	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"declining the link must not condense session work into the teammate's checkpoint ID")
}

func idsOf(message string) []string {
	var out []string
	for _, cpID := range trailers.ParseAllCheckpoints(message) {
		out = append(out, cpID.String())
	}
	return out
}
