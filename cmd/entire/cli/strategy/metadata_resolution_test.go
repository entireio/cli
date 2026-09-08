package strategy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestSessionLocksDeduplicatePhysicalRepositories(t *testing.T) {
	t.Parallel()
	main := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked", linked)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(main, alias))
	other := setupGitRepo(t)
	var commonDirs []string
	for _, root := range []string{main, linked, alias, other} {
		metadata, err := gitrepo.ResolveWorktreeMetadata(root)
		require.NoError(t, err)
		commonDirs = append(commonDirs, metadata.CommonDir)
	}
	aliasGitDir, err := getGitDirInPath(t.Context(), alias)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(alias, ".git"), aliasGitDir, "caller-visible paths retain their lexical spelling")
	ordered, err := sessionLockCommonDirs(commonDirs)
	require.NoError(t, err)
	require.Len(t, ordered, 2)
	reverse, err := sessionLockCommonDirs([]string{commonDirs[3], commonDirs[2], commonDirs[1], commonDirs[0]})
	require.NoError(t, err)
	require.Equal(t, ordered, reverse, "input order and aliases must not affect acquisition order")

	callbackErr := errors.New("callback failed")
	err = WithSessionStateLocks(t.Context(), "shared-session", commonDirs, func() error {
		for _, dir := range ordered {
			lock, err := stateLockInCommonDir(dir, "shared-session")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			release, err := flock.AcquireContextIn(ctx, lock.root, lock.name)
			cancel()
			if release != nil {
				release()
			}
			require.ErrorIs(t, err, context.DeadlineExceeded, "each repository must be locked during the callback")
		}
		return callbackErr
	})
	require.ErrorIs(t, err, callbackErr)
	for _, dir := range ordered {
		requireSessionLockReleased(t, dir, "shared-session")
	}
}

func TestSessionLocksOrderIndependentOfCaseAliases(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	first := filepath.Join(base, "alpha")
	second := filepath.Join(base, "Beta")
	require.NoError(t, os.Mkdir(first, 0o750))
	require.NoError(t, os.Mkdir(second, 0o750))
	testutil.InitRepo(t, first)
	testutil.InitRepo(t, second)
	alias := filepath.Join(base, "Alpha")
	aliasInfo, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("requires a case-insensitive filesystem")
	}
	require.NoError(t, err)
	firstInfo, err := os.Stat(first)
	require.NoError(t, err)
	require.True(t, os.SameFile(firstInfo, aliasInfo))

	inputs := [][]string{
		{filepath.Join(first, ".git"), filepath.Join(second, ".git")},
		{filepath.Join(alias, ".git"), filepath.Join(second, ".git")},
	}
	var expected []os.FileInfo
	for _, dirs := range inputs {
		ordered, err := sessionLockCommonDirs(dirs)
		require.NoError(t, err)
		require.Len(t, ordered, 2)
		for i, dir := range ordered {
			info, err := os.Stat(dir)
			require.NoError(t, err)
			if len(expected) < len(ordered) {
				expected = append(expected, info)
			} else {
				require.True(t, os.SameFile(expected[i], info), "aliases must preserve physical lock order: %v", ordered)
			}
		}
		require.NoError(t, WithSessionStateLocks(t.Context(), "case-session", dirs, func() error { return nil }))
		for _, dir := range dirs {
			requireSessionLockReleased(t, dir, "case-session")
		}
	}
}

func TestSessionRoutingRecognizesRepositoryAliases(t *testing.T) {
	t.Parallel()
	main := setupGitRepo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(main, alias))
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked", linked)
	state := &SessionState{SessionID: "session", WorktreePath: alias}
	s := &ManualCommitStrategy{}
	matches, ambiguous := s.findSessionsForWorktreeFromStates(t.Context(), []*SessionState{state}, linked)
	require.False(t, ambiguous)
	require.Equal(t, []*SessionState{state}, matches)
	other := setupGitRepo(t)
	matches, ambiguous = s.findSessionsForWorktreeFromStates(t.Context(), []*SessionState{state}, other)
	require.False(t, ambiguous)
	require.Empty(t, matches)
}

