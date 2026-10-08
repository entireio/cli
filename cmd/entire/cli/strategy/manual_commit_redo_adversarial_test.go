package strategy

// Adversarial review of the redo half of PR #2574 (redone commits keep their
// checkpoint trailers). Each test asserts what a user would WANT; a failing
// test means the PR does something the user did not ask for. Tests marked
// GUARD document workflows the PR already handles correctly.
//
// These exercise PrepareCommitMsg only (trailer inheritance). The PostCommit
// condensation consequences are covered by
// integration_test/rewrite_adversarial_test.go.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	advCheckpointA = "01M2VBJBJQZ2BP1W2PBWDF3J41"
	advCheckpointB = "01M2VBJBJQZ2BP1W2PBWDF3J42"
	advCheckpointC = "01M2VBJBJQZ2BP1W2PBWDF3J43"
)

// advRepo creates an isolated repo with one base commit. Commits in these tests
// go through the real git CLI (not go-git) so HEAD's reflog looks exactly like
// a user's, which resetIsLatestRefOperation depends on.
func advRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	for path, content := range files {
		testutil.WriteFile(t, dir, path, content)
	}
	advCommitAll(t, dir, "init")
	return dir
}

func advCommitAll(t *testing.T, dir, message string) {
	t.Helper()
	testutil.RunGit(t, dir, "add", "-A")
	testutil.RunGit(t, dir, "commit", "-q", "--no-verify", "-m", message)
}

// Seed B. Workflow: an agent attempt adds a dependency (go.mod + go.sum + code)
// and is committed with checkpoint A. The user throws the attempt away with
// `git reset --hard HEAD~1` and writes a different implementation by hand that
// happens to need the same dependency, so go.sum is byte-identical.
// User wants: the new commit is not linked to the discarded attempt's session.
// PR does: go.sum is "staged with exactly its ORIG_HEAD content", so the
// discarded attempt's trailer is inherited.
func TestAdversarial_DiscardedAttemptSharedLockfileIsNotARedo(t *testing.T) {
	dir := advRepo(t, map[string]string{"go.mod": "module x\n", "main.go": "package main\n"})
	t.Chdir(dir)
	testutil.WriteFile(t, dir, "go.mod", "module x\n\nrequire example.com/dep v1.2.3\n")
	testutil.WriteFile(t, dir, "go.sum", "example.com/dep v1.2.3 h1:abc=\n")
	testutil.WriteFile(t, dir, "main.go", "package main\n// agent approach: goroutine pool\n")
	advCommitAll(t, dir, "agent: approach one\n\nEntire-Checkpoint: "+advCheckpointA)

	testutil.RunGit(t, dir, "reset", "-q", "--hard", "HEAD~1")

	// Hand-written second approach, same dependency.
	testutil.WriteFile(t, dir, "go.mod", "module x\n\nrequire example.com/dep v1.2.3\n\n// pinned\n")
	testutil.WriteFile(t, dir, "go.sum", "example.com/dep v1.2.3 h1:abc=\n")
	testutil.WriteFile(t, dir, "main.go", "package main\n// hand-written approach: channels\n")
	testutil.RunGit(t, dir, "add", "-A")

	got := advPrepare(t, "feat: channel-based approach\n", "message")
	require.Empty(t, checkpointIDs(got),
		"a discarded attempt's trailer must not ride along because one lockfile is byte-identical: %q", got)
}

// Seed B, deletion variant. Workflow: attempt one deletes a legacy file (plus
// other edits) and is discarded with `reset --hard`; the user's own rewrite also
// deletes that legacy file. Convergent deletions are very common.
// User wants: no link to the discarded attempt.
// PR does: a staged deletion of a path absent at ORIG_HEAD counts as a redo.
func TestAdversarial_DiscardedAttemptConvergentDeletionIsNotARedo(t *testing.T) {
	dir := advRepo(t, map[string]string{"legacy.go": "package legacy\n", "api.go": "package api\n"})
	t.Chdir(dir)
	testutil.RunGit(t, dir, "rm", "-q", "legacy.go")
	testutil.WriteFile(t, dir, "api.go", "package api\n// agent rewrite\n")
	advCommitAll(t, dir, "agent: drop legacy, rewrite api\n\nEntire-Checkpoint: "+advCheckpointA)

	testutil.RunGit(t, dir, "reset", "-q", "--hard", "HEAD~1")

	testutil.RunGit(t, dir, "rm", "-q", "legacy.go")
	testutil.WriteFile(t, dir, "api.go", "package api\n// my own rewrite, nothing like the agent's\n")
	testutil.RunGit(t, dir, "add", "-A")

	got := advPrepare(t, "refactor: remove legacy package\n", "message")
	require.Empty(t, checkpointIDs(got),
		"deleting the same file as a discarded attempt is not redoing that attempt: %q", got)
}

