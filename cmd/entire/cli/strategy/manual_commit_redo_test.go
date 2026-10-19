package strategy

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

const (
	redoCheckpointOne = "a1b2c3d4e501"
	redoCheckpointTwo = "a1b2c3d4e502"
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
	// The dropped commits' checkpoints exist, as when their post-commit
	// condensed: only a checkpoint this clone has may hold a redone file.
	writeRedoCheckpoints(t, redoCheckpointOne, redoCheckpointTwo)
	return dir
}

// writeRedoCheckpoints writes checkpoints through the store the hooks read.
func writeRedoCheckpoints(t *testing.T, ids ...string) {
	t.Helper()
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	store, err := NewManualCommitStrategy().getPersistentStore(t.Context(), repo)
	require.NoError(t, err)
	for _, cpID := range ids {
		require.NoError(t, store.Write(t.Context(), checkpoint.Session{
			CheckpointID: id.MustCheckpointID(cpID),
			SessionID:    "sess-original",
			CreatedAt:    time.Now(),
			Strategy:     "manual-commit",
			Transcript:   redact.AlreadyRedacted([]byte(testTranscriptPromptResponse)),
			Agent:        agent.AgentTypeClaudeCode,
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
		}))
	}
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

	staged, err := stagedBlobs(context.Background())
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

// A redo committed with `git commit -v`: git discards everything below the
// scissors line, so the inherited trailers must land above git's comments.
func TestPrepareCommitMsg_RedoInVerboseEditorKeepsTrailersAboveScissors(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~2")
	scissors := "# ------------------------ >8 ------------------------"
	got := advPrepare(t, "\n# Please enter the commit message.\n"+scissors+"\ndiff --git a/f1.txt b/f1.txt\n", "")
	kept, _, _ := strings.Cut(got, scissors)
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(kept), "%q", got)
}

// A redo committed with -m "#42 ..." keeps that line as its subject: a -m
// message has no git comment block for the trailers to go above.
func TestPrepareCommitMsg_RedoWithHashSubjectKeepsItsSubject(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~2")
	got := prepareMessage(t, "#42 the feature (logical)\n")
	require.True(t, strings.HasPrefix(got, "#42 the feature (logical)\n"), "%q", got)
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got), "%q", got)
}

// After a reset, every later commit ran two git subprocesses and diffed every
// dropped commit before rejecting the redo on its first since-commit, roughly
// doubling prepare-commit-msg for the rest of the branch. An unrelated commit
// since the reset must be rejected before any of that: a git wrapper on PATH
// records each subcommand run.
//
// Not parallel: t.Chdir and t.Setenv.
func TestReplacedCommits_UnrelatedCommitSinceResetRejectsBeforeSubprocesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper for git")
	}
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--hard", "HEAD~1")
	testutil.WriteFile(t, dir, "unrelated.txt", "later work\n")
	// The git CLI, not go-git: the commit must be in HEAD's reflog.
	testutil.RunGit(t, dir, "add", "unrelated.txt")
	testutil.RunGit(t, dir, "commit", "-q", "--no-verify", "-m", "unrelated")
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	wrapper := "#!/bin/sh\necho \"$1\" >> " + calls + "\nexec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, dropped, why := replacedCommits(ctx, repo)
	require.Empty(t, dropped)
	require.Equal(t, "a commit since the reset redid none of the dropped work", why)
	ran, err := os.ReadFile(calls)
	if err != nil {
		require.ErrorIs(t, err, fs.ErrNotExist)
	}
	require.NotContains(t, string(ran), "rev-list")
	require.NotContains(t, string(ran), "for-each-ref")
}

