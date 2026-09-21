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

// squashFixture: a trailer-carrying branch commit, squash-merged into main.
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

// `merge --squash` + `commit -m` reports source "message"; the squashed commits'
// trailers must be inherited and no session matched.
func TestPrepareCommitMsg_SquashWithCustomMessageInheritsBranchTrailers(t *testing.T) {
	dir, branchCheckpoint := squashFixture(t)
	// Must not be linked to the squash.
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

// Editor flow: the seeded message already carries the trailer; keep it once.
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

// No trailers to inherit: ordinary matching must run.
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

// Unreadable message: nothing inherited, must not claim to have taken over.
func TestInheritSquashedCheckpointTrailers_UnreadableMessageFallsThrough(t *testing.T) {
	squashFixture(t)
	s := NewManualCommitStrategy()
	require.False(t, s.inheritSquashedCheckpointTrailers(context.Background(), t.TempDir(), "message"),
		"a directory is not a readable message file")
}

// A stale SQUASH_MSG must not turn an amend into a squash: amend keeps its own
// preserve/restore logic.
func TestInheritSquashedCheckpointTrailers_AmendIgnoresStaleSquashMsg(t *testing.T) {
	_, branchCheckpoint := squashFixture(t)
	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte("Amended message\n"), 0o600))
	s := NewManualCommitStrategy()
	require.False(t, s.inheritSquashedCheckpointTrailers(context.Background(), msgFile, "commit"))
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.NotContains(t, string(got), branchCheckpoint)
}