// Reflog window. Workflow: discard an attempt with `reset --hard HEAD~1`, keep
// working on the same branch for several ordinary commits (no checkout, pull,
// or rebase in between), then days later restore an empty __init__.py-style
// file the discarded attempt had also created.
// User wants: no link; the reset was many commits ago.
// PR does: only non-"commit" reflog entries end the window, so the reset still
// counts as "the latest ref operation" and the old trailer is inherited.
func TestAdversarial_RedoWindowOutlivesManyLaterCommits(t *testing.T) {
	dir := advRepo(t, nil)
	t.Chdir(dir)
	testutil.WriteFile(t, dir, "pkg/__init__.py", "")
	testutil.WriteFile(t, dir, "pkg/impl.py", "agent attempt\n")
	advCommitAll(t, dir, "agent: attempt\n\nEntire-Checkpoint: "+advCheckpointA)
	testutil.RunGit(t, dir, "reset", "-q", "--hard", "HEAD~1")

	for i, name := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		testutil.WriteFile(t, dir, name, name+"\n")
		advCommitAll(t, dir, "unrelated work "+string(rune('1'+i)))
	}

	testutil.WriteFile(t, dir, "pkg/__init__.py", "")
	testutil.WriteFile(t, dir, "pkg/mine.py", "my own package\n")
	testutil.RunGit(t, dir, "add", "-A")

	got := advPrepare(t, "feat: new package\n", "message")
	require.Empty(t, checkpointIDs(got),
		"four ordinary commits after a reset, restoring an empty file is not a redo: %q", got)
}

// Walk past the merge base. Workflow: a feature branch forked from main at A,
// main moved on, the user ran `git merge main` into the feature branch, then
// squashed the branch with `git reset --soft main` and committed. A is an old
// teammate commit (with its own trailer) that already lives on main and that
// touched shared.txt, which the feature also edits.
// User wants: only the feature commit's trailer is inherited.
// PR does: replacedCommits walks ORIG_HEAD with only the merge base as the
// stop hash. The merge's second parent leads into main's history below that
// base, so commits that are ancestors of HEAD (A, and up to 200 more) are
// treated as "dropped". A matches because shared.txt is staged with the
// ORIG_HEAD (feature) version.
func TestAdversarial_ResetSoftAfterMergingMainDoesNotInheritMainHistory(t *testing.T) {
	dir := advRepo(t, map[string]string{"shared.txt": "v0\n"})
	t.Chdir(dir)
	// A: an old teammate commit already on main.
	testutil.WriteFile(t, dir, "shared.txt", "teammate v1\n")
	advCommitAll(t, dir, "teammate: shared tweak\n\nEntire-Checkpoint: "+advCheckpointA)

	testutil.RunGit(t, dir, "checkout", "-q", "-b", "feature")
	testutil.WriteFile(t, dir, "shared.txt", "teammate v1\nfeature line\n")
	advCommitAll(t, dir, "agent: feature\n\nEntire-Checkpoint: "+advCheckpointB)

	testutil.RunGit(t, dir, "checkout", "-q", "master")
	testutil.WriteFile(t, dir, "other.txt", "teammate other\n")
	advCommitAll(t, dir, "teammate: other work\n\nEntire-Checkpoint: "+advCheckpointC)

	testutil.RunGit(t, dir, "checkout", "-q", "feature")
	testutil.RunGit(t, dir, "merge", "-q", "--no-edit", "master")
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "master")

	got := advPrepare(t, "feat: the feature (squashed)\n", "message")
	require.Equal(t, []string{advCheckpointB}, checkpointIDs(got),
		"only the feature's own commit was dropped; main's history must not be inherited: %q", got)
}

// Undo a rebase. Workflow: `git rebase main` pulled in a teammate commit that
// changed config.yml; the user undoes the rebase with `git reset --hard
// ORIG_HEAD`, then later takes just the teammate's config.yml
// (`git checkout main -- config.yml`) and commits it with their own change.
// User wants: the commit is not linked to the teammate's checkpoint (the
// teammate's commit keeps its own link on main).
// PR does: the rebased tip becomes ORIG_HEAD; the teammate commit is between
// the merge base and ORIG_HEAD, so it counts as "dropped" and its trailer is
// inherited.
func TestAdversarial_UndoRebaseThenTakeUpstreamFileDoesNotInheritTeammate(t *testing.T) {
	dir := advRepo(t, map[string]string{"config.yml": "a: 1\n"})
	t.Chdir(dir)
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "feature")
	testutil.WriteFile(t, dir, "feature.txt", "mine\n")
	advCommitAll(t, dir, "my feature")
	testutil.RunGit(t, dir, "checkout", "-q", "master")
	testutil.WriteFile(t, dir, "config.yml", "a: 2\n")
	advCommitAll(t, dir, "teammate: bump config\n\nEntire-Checkpoint: "+advCheckpointA)
	testutil.RunGit(t, dir, "checkout", "-q", "feature")
	testutil.RunGit(t, dir, "rebase", "-q", "master")
	testutil.RunGit(t, dir, "reset", "-q", "--hard", "ORIG_HEAD") // undo the rebase

	testutil.RunGit(t, dir, "checkout", "master", "--", "config.yml")
	testutil.WriteFile(t, dir, "feature.txt", "mine, v2\n")
	testutil.RunGit(t, dir, "add", "-A")

	got := advPrepare(t, "feat: adopt new config\n", "message")
	require.Empty(t, checkpointIDs(got),
		"the teammate's commit was never dropped from main: %q", got)
}

