package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	redoCheckpointOne = "01M2VBJBJQZ2BP1W2PBWDF3J31"
	redoCheckpointTwo = "01M2VBJBJQZ2BP1W2PBWDF3J32"
)

// redoFixture: two linked commits on top of a base, ready to be redone.
func redoFixture(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, "f1.txt", "one\n")
	testutil.GitAdd(t, dir, "f1.txt")
	testutil.GitCommit(t, dir, "agent: part one\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.WriteFile(t, dir, "f2.txt", "two\n")
	testutil.GitAdd(t, dir, "f2.txt")
	testutil.GitCommit(t, dir, "agent: part two\n\nEntire-Checkpoint: "+redoCheckpointTwo)
	t.Chdir(dir)
	return dir
}

func prepareMessage(t *testing.T, text string) string {
	t.Helper()
	msgFile := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(text), 0o600))
	require.NoError(t, NewManualCommitStrategy().PrepareCommitMsg(context.Background(), msgFile, "message"))
	got, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	return string(got)
}

func TestPrepareCommitMsg_RedoAfterSoftResetInheritsBothTrailers(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~2")
	got := prepareMessage(t, "feat: the feature (logical)\n")
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got), "%q", got)
}

// A backup branch at the tip being redone (`git branch backup` before the
// reset) holds the very work being recommitted; it must not make that work
// look like it is still on a branch.
func TestPrepareCommitMsg_RedoWithBackupBranchAtTipStillInherits(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "branch", "backup")
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~2")
	got := prepareMessage(t, "feat: the feature (logical)\n")
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_RedoDeletedFileInheritsTrailer(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "deleted.txt", "delete me\n")
	testutil.GitAdd(t, dir, "deleted.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "rm", "-q", "deleted.txt")
	testutil.GitCommit(t, dir, "agent: delete file\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~1")
	t.Chdir(dir)

	got := prepareMessage(t, "chore: delete obsolete file\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_RedoLastFileInDirectoryInheritsTrailer(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "nested/deleted.txt", "delete me\n")
	testutil.GitAdd(t, dir, "nested/deleted.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "rm", "-q", "nested/deleted.txt")
	testutil.GitCommit(t, dir, "agent: delete nested file\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~1")
	t.Chdir(dir)

	got := prepareMessage(t, "chore: delete obsolete nested file\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_RedoPureRenameInheritsTrailer(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "before.txt", "unchanged\n")
	testutil.GitAdd(t, dir, "before.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "mv", "before.txt", "after.txt")
	testutil.GitCommit(t, dir, "agent: rename file\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~1")
	t.Chdir(dir)

	got := prepareMessage(t, "chore: rename file\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_RedoRenameSourceDeletionInheritsTrailer(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "before.txt", "unchanged\n")
	testutil.GitAdd(t, dir, "before.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "mv", "before.txt", "after.txt")
	testutil.GitCommit(t, dir, "agent: rename file\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~1")
	testutil.RunGit(t, dir, "add", "-u", "--", "before.txt")
	t.Chdir(dir)

	got := prepareMessage(t, "chore: remove the old name\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_FileToDirectoryPartialRedoInheritsNothing(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "p", "regular file\n")
	testutil.GitAdd(t, dir, "p")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "rm", "-q", "p")
	testutil.WriteFile(t, dir, "p/child", "replacement child\n")
	testutil.GitAdd(t, dir, "p/child")
	testutil.GitCommit(t, dir, "agent: replace file with directory\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~1")
	testutil.RunGit(t, dir, "rm", "-q", "--cached", "p")
	t.Chdir(dir)

	got := prepareMessage(t, "chore: delete p without adding its replacement\n")
	require.Empty(t, checkpointIDs(got), "a directory at ORIG_HEAD is not an absent path: %q", got)
}

func TestPrepareCommitMsg_DirectoryToFilePartialDeletionInheritsTrailer(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "p/child", "nested file\n")
	testutil.GitAdd(t, dir, "p/child")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "rm", "-q", "p/child")
	testutil.WriteFile(t, dir, "p", "replacement file\n")
	testutil.GitAdd(t, dir, "p")
	testutil.GitCommit(t, dir, "agent: replace directory with file\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~1")
	testutil.RunGit(t, dir, "rm", "-q", "--cached", "p/child")
	t.Chdir(dir)

	got := prepareMessage(t, "chore: remove the nested path first\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

func TestStagedBlobs_UnusualExistingPathIsNotDeletion(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	name := "café.txt"
	testutil.WriteFile(t, dir, name, "before\n")
	testutil.GitAdd(t, dir, name)
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, name, "after\n")
	testutil.GitAdd(t, dir, name)
	t.Chdir(dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()

	staged, err := stagedBlobs(context.Background(), repo)
	require.NoError(t, err)
	entry, ok := staged[name]
	require.True(t, ok, "staged path must retain its literal bytes: %#v", staged)
	require.False(t, entry.deleted)
	require.NotZero(t, entry.blob)
}

func TestPrepareCommitMsg_RedoByFileInheritsOnlyThatFilesTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	testutil.GitAdd(t, dir, "f2.txt")
	got := prepareMessage(t, "feat: part two (logical)\n")
	require.Equal(t, []string{redoCheckpointTwo}, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_RewrittenContentInheritsNothing(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--hard", "HEAD~2")
	testutil.WriteFile(t, dir, "f1.txt", "something else\n")
	testutil.GitAdd(t, dir, "f1.txt")
	got := prepareMessage(t, "feat: a different take\n")
	require.Empty(t, checkpointIDs(got), "content that differs from the dropped commits is new work, not a redo: %q", got)
}

func TestPrepareCommitMsg_NoResetInheritsNothing(t *testing.T) {
	dir := redoFixture(t)
	testutil.WriteFile(t, dir, "f3.txt", "three\n")
	testutil.GitAdd(t, dir, "f3.txt")
	got := prepareMessage(t, "feat: part three\n")
	require.Empty(t, checkpointIDs(got), "%q", got)
}

func TestPrepareCommitMsg_RedoKeepsAnAlreadyPresentTrailerOnce(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~2")
	got := prepareMessage(t, "feat: the feature\n\nEntire-Checkpoint: "+redoCheckpointOne+"\n")
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got), "%q", got)
}

// A redo split into several commits keeps inheriting: the reset is still the
// latest ref operation while those commits land.
func TestPrepareCommitMsg_RedoSecondCommitStillInherits(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	testutil.GitAdd(t, dir, "f1.txt")
	testutil.GitCommit(t, dir, "feat: part one (logical)")
	testutil.GitAdd(t, dir, "f2.txt")
	got := prepareMessage(t, "feat: part two (logical)\n")
	require.Equal(t, []string{redoCheckpointTwo}, checkpointIDs(got), "%q", got)
}

// ORIG_HEAD outlives the reset that set it. Once another ref operation has
// happened, restoring old content is new work, not a redo.
func TestPrepareCommitMsg_StaleOrigHeadInheritsNothing(t *testing.T) {
	dir := redoFixture(t)
	// A completed rebase leaves ORIG_HEAD at the pre-rebase tip.
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "base", "HEAD~2")
	testutil.WriteFile(t, dir, "other.txt", "other\n")
	testutil.GitAdd(t, dir, "other.txt")
	testutil.GitCommit(t, dir, "unrelated base work")
	testutil.RunGit(t, dir, "checkout", "-q", "-")
	testutil.RunGit(t, dir, "rebase", "-q", "base")
	// Later, unrelated work that happens to restore f2.txt byte for byte.
	testutil.RunGit(t, dir, "rm", "-q", "f2.txt")
	testutil.GitCommit(t, dir, "drop f2")
	testutil.WriteFile(t, dir, "f2.txt", "two\n")
	testutil.GitAdd(t, dir, "f2.txt")
	got := prepareMessage(t, "bring f2 back\n")
	require.Empty(t, checkpointIDs(got), "a stale ORIG_HEAD must not link unrelated work: %q", got)
}