// A reftable repository keeps its reflog in .git/reftable/, so there is no
// logs/HEAD to read and redo inheritance silently never fired there.
//
// Not parallel: t.Chdir.
func TestReplacedCommits_ReftableRepository(t *testing.T) {
	// Commits through the git CLI so HEAD's reflog holds them, as a user's does.
	dir := advRepo(t, nil)
	t.Chdir(dir)
	testutil.WriteFile(t, dir, "f1.txt", "one\n")
	advCommitAll(t, dir, "agent: part one\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.WriteFile(t, dir, "f2.txt", "two\n")
	advCommitAll(t, dir, "agent: part two\n\nEntire-Checkpoint: "+redoCheckpointTwo)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~1")
	testutil.MigrateToReftable(t, dir)
	_, statErr := os.Stat(filepath.Join(dir, ".git", "logs", "HEAD"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "precondition: no files-backend reflog")
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	_, dropped, why := replacedCommits(ctx, repo)
	require.Len(t, dropped, 1, "the reset dropped part two: %s", why)
}

// redoSession is an ended session whose touched files include the redone ones,
// as after an agent commits, resets, and its turn end records the files again.
func redoSession(t *testing.T, dir string, phase session.Phase, files ...string) {
	t.Helper()
	saveIdentitySession(t, "sess-redo", func(st *SessionState) {
		st.WorktreePath = dir
		st.Phase = phase
		st.StepCount = 1
		st.FilesTouched = files
	})
}

// Regression: an agent committed, then ran `git reset HEAD~1`, and the redone
// commit got the inherited trailer plus a fresh one whose checkpoint held no
// new work, only the commit and the reset.
func TestPrepareCommitMsg_RedoOfSessionFilesStampsNoFreshTrailer(t *testing.T) {
	for _, phase := range []session.Phase{session.PhaseEnded, session.PhaseActive} {
		t.Run(string(phase), func(t *testing.T) {
			dir := redoFixture(t)
			redoSession(t, dir, phase, "f1.txt", "f2.txt")
			testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
			testutil.GitAdd(t, dir, "f1.txt", "f2.txt")
			got := prepareMessage(t, "feat: the feature (logical)\n")
			require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got),
				"a pure redo links the dropped checkpoints and stamps nothing new: %q", got)
		})
	}
}

// Work the redo does not cover still gets a checkpoint of its own.
func TestPrepareCommitMsg_RedoWithNewSessionWorkStampsFreshTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	testutil.WriteFile(t, dir, "f3.txt", "three\n")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redo", session.PhaseEnded, "f1.txt", "f3.txt")
	testutil.GitAdd(t, dir, "f1.txt", "f3.txt")
	ids := commitThroughHooks(t, dir, s, "feat: part one and three\n")
	require.Len(t, ids, 2, "%v", ids)
	require.Equal(t, redoCheckpointOne, ids[0], "the redone commit's trailer is inherited first")
	requireFreshTrailersResolve(t, ids)
}

// A redone file the session only shares with the redo is no evidence of new
// work, even when other staged files are unrelated to the session.
func TestPrepareCommitMsg_RedoPlusUnrelatedFileStampsNoFreshTrailer(t *testing.T) {
	dir := redoFixture(t)
	redoSession(t, dir, session.PhaseEnded, "f1.txt")
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	testutil.WriteFile(t, dir, "notes.txt", "human notes\n")
	testutil.GitAdd(t, dir, "f1.txt", "notes.txt")
	got := prepareMessage(t, "feat: part one plus notes\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

// The mid-turn agent fast path cannot tell the session's new work from a
// redone file mixed with other staged files: edits made through shell commands
// leave no trace in the transcript's tool calls. Keeping the link wins over
// avoiding a redundant checkpoint, so a partial redo still gets its trailer.
func TestPrepareCommitMsg_PartialRedoActiveSessionKeepsFastPathTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redo", session.PhaseActive, "f1.txt")
	testutil.WriteFile(t, dir, "notes.txt", "formatted by a shell command\n")
	testutil.GitAdd(t, dir, "f1.txt", "notes.txt")
	ids := commitThroughHooks(t, dir, s, "feat: part one plus notes\n")
	require.Len(t, ids, 2, "%v", ids)
	require.Equal(t, redoCheckpointOne, ids[0])
	requireFreshTrailersResolve(t, ids)
}

// A mode change made after the reset is new work, though the content matches:
// the redone commit's trailer is still inherited, but the session's checkpoint
// for the change is not suppressed.
func TestPrepareCommitMsg_RedoWithNewModeStampsFreshTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redo", session.PhaseEnded, "f1.txt", "f2.txt")
	testutil.GitAdd(t, dir, "f1.txt", "f2.txt")
	testutil.RunGit(t, dir, "update-index", "--chmod=+x", "f1.txt")
	ids := commitThroughHooks(t, dir, s, "feat: the feature, f1 executable\n")
	require.Len(t, ids, 3, "%v", ids)
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, ids[:2])
	requireFreshTrailersResolve(t, ids)
}