// False miss. Workflow: `git reset --soft HEAD~2` to squash two linked commits,
// then plain `git reset` to unstage everything and re-add selectively (or
// `git stash` / `git stash pop`, which also runs `reset: moving to HEAD`).
// User wants: the squashed commit keeps both trailers.
// PR does: the second reset rewrites ORIG_HEAD to HEAD, so nothing counts as
// dropped and both trailers are lost (same as main, but the PR's promise is
// that redone commits keep their trailers).
func TestAdversarial_UnstageAfterSoftResetKeepsRedoTrailers(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~2")
	testutil.RunGit(t, dir, "reset", "-q")
	testutil.RunGit(t, dir, "add", "f1.txt", "f2.txt")

	got := advPrepare(t, "feat: the feature (logical)\n", "message")
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got),
		"unstaging between reset and commit must not lose the redo: %q", got)
}

// False miss. Workflow: fold the last commit into the previous one with
// `git reset --soft HEAD~1 && git commit --amend --no-edit` (a very common
// "squash last two" idiom).
// User wants: the amended commit carries both commits' trailers.
// PR does: prepare-commit-msg with source "commit" goes to handleAmendCommitMsg
// before inheritReplacedCommitsTrailers, so the folded commit's trailer is lost
// (same as main).
func TestAdversarial_AmendFoldKeepsFoldedCommitTrailer(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "--soft", "HEAD~1")

	got := advPrepare(t, "agent: part one\n\nEntire-Checkpoint: "+redoCheckpointOne+"\n", "commit")
	require.Equal(t, []string{redoCheckpointOne, redoCheckpointTwo}, checkpointIDs(got),
		"folding HEAD into HEAD~1 via amend is a squash: %q", got)
}

// GUARD. Workflow: `git reset HEAD~1` to undo the last commit, then commit only
// an unrelated file (leaving the undone work unstaged).
// User wants and PR does: no inheritance.
func TestAdversarial_Guard_UndoThenCommitUnrelatedFileInheritsNothing(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "reset", "-q", "HEAD~1")
	testutil.WriteFile(t, dir, "unrelated.txt", "hotfix\n")
	testutil.RunGit(t, dir, "add", "unrelated.txt")

	got := advPrepare(t, "fix: hotfix\n", "message")
	require.Empty(t, checkpointIDs(got), "%q", got)
}

// GUARD. Workflow: `git stash` rewrites ORIG_HEAD to HEAD and writes a
// "reset: moving to HEAD" reflog entry. After a completed rebase (stale
// ORIG_HEAD) a stash must not revive the rebase's ORIG_HEAD as a redo.
// User wants and PR does: no inheritance (stash overwrote ORIG_HEAD).
func TestAdversarial_Guard_StashAfterRebaseDoesNotReviveStaleOrigHead(t *testing.T) {
	dir := redoFixture(t)
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "base", "HEAD~2")
	testutil.WriteFile(t, dir, "other.txt", "other\n")
	advCommitAll(t, dir, "unrelated base work")
	testutil.RunGit(t, dir, "checkout", "-q", "-")
	testutil.RunGit(t, dir, "rebase", "-q", "base")
	testutil.WriteFile(t, dir, "scratch.txt", "wip\n")
	testutil.RunGit(t, dir, "add", "scratch.txt")
	testutil.RunGit(t, dir, "stash", "-q")
	testutil.RunGit(t, dir, "stash", "pop", "-q")
	// Restore f2.txt's pre-rebase blob after deleting it.
	testutil.RunGit(t, dir, "rm", "-q", "f2.txt")
	advCommitAll(t, dir, "drop f2")
	testutil.WriteFile(t, dir, "f2.txt", "two\n")
	testutil.RunGit(t, dir, "add", "f2.txt")

	got := advPrepare(t, "bring f2 back\n", "message")
	require.Empty(t, checkpointIDs(got), "%q", got)
}
