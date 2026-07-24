package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestManualCommitStrategy_FindSessionsForWorktree_MatchesParentSessionFromNestedWorktree(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	worktreeDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, worktreeDir, "feature")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, worktreeDir) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "parent-session",
		WorktreePath: mainDir,
	})

	t.Chdir(worktreeDir)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, worktreeDir)
	require.NoError(t, err)
	require.Len(t, matching, 1)
	require.Equal(t, "parent-session", matching[0].SessionID)
}

func TestManualCommitStrategy_PrepareCommitMsg_AddsTrailerForParentSessionFromNestedWorktree(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	worktreeDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, worktreeDir, "feature")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, worktreeDir) })

	saver := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, saver, mainDir, &SessionState{
		SessionID:    "parent-session",
		WorktreePath: mainDir,
		FilesTouched: []string{"smoke.txt"},
		StepCount:    1,
	})

	t.Chdir(worktreeDir)
	clearSessionMatchCaches()

	commitMsgFile := filepath.Join(worktreeDir, "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(commitMsgFile, []byte("smoke commit\n"), 0o600))

	hook := &ManualCommitStrategy{}
	require.NoError(t, hook.PrepareCommitMsg(ctx, commitMsgFile, "message"))

	content, err := os.ReadFile(commitMsgFile)
	require.NoError(t, err)
	cpID, found := trailers.ParseCheckpoint(string(content))
	require.True(t, found, "prepare-commit-msg should add a checkpoint trailer from the parent-recorded session")
	require.False(t, cpID.IsEmpty())
}

func TestManualCommitStrategy_FindSessionsForWorktree_MatchesUniqueSiblingByCommonDir(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	recordedWorktree := resolvedRemovedTempDir(t)
	commitWorktree := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, recordedWorktree, "recorded")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, recordedWorktree) })
	createSessionMatchWorktree(t, mainDir, commitWorktree, "commit")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, commitWorktree) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "unique-sibling-session",
		WorktreePath: recordedWorktree,
	})

	t.Chdir(commitWorktree)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, commitWorktree)
	require.NoError(t, err)
	require.Len(t, matching, 1)
	require.Equal(t, "unique-sibling-session", matching[0].SessionID)
}

func TestManualCommitStrategy_FindSessionsForWorktree_ExactMatchWinsOverSiblingFallback(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	worktreeDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, worktreeDir, "feature")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, worktreeDir) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "parent-session",
		WorktreePath: mainDir,
	})
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "exact-session",
		WorktreePath: worktreeDir,
	})

	t.Chdir(worktreeDir)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, worktreeDir)
	require.NoError(t, err)
	require.Len(t, matching, 1)
	require.Equal(t, "exact-session", matching[0].SessionID)
}

func TestManualCommitStrategy_FindSessionsForWorktree_DoesNotMatchUnrelatedRepo(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	otherDir := setupSessionMatchRepo(t)

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "unrelated-session",
		WorktreePath: otherDir,
	})

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, mainDir)
	require.NoError(t, err)
	require.Empty(t, matching)
}

func TestManualCommitStrategy_FindSessionsForWorktree_DoesNotGuessAmbiguousSiblingSessions(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	firstWorktree := resolvedRemovedTempDir(t)
	secondWorktree := resolvedRemovedTempDir(t)
	commitWorktree := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, firstWorktree, "first")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, firstWorktree) })
	createSessionMatchWorktree(t, mainDir, secondWorktree, "second")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, secondWorktree) })
	createSessionMatchWorktree(t, mainDir, commitWorktree, "commit")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, commitWorktree) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "first-session",
		WorktreePath: firstWorktree,
	})
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "second-session",
		WorktreePath: secondWorktree,
	})

	t.Chdir(commitWorktree)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, commitWorktree)
	require.NoError(t, err)
	require.Empty(t, matching)
}

func TestManualCommitStrategy_FindSessionsForWorktree_ReturnsConcurrentSessionsFromParent(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	worktreeDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, worktreeDir, "feature")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, worktreeDir) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "parent-session-a",
		WorktreePath: mainDir,
	})
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "parent-session-b",
		WorktreePath: mainDir,
	})

	t.Chdir(worktreeDir)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, worktreeDir)
	require.NoError(t, err)
	require.Len(t, matching, 2, "concurrent sessions recorded in the same parent worktree should all match")
}

func TestManualCommitStrategy_FindSessionsForWorktree_ReturnsConcurrentSessionsFromSameSibling(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	recordedWorktree := resolvedRemovedTempDir(t)
	commitWorktree := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, recordedWorktree, "recorded")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, recordedWorktree) })
	createSessionMatchWorktree(t, mainDir, commitWorktree, "commit")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, commitWorktree) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "sibling-session-a",
		WorktreePath: recordedWorktree,
	})
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "sibling-session-b",
		WorktreePath: recordedWorktree,
	})

	t.Chdir(commitWorktree)
	clearSessionMatchCaches()

	finder := &ManualCommitStrategy{}
	matching, err := finder.findSessionsForWorktree(ctx, commitWorktree)
	require.NoError(t, err)
	require.Len(t, matching, 2, "concurrent sessions recorded in the same sibling worktree should all match")
}

