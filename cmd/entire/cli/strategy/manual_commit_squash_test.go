package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// squashFixture builds a repository whose feature branch carries one commit
// with an Entire-Checkpoint trailer, then runs `git merge --squash` of that
// branch in the main checkout so `.git/SQUASH_MSG` exists and the change is
// staged. Returns the repo dir and the branch commit's checkpoint ID.
func squashFixture(t *testing.T) (string, string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "feature")
	testutil.WriteFile(t, dir, "feature.txt", "agent work\n")
	testutil.GitAdd(t, dir, "feature.txt")
	const branchCheckpoint = "01M2VBJBJQZ2BP1W2PBWDF3J30"
	testutil.GitCommit(t, dir, "agent: feature work\n\nEntire-Checkpoint: "+branchCheckpoint)
	testutil.RunGit(t, dir, "checkout", "-q", "-")
	testutil.RunGit(t, dir, "merge", "--squash", "feature")
	t.Chdir(dir)
	return dir, branchCheckpoint
}

// TestPrepareCommitMsg_SquashWithCustomMessageInheritsBranchTrailers covers
// `git merge --squash feature && git commit -m "…"`, the common way worktree
// work is integrated from the main checkout. git reports source "message"
// for that commit, not "squash", so the hook used to run ordinary session
// matching: it either refused (several live worktrees) or minted a fresh,
// near-empty checkpoint for whatever session it could find. The squash's
// provenance is the squashed commits, so their trailers are carried over from
// SQUASH_MSG and no session is matched.
func TestPrepareCommitMsg_SquashWithCustomMessageInheritsBranchTrailers(t *testing.T) {
	dir, branchCheckpoint := squashFixture(t)
	// A live session in this checkout must not be linked to the squash.
	saveIdentitySession(t, "sess-in-parent", func(st *SessionState) {
		st.WorktreePath = dir
		st.TranscriptPath = filepath.Join(dir, "transcript.jsonl")
		st.Phase = session.PhaseActive
	})

	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte("Feature work (squashed)\n"), 0o600))

	s := NewManualCommitStrategy()
	require.NoError(t, s.PrepareCommitMsg(context.Background(), msgFile, "message"))

	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.Contains(t, string(got), "Entire-Checkpoint: "+branchCheckpoint, "the squashed commit's trailer must be inherited")
	require.Equal(t, 1, strings.Count(string(got), "Entire-Checkpoint:"), "no fresh checkpoint may be minted for a squash: %q", got)
}

// TestPrepareCommitMsg_SquashDefaultMessageKeepsInheritedTrailerOnce covers
// the editor flow, where git seeds the message from SQUASH_MSG (trailer
// included) and reports source "squash": the trailer stays, and is not
// duplicated.
func TestPrepareCommitMsg_SquashDefaultMessageKeepsInheritedTrailerOnce(t *testing.T) {
	dir, branchCheckpoint := squashFixture(t)
	squashMsg, err := os.ReadFile(filepath.Join(dir, ".git", "SQUASH_MSG"))
	require.NoError(t, err)
	require.Contains(t, string(squashMsg), branchCheckpoint, "precondition: git's SQUASH_MSG carries the branch trailer")

	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, squashMsg, 0o600))

	s := NewManualCommitStrategy()
	require.NoError(t, s.PrepareCommitMsg(context.Background(), msgFile, "squash"))

	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(got), "Entire-Checkpoint:"))
	require.Contains(t, string(got), branchCheckpoint)
}

// A squash of commits that carry no trailers has nothing to inherit: ordinary
// matching must run, exactly as before the squash handling existed.
func TestInheritSquashedCheckpointTrailers_NoTrailersFallsThrough(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "plain")
	testutil.WriteFile(t, dir, "plain.txt", "no entire here\n")
	testutil.GitAdd(t, dir, "plain.txt")
	testutil.GitCommit(t, dir, "plain work, no trailer")
	testutil.RunGit(t, dir, "checkout", "-q", "-")
	testutil.RunGit(t, dir, "merge", "--squash", "plain")
	t.Chdir(dir)

	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte("Plain squash\n"), 0o600))
	s := NewManualCommitStrategy()
	require.False(t, s.inheritSquashedCheckpointTrailers(context.Background(), msgFile, "message"))
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.Equal(t, "Plain squash\n", string(got))
}