// A checkpoint covers a path only as its commit left it: a later dropped
// commit without a checkpoint that changed the same file holds work no
// inherited checkpoint does.
func TestPrepareCommitMsg_RedoOfUnlinkedLaterChangeStampsFreshTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.WriteFile(t, dir, "f1.txt", "one, revised\n")
	testutil.GitAdd(t, dir, "f1.txt")
	testutil.GitCommit(t, dir, "revise part one")
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~3")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redo", session.PhaseEnded, "f1.txt")
	testutil.GitAdd(t, dir, "f1.txt")
	ids := commitThroughHooks(t, dir, s, "feat: part one, revised\n")
	require.Len(t, ids, 2, "%v", ids)
	require.Equal(t, redoCheckpointOne, ids[0], "the linked commit's trailer is still inherited")
	requireFreshTrailersResolve(t, ids)
}

// Session A only redid f1.txt, session B wrote f3.txt. Prepare stamps a fresh
// trailer for B, and post-commit must write it. A may fold into the same
// checkpoint through its redone file: post-commit judging overlap without the
// redo is what keeps every stamped trailer resolvable (a stale ACTIVE or a
// read-only session can depend on it), and a redundant transcript slice is
// preferred to a dangling trailer.
func TestRedo_TwoSessionPartialRedoFreshTrailerResolves(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	setupSessionWithCheckpointAndFile(t, s, dir, "sess-redid", "f1.txt")
	setupSessionWithCheckpointAndFile(t, s, dir, "sess-new-work", "f3.txt")
	testutil.WriteFile(t, dir, "f1.txt", "one\n") // the redone content
	setPhase(t, s, "sess-redid", session.PhaseEnded, time.Now())
	setPhase(t, s, "sess-new-work", session.PhaseEnded, time.Now())
	testutil.GitAdd(t, dir, "f1.txt", "f3.txt")

	ids := commitThroughHooks(t, dir, s, "feat: part one plus three\n")
	require.Len(t, ids, 2, "%v", ids)
	require.Equal(t, redoCheckpointOne, ids[0])
	requireFreshTrailersResolve(t, ids)
	newWork, err := s.loadSessionState(context.Background(), "sess-new-work")
	require.NoError(t, err)
	require.Equal(t, ids[1], newWork.LastCheckpointID.String())
}

// commitThroughHooks runs prepare-commit-msg, the commit, and post-commit, and
// returns the commit's checkpoint trailers.
func commitThroughHooks(t *testing.T, dir string, s *ManualCommitStrategy, message string) []string {
	t.Helper()
	msg := prepareMessage(t, message)
	msgFile := filepath.Join(t.TempDir(), "MSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(msg), 0o600))
	testutil.RunGit(t, dir, "commit", "-q", "-F", msgFile)
	require.NoError(t, s.PostCommit(context.Background()))
	return checkpointIDs(msg)
}

// requireFreshTrailersResolve asserts every trailer other than the redone
// commits' was written to the checkpoint store: a fresh trailer post-commit
// never condenses into leaves the commit pointing at nothing.
func requireFreshTrailersResolve(t *testing.T, ids []string) {
	t.Helper()
	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	stored, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).List(t.Context())
	require.NoError(t, err)
	written := make(map[string]bool, len(stored))
	for _, c := range stored {
		written[c.CheckpointID.String()] = true
	}
	for _, cpID := range ids {
		if cpID == redoCheckpointOne || cpID == redoCheckpointTwo {
			continue
		}
		require.True(t, written[cpID], "fresh trailer %s has no checkpoint behind it", cpID)
	}
}

// setPhase moves a seeded session to phase, with its last interaction at last.
func setPhase(t *testing.T, s *ManualCommitStrategy, sessionID string, phase session.Phase, last time.Time) {
	t.Helper()
	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	state.Phase = phase
	state.LastInteractionTime = &last
	require.NoError(t, s.saveSessionState(context.Background(), state))
}

// A stale ACTIVE session (agent killed without a Stop hook) whose only tracked
// file is redone: the fast path stamps it, so post-commit must condense it.
func TestRedo_StaleActiveSessionFreshTrailerResolves(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	setupSessionWithCheckpointAndFile(t, s, dir, "sess-stale", "f1.txt")
	testutil.WriteFile(t, dir, "f1.txt", "one\n")
	setPhase(t, s, "sess-stale", session.PhaseActive, time.Now().Add(-24*time.Hour))
	testutil.WriteFile(t, dir, "notes.txt", "notes\n")
	testutil.GitAdd(t, dir, "f1.txt", "notes.txt")

	ids := commitThroughHooks(t, dir, s, "feat: part one plus notes\n")
	require.Len(t, ids, 2, "%v", ids)
	requireFreshTrailersResolve(t, ids)
}