func TestManualCommitStrategy_PostCommitBaseUpdate_DoesNotRewriteSiblingSessionBase(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	recordedWorktree := resolvedRemovedTempDir(t)
	commitWorktree := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, recordedWorktree, "recorded")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, recordedWorktree) })
	createSessionMatchWorktree(t, mainDir, commitWorktree, "commit")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, commitWorktree) })

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "sibling-session",
		WorktreePath: recordedWorktree,
	})
	originalBase := testutil.GetHeadHash(t, mainDir)

	// Trailer-less commit in the sibling worktree: the fallback would match
	// the recorded session here, but BaseCommit must only follow the HEAD of
	// the session's own worktree (shadow branches are keyed off it).
	t.Chdir(commitWorktree)
	clearSessionMatchCaches()
	testutil.WriteFile(t, commitWorktree, "untracked.txt", "manual\n")
	testutil.GitAdd(t, commitWorktree, "untracked.txt")
	testutil.GitCommit(t, commitWorktree, "manual commit without trailer")
	newHead := testutil.GetHeadHash(t, commitWorktree)
	require.NotEqual(t, originalBase, newHead)

	hook := &ManualCommitStrategy{}
	hook.postCommitUpdateBaseCommitOnly(ctx, plumbing.NewHashReference(plumbing.HEAD, plumbing.NewHash(newHead)))

	reloaded, err := hook.loadSessionState(ctx, "sibling-session")
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	require.Equal(t, originalBase, reloaded.BaseCommit,
		"trailer-less commit in a sibling worktree must not rewrite the recorded session's BaseCommit")
}

func TestManualCommitStrategy_PostCommitBaseUpdate_StillAdvancesExactMatchSessionBase(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)

	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "exact-session",
		WorktreePath: mainDir,
	})
	originalBase := testutil.GetHeadHash(t, mainDir)

	t.Chdir(mainDir)
	clearSessionMatchCaches()
	testutil.WriteFile(t, mainDir, "untracked.txt", "manual\n")
	testutil.GitAdd(t, mainDir, "untracked.txt")
	testutil.GitCommit(t, mainDir, "manual commit without trailer")
	newHead := testutil.GetHeadHash(t, mainDir)
	require.NotEqual(t, originalBase, newHead)

	hook := &ManualCommitStrategy{}
	hook.postCommitUpdateBaseCommitOnly(ctx, plumbing.NewHashReference(plumbing.HEAD, plumbing.NewHash(newHead)))

	reloaded, err := hook.loadSessionState(ctx, "exact-session")
	require.NoError(t, err)
	require.NotNil(t, reloaded)
	require.Equal(t, newHead, reloaded.BaseCommit,
		"trailer-less commit in the session's own worktree must still advance BaseCommit")
}

func TestGitCommonDirForWorktree_IgnoresHookGitDirEnv(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	mainDir := setupSessionMatchRepo(t)
	otherDir := setupSessionMatchRepo(t)

	// Git hooks export GIT_DIR for the hook's own repo; resolution for a
	// different worktree must not be redirected by it.
	t.Setenv("GIT_DIR", filepath.Join(otherDir, ".git"))
	t.Chdir(otherDir)

	commonDir, err := gitCommonDirForWorktree(ctx, mainDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(mainDir, ".git"), commonDir)
}

func setupSessionMatchRepo(t *testing.T) string {
	t.Helper()

	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "test\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")
	return dir
}

func saveSessionMatchState(ctx context.Context, t *testing.T, s *ManualCommitStrategy, repoDir string, state *SessionState) {
	t.Helper()

	t.Chdir(repoDir)
	clearSessionMatchCaches()

	now := time.Now()
	state.StartedAt = now
	state.Phase = session.PhaseActive
	state.BaseCommit = testutil.GetHeadHash(t, repoDir)
	if state.WorktreeID == "" && state.WorktreePath != "" {
		worktreeID, err := paths.GetWorktreeID(state.WorktreePath)
		require.NoError(t, err)
		state.WorktreeID = worktreeID
	}
	require.NoError(t, s.saveSessionState(ctx, state))
}

func createSessionMatchWorktree(t *testing.T, repoDir, worktreeDir, branch string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(worktreeDir), 0o755))
	cmd := exec.CommandContext(context.Background(), "git", "worktree", "add", worktreeDir, "-b", branch)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git worktree add output:\n%s", output)
}

func removeSessionMatchWorktree(repoDir, worktreeDir string) {
	cmd := exec.CommandContext(context.Background(), "git", "worktree", "remove", worktreeDir, "--force")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	_ = cmd.Run() //nolint:errcheck // best-effort test cleanup
}

func resolvedRemovedTempDir(t *testing.T) string {
	t.Helper()

	dir := resolvedTempDir(t)
	require.NoError(t, os.Remove(dir))
	return dir
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return resolved
}

func clearSessionMatchCaches() {
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
}
