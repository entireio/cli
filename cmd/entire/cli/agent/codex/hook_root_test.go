package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestResolveHookDiscovery_NormalCheckout(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "repo")
	initCommittedRepo(t, root)

	discovery := resolveHookDiscovery(root)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, root), discovery.DiscoveredHooks.Path())
	require.NoError(t, discovery.Diagnostic)
}

func TestResolveHookDiscovery_ConventionalLinkedWorktree(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)

	discovery := resolveHookDiscovery(linkedRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, mainRoot), discovery.DiscoveredHooks.Path())
	require.Equal(t, canonicalPathForTest(t, linkedRoot), discovery.worktreeRoot)
}

func TestResolveHookDiscovery_OrdinarySubmodule(t *testing.T) {
	t.Parallel()
	ordinaryRoot, _ := setupSubmoduleWorktrees(t)

	discovery := resolveHookDiscovery(ordinaryRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, ordinaryRoot), discovery.DiscoveredHooks.Path())
}

func TestResolveHookDiscovery_SeparateGitDirectory(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	storageRoot := filepath.Join(tmp, "git-storage")
	runGitWithDir(t, tmp, "init", "--separate-git-dir", storageRoot, mainRoot)

	discovery := resolveHookDiscovery(mainRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, mainRoot), discovery.DiscoveredHooks.Path())
}

func TestResolveHookDiscovery_SupportedFallbackLayouts(t *testing.T) {
	t.Parallel()

	t.Run("bare worktrees", func(t *testing.T) {
		t.Parallel()
		featureRoot := setupBareWorktreeLayout(t)

		discovery := resolveHookDiscovery(featureRoot)
		require.Equal(t, HookDiscoveryResolved, discovery.State)
		require.Equal(t, canonicalHooksPath(t, filepath.Dir(featureRoot)), discovery.DiscoveredHooks.Path())
	})

	t.Run("pointerless bare container falls back to current worktree", func(t *testing.T) {
		t.Parallel()
		featureRoot := setupPointerlessBareWorktreeLayout(t)

		discovery := resolveHookDiscovery(featureRoot)
		require.Equal(t, HookDiscoveryResolved, discovery.State)
		require.Equal(t, canonicalHooksPath(t, featureRoot), discovery.DiscoveredHooks.Path())
	})

	t.Run("standalone bare repository falls back to current worktree", func(t *testing.T) {
		t.Parallel()
		featureRoot := setupStandaloneBareRepositoryWorktree(t)

		discovery := resolveHookDiscovery(featureRoot)
		require.Equal(t, HookDiscoveryResolved, discovery.State)
		require.Equal(t, canonicalHooksPath(t, featureRoot), discovery.DiscoveredHooks.Path())
	})

	t.Run("linked submodule", func(t *testing.T) {
		t.Parallel()
		_, linkedRoot := setupSubmoduleWorktrees(t)

		discovery := resolveHookDiscovery(linkedRoot)
		require.Equal(t, HookDiscoveryResolved, discovery.State)
		require.Equal(t, canonicalHooksPath(t, linkedRoot), discovery.DiscoveredHooks.Path())
	})
}

func TestResolveHookDiscovery_ContradictoryMetadataIsUnresolved(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	movedRoot := filepath.Join(tmp, "moved")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)
	require.NoError(t, os.Rename(linkedRoot, movedRoot))

	discovery := resolveHookDiscovery(movedRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, movedRoot), discovery.DiscoveredHooks.Path())
}

func TestResolveHookDiscovery_RefusesUserWideRoot(t *testing.T) {
	fakeHome := t.TempDir()
	initCommittedRepo(t, fakeHome)
	t.Setenv("HOME", fakeHome)
	t.Setenv("CODEX_HOME", "")

	discovery := resolveHookDiscovery(fakeHome)
	require.Equal(t, HookDiscoveryUnresolved, discovery.State)
	var unresolved *UnresolvedHookDiscoveryError
	require.ErrorAs(t, discovery.Diagnostic, &unresolved)
	require.Contains(t, unresolved.Reason, "user-wide")
}

func setupBareWorktreeLayout(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	seedRoot := filepath.Join(tmp, "seed")
	layoutRoot := filepath.Join(tmp, "layout")
	bareRoot := filepath.Join(layoutRoot, ".bare")
	mainRoot := filepath.Join(layoutRoot, "main")
	featureRoot := filepath.Join(layoutRoot, "feature")
	initCommittedRepo(t, seedRoot)
	require.NoError(t, os.MkdirAll(layoutRoot, 0o750))
	runGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	require.NoError(t, os.WriteFile(filepath.Join(layoutRoot, ".git"), []byte("gitdir: ./.bare\n"), 0o600))
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", mainRoot)
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", featureRoot)
	return featureRoot
}

func setupPointerlessBareWorktreeLayout(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	seedRoot := filepath.Join(tmp, "seed")
	layoutRoot := filepath.Join(tmp, "layout")
	bareRoot := filepath.Join(layoutRoot, ".bare")
	featureRoot := filepath.Join(layoutRoot, "feature")
	initCommittedRepo(t, seedRoot)
	require.NoError(t, os.MkdirAll(layoutRoot, 0o750))
	runGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", featureRoot)
	return featureRoot
}

func setupStandaloneBareRepositoryWorktree(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	seedRoot := filepath.Join(tmp, "seed")
	bareRoot := filepath.Join(tmp, "repo.git")
	featureRoot := filepath.Join(tmp, "feature")
	initCommittedRepo(t, seedRoot)
	runGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", featureRoot)
	return featureRoot
}

func setupSubmoduleWorktrees(t *testing.T) (ordinarySubmoduleRoot, linkedSubmoduleRoot string) {
	t.Helper()
	tmp := t.TempDir()
	subjectRoot := filepath.Join(tmp, "subject")
	superRoot := filepath.Join(tmp, "super")
	ordinarySubmoduleRoot = filepath.Join(superRoot, "sub")
	linkedSubmoduleRoot = filepath.Join(tmp, "linked-sub")
	initCommittedRepo(t, subjectRoot)
	initCommittedRepo(t, superRoot)
	runGit(t, superRoot, "-c", "protocol.file.allow=always", "submodule", "add", subjectRoot, "sub")
	testutil.GitAdd(t, superRoot, ".gitmodules", "sub")
	testutil.GitCommit(t, superRoot, "add submodule")
	runGit(t, ordinarySubmoduleRoot, "worktree", "add", "-b", "linked", linkedSubmoduleRoot)
	return ordinarySubmoduleRoot, linkedSubmoduleRoot
}

func initCommittedRepo(t *testing.T, repoRoot string) {
	t.Helper()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")
}

func runGit(t *testing.T, repoRoot string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repoRoot}, args...)
	runGitWithDir(t, repoRoot, commandArgs...)
}

func runGitWithDir(t *testing.T, commandDir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = commandDir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func canonicalHooksPath(t *testing.T, root string) string {
	t.Helper()
	return filepath.Join(canonicalPathForTest(t, root), ".codex", HooksFileName)
}

func canonicalPathForTest(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalPath(path)
	require.NoError(t, err)
	return canonical
}