// An ACTIVE read-only session is stamped by the fast path while an ENDED
// session claims the commit only through a redone file. One of them must
// condense into the fresh trailer.
func TestRedo_ReadOnlyActiveSessionWithRedoOnlyClaimantResolves(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	setupSessionWithCheckpointAndFile(t, s, dir, "sess-redid", "f1.txt")
	testutil.WriteFile(t, dir, "f1.txt", "one\n")
	setPhase(t, s, "sess-redid", session.PhaseEnded, time.Now())
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(testTranscriptPromptResponse), 0o600))
	now := time.Now()
	require.NoError(t, SaveSessionState(context.Background(), &SessionState{
		SessionID:           "sess-reader",
		BaseCommit:          testutil.GetHeadHash(t, dir),
		WorktreePath:        dir,
		AgentType:           agent.AgentTypeClaudeCode,
		TranscriptPath:      transcriptPath,
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
	}))
	testutil.WriteFile(t, dir, "notes.txt", "notes\n")
	testutil.GitAdd(t, dir, "f1.txt", "notes.txt")

	ids := commitThroughHooks(t, dir, s, "feat: part one plus notes\n")
	require.Len(t, ids, 2, "%v", ids)
	requireFreshTrailersResolve(t, ids)
}

// seedTurnEndSession records a turn end that touched files with their current
// content, then moves the session to phase.
func seedTurnEndSession(t *testing.T, s *ManualCommitStrategy, dir, sessionID string, phase session.Phase, files ...string) {
	t.Helper()
	metadataDir := ".entire/metadata/" + sessionID
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, metadataDir, paths.TranscriptFileName), []byte(testTranscriptPromptResponse), 0o644))
	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:     sessionID,
		ModifiedFiles: []string{},
		NewFiles:      files,
		DeletedFiles:  []string{},
		MetadataDir:   metadataDir,
		CommitMessage: "Checkpoint 1",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))
	setPhase(t, s, sessionID, phase, time.Now())
}

// A pure redo stamps no fresh trailer, so nothing condenses; the redone file
// must still leave the session's pending set, while its other pending file
// stays. A later human edit to the redone file is not the session's work.
func TestRedo_PureRedoSettlesOnlyRedoneFiles(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	testutil.WriteFile(t, dir, "wip.txt", "agent work in progress\n")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redid", session.PhaseEnded, "f1.txt", "wip.txt")
	testutil.GitAdd(t, dir, "f1.txt")

	ids := commitThroughHooks(t, dir, s, "feat: part one (logical)\n")
	require.Equal(t, []string{redoCheckpointOne}, ids)
	state, err := s.loadSessionState(context.Background(), "sess-redid")
	require.NoError(t, err)
	require.Equal(t, []string{"wip.txt"}, state.FilesTouched, "only the redone file is settled")
	require.NotContains(t, state.TouchedFileHashes, "f1.txt")
	require.Contains(t, state.TouchedFileHashes, "wip.txt")

	testutil.WriteFile(t, dir, "f1.txt", "one, edited by a human\n")
	testutil.GitAdd(t, dir, "f1.txt")
	require.Empty(t, commitThroughHooks(t, dir, s, "fix: tweak part one\n"))
	state, err = s.loadSessionState(context.Background(), "sess-redid")
	require.NoError(t, err)
	require.Equal(t, []string{"wip.txt"}, state.FilesTouched)
}

// A trailer whose checkpoint this clone does not have (its post-commit
// condensation failed) is still inherited as a link, but it cannot stand in
// for capture: the session's pending work gets a fresh checkpoint.
func TestRedo_MissingInheritedCheckpointKeepsFreshCapture(t *testing.T) {
	dir := redoFixture(t)
	const unwritten = "a1b2c3d4e509"
	testutil.WriteFile(t, dir, "f3.txt", "three\n")
	testutil.GitAdd(t, dir, "f3.txt")
	testutil.GitCommit(t, dir, "agent: part three\n\nEntire-Checkpoint: "+unwritten)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~1")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-pending", session.PhaseEnded, "f3.txt")
	testutil.GitAdd(t, dir, "f3.txt")

	ids := commitThroughHooks(t, dir, s, "redo after a failed condensation\n")
	require.Len(t, ids, 2, "%v", ids)
	require.Equal(t, unwritten, ids[0], "the link is still inherited")
	requireFreshTrailersResolve(t, ids[1:])
	state, err := s.loadSessionState(context.Background(), "sess-pending")
	require.NoError(t, err)
	require.Equal(t, ids[1], state.LastCheckpointID.String())
}