func TestSessionLocksFailClosedBeforeCreatingLocks(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"empty", "missing", "dangling", "file"} {
		t.Run(invalid, func(t *testing.T) {
			t.Parallel()
			dir := setupGitRepo(t)
			common := filepath.Join(dir, ".git")
			bad := filepath.Join(t.TempDir(), "invalid")
			switch invalid {
			case "empty":
				bad = ""
			case "missing":
			case "dangling":
				require.NoError(t, os.Symlink("missing", bad))
			case "file":
				require.NoError(t, os.WriteFile(bad, []byte("file"), 0o600))
			}
			called := false
			err := WithSessionStateLocks(t.Context(), "session", []string{common, bad}, func() error {
				called = true
				return nil
			})
			require.Error(t, err)
			require.False(t, called)
			require.NoDirExists(t, filepath.Join(common, SessionLockDirName))
		})
	}
}

func TestSessionLocksReleaseOnAcquisitionFailureAndCancellation(t *testing.T) {
	t.Parallel()
	first := filepath.Join(setupGitRepo(t), ".git")
	second := filepath.Join(setupGitRepo(t), ".git")
	dirs, err := sessionLockCommonDirs([]string{first, second})
	require.NoError(t, err)
	blocked, err := stateLockInCommonDir(dirs[1], "session")
	require.NoError(t, err)
	require.NoError(t, os.Symlink("target", blocked.path))
	called := false
	err = WithSessionStateLocks(t.Context(), "session", dirs, func() error {
		called = true
		return nil
	})
	require.Error(t, err)
	require.False(t, called)
	requireSessionLockReleased(t, dirs[0], "session")
	require.NoFileExists(t, filepath.Join(filepath.Dir(blocked.path), "target"))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = WithSessionStateLocks(ctx, "canceled-session", dirs, func() error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
	for _, dir := range dirs {
		requireSessionLockReleased(t, dir, "canceled-session")
	}
}

func requireSessionLockReleased(t *testing.T, dir, sessionID string) {
	t.Helper()
	lock, err := stateLockInCommonDir(dir, sessionID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release, err := flock.AcquireContextIn(ctx, lock.root, lock.name)
	require.NoError(t, err, "a failed operation must release every acquired lock")
	release()
}

func TestStrategyMetadataUsesLinkedWorktreeAndSharedStorage(t *testing.T) {
	main := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked", linked)
	metadata, err := gitrepo.ResolveWorktreeMetadata(linked)
	require.NoError(t, err)
	ctx := t.Context()
	t.Chdir(linked)
	clearSessionMatchCaches()
	created, err := StoreAgentTypeHint(ctx, "shared-session", agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	require.True(t, created)
	require.FileExists(t, filepath.Join(main, ".git", session.SessionStateDirName, "shared-session.agent"))
	require.NoError(t, saveCapturedSyncRemote(ctx, "upstream"))
	require.NoError(t, os.WriteFile(filepath.Join(metadata.GitDir, "CHERRY_PICK_HEAD"), []byte("marker"), 0o600))
	require.True(t, isGitSequenceOperation(ctx))
	cacheDir := filepath.Join(main, ".git", checkpoint.RedactCacheDirName)
	require.NoError(t, os.MkdirAll(cacheDir, 0o750))
	require.NoError(t, deleteRedactCache(ctx))
	require.NoDirExists(t, cacheDir)

	t.Chdir(main)
	clearSessionMatchCaches()
	require.False(t, isGitSequenceOperation(ctx), "sequence markers belong only to their worktree")
	require.Equal(t, []string{"upstream"}, loadCapturedSyncRemotes(ctx))
	created, err = StoreAgentTypeHint(ctx, "shared-session", agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	require.False(t, created, "linked and main worktrees must share hint ownership")
}

func TestStrategyMetadataFailurePoliciesAndRepair(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	clearSessionMatchCaches()
	ctx := t.Context()
	root, err := openSessionStateRoot(ctx)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	commonFile := filepath.Join(dir, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))
	_, err = StoreAgentTypeHint(ctx, "broken-session", agent.AgentTypeClaudeCode)
	require.Error(t, err)
	_, err = stateLockForSession(ctx, "broken-session")
	require.Error(t, err)
	_, err = openSessionStateRootForRead(ctx)
	require.Error(t, err, "broken metadata must not be mistaken for an absent session directory")
	require.Error(t, ClearSessionState(ctx, "broken-session"))
	require.Nil(t, loadCapturedSyncRemotes(ctx))
	require.False(t, isGitSequenceOperation(ctx))
	var warning bytes.Buffer
	warnStaleEndedSessionsTo(ctx, 5, &warning)
	require.Empty(t, warning.String())
	require.Error(t, saveCapturedSyncRemote(ctx, "upstream"))
	require.Error(t, deleteRedactCache(ctx))
	_, err = loadShallowHashes(ctx, dir)
	require.Error(t, err)
	require.NoError(t, os.Remove(commonFile))
	created, err := StoreAgentTypeHint(ctx, "broken-session", agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	require.True(t, created, "repair must be visible without clearing a metadata cache")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = stateLockForSession(canceled, "canceled-session")
	require.ErrorIs(t, err, context.Canceled)
	_, err = loadShallowHashes(canceled, dir)
	require.ErrorIs(t, err, context.Canceled)
}

func TestResetUsesLinkedWorktreeMetadata(t *testing.T) {
	main := setupGitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked", linked)
	metadata, err := gitrepo.ResolveWorktreeMetadata(linked)
	require.NoError(t, err)
	base := testutil.GetHeadHash(t, main)
	mainShadow := getShadowBranchNameForCommit(base, "")
	linkedShadow := getShadowBranchNameForCommit(base, metadata.WorktreeID)
	testutil.RunGit(t, main, "branch", mainShadow)
	testutil.RunGit(t, main, "branch", linkedShadow)
	t.Chdir(linked)
	clearSessionMatchCaches()
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(t.Context(), "reset-session", agent.AgentTypeClaudeCode, "", "", ""))
	state, err := LoadSessionState(t.Context(), "reset-session")
	require.NoError(t, err)
	require.NotNil(t, state)
	require.Equal(t, metadata.WorktreeID, state.WorktreeID)
	var output, warning bytes.Buffer
	require.NoError(t, s.Reset(t.Context(), &output, &warning))
	require.Empty(t, warning.String())
	require.Contains(t, output.String(), "Deleted shadow branch "+linkedShadow)
	require.NoError(t, branchExistsCLI(t.Context(), mainShadow))
	require.Error(t, branchExistsCLI(t.Context(), linkedShadow))
	state, err = LoadSessionState(t.Context(), "reset-session")
	require.NoError(t, err)
	require.Nil(t, state)
}

func TestStrategyExplicitQueriesIgnoreInheritedRepositorySelectors(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	target := setupGitRepo(t)
	foreign := setupGitRepo(t)
	testutil.WriteFile(t, target, "target.txt", "target only\n")
	testutil.GitAdd(t, target, "target.txt")
	testutil.GitCommit(t, target, "target commit")
	head := testutil.GetHeadHash(t, target)
	parent := strings.TrimSpace(testutil.RunGit(t, target, "rev-parse", "HEAD^"))
	testutil.RunGit(t, target, "config", "core.hooksPath", "target-hooks")
	testutil.RunGit(t, foreign, "config", "core.hooksPath", "foreign-hooks")
	for key, value := range map[string]string{
		"GIT_DIR":        filepath.Join(foreign, ".git"),
		"GIT_WORK_TREE":  foreign,
		"GIT_INDEX_FILE": filepath.Join(foreign, ".git", "index"),
	} {
		t.Setenv(key, value)
	}
	ctx := t.Context()
	hooks, err := getHooksDirInPath(ctx, target)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(target, "target-hooks"), hooks)
	gitDir, err := getGitDirInPath(ctx, target)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(target, ".git"), gitDir)
	common, err := gitCommonDirForWorktree(ctx, target)
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(filepath.Join(target, ".git"))
	require.NoError(t, err)
	require.Equal(t, physical, common)
	disconnected, err := isDisconnected(ctx, target, head, parent)
	require.NoError(t, err)
	require.False(t, disconnected)
	base, err := getMergeBase(ctx, target, head, parent)
	require.NoError(t, err)
	require.Equal(t, plumbing.NewHash(parent), base)
	require.NoError(t, os.WriteFile(filepath.Join(target, ".git", "shallow"), []byte(head+"\n"), 0o600))
	hashes, err := loadShallowHashes(ctx, target)
	require.NoError(t, err)
	require.Equal(t, map[plumbing.Hash]bool{plumbing.NewHash(head): true}, hashes)
}

func TestStrategyStorageRefusesSymlinkedSessionDirectory(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	target := filepath.Join(dir, ".git", "redirected")
	require.NoError(t, os.Mkdir(target, 0o750))
	require.NoError(t, os.Symlink("redirected", filepath.Join(dir, ".git", session.SessionStateDirName)))
	_, err := openSessionStateRoot(t.Context())
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	_, err = openSessionStateRootForRead(t.Context())
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
}