// A checkpoint deleted from this clone is neither inherited nor allowed to
// hold the redone file.
func TestRedo_DeletedInheritedCheckpointKeepsFreshCapture(t *testing.T) {
	dir := redoFixture(t)
	recordDeleted(t, id.MustCheckpointID(redoCheckpointOne))
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-pending", session.PhaseEnded, "f1.txt")
	testutil.GitAdd(t, dir, "f1.txt")

	ids := commitThroughHooks(t, dir, s, "redo of a deleted checkpoint\n")
	require.Len(t, ids, 1, "%v", ids)
	require.NotEqual(t, redoCheckpointOne, ids[0])
	requireFreshTrailersResolve(t, ids)
}

// Git pairs a staged deletion with an identical added file as a rename. The
// held file must not hide the deletion: the redo and the session check read
// the same rename-free staged paths, and the human's deletion is no session's
// work.
func TestRedo_RenamePairedDeletionStampsNoFreshTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.WriteFile(t, dir, "a.txt", "same\n")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "source")
	testutil.WriteFile(t, dir, "b.txt", "same\n")
	testutil.GitAdd(t, dir, "b.txt")
	testutil.GitCommit(t, dir, "destination\n\nEntire-Checkpoint: "+redoCheckpointOne)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~1")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redid", session.PhaseIdle, "b.txt")
	testutil.RunGit(t, dir, "rm", "-q", "a.txt")
	testutil.GitAdd(t, dir, "b.txt")
	got := prepareMessage(t, "redo with a human deletion\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got))
}

// `git commit -a` and path-limited commits prepare the commit in a temporary
// index. The redo reads content and mode from it, not from .git/index.
func TestRedo_ReadsTheTemporaryCommitIndex(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redid", session.PhaseEnded, "f1.txt")
	// The ordinary index stages f1.txt executable; the commit index stages it
	// exactly as the dropped commit left it.
	testutil.GitAdd(t, dir, "f1.txt")
	testutil.RunGit(t, dir, "update-index", "--chmod=+x", "f1.txt")
	commitIndex := filepath.Join(dir, ".git", "entire-test-commit-index")
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"add", "f1.txt"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+commitIndex)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	t.Setenv("GIT_INDEX_FILE", commitIndex)
	got := prepareMessage(t, "feat: part one (logical)\n")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(got), "%q", got)
}

// A commit aborted after prepare leaves its marker behind. The next commit's
// prepare clears it, so a later trailer-less commit cannot settle the aborted
// redo's paths out of a session that never committed them.
func TestRedo_AbortedCommitMarkerIsNotReused(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redid", session.PhaseEnded, "f1.txt")
	testutil.GitAdd(t, dir, "f1.txt")
	require.Equal(t, []string{redoCheckpointOne}, checkpointIDs(prepareMessage(t, "aborted redo\n")))

	testutil.RunGit(t, dir, "reset", "-q", "--", "f1.txt")
	testutil.WriteFile(t, dir, "notes.txt", "human notes\n")
	testutil.GitAdd(t, dir, "notes.txt")
	require.Empty(t, commitThroughHooks(t, dir, s, "docs: notes\n"))
	state, err := s.loadSessionState(context.Background(), "sess-redid")
	require.NoError(t, err)
	require.Contains(t, state.FilesTouched, "f1.txt", "f1.txt was never committed; it is still the session's pending work")
}

// The cleanup after a redo settles held files only. Other touched files are
// left exactly as they were, even ones the carry-forward check would prune on
// its own (a path that exists nowhere): that is the next condensation's call.
func TestRedo_PureRedoLeavesOtherTouchedFilesAlone(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~2")
	s := NewManualCommitStrategy()
	seedTurnEndSession(t, s, dir, "sess-redid", session.PhaseEnded, "f1.txt")
	state, err := s.loadSessionState(context.Background(), "sess-redid")
	require.NoError(t, err)
	state.FilesTouched = append(state.FilesTouched, "never-created.txt")
	require.NoError(t, s.saveSessionState(context.Background(), state))
	testutil.GitAdd(t, dir, "f1.txt")

	require.Equal(t, []string{redoCheckpointOne}, commitThroughHooks(t, dir, s, "feat: part one (logical)\n"))
	state, err = s.loadSessionState(context.Background(), "sess-redid")
	require.NoError(t, err)
	require.Equal(t, []string{"never-created.txt"}, state.FilesTouched)
}
