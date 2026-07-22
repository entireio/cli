//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// huskyHScript is the core of the husky v9 `_/h` dispatcher (HUSKY=0 opt-out,
// ~/.config/husky/init.sh sourcing, and node_modules/.bin PATH prepend trimmed).
// Git runs `.husky/_/<hook>`, which sources this script, which then executes
// `.husky/<hook>` if present via `sh -e`.
const huskyHScript = `#!/usr/bin/env sh
n=$(basename "$0")
s=$(dirname "$(dirname "$0")")/$n
[ ! -f "$s" ] && exit 0
sh -e "$s" "$@"
`

func seedHuskyLayout(t *testing.T, env *TestEnv) (ownedDir, userDir string) {
	t.Helper()
	repoDir := env.RepoDir
	userDir = filepath.Join(repoDir, ".husky")
	ownedDir = filepath.Join(userDir, "_")
	require.NoError(t, os.MkdirAll(ownedDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ownedDir, "h"), []byte(huskyHScript), 0o755))

	cmd := exec.CommandContext(t.Context(), "git", "config", "core.hooksPath", ".husky/_")
	cmd.Dir = repoDir
	require.NoError(t, cmd.Run())

	// Intentional hooksPath mutation for the husky layout under test.
	configData, err := os.ReadFile(env.gitConfigPath())
	require.NoError(t, err)
	env.AcceptGitConfigChanges(string(configData))

	writeHuskyStubs(t, ownedDir)
	return ownedDir, userDir
}

func writeHuskyStubs(t *testing.T, ownedDir string) {
	t.Helper()
	stub := "#!/usr/bin/env sh\n. \"$(dirname \"$0\")/h\"\n"
	for _, hookName := range strategy.ManagedGitHookNames() {
		require.NoError(t, os.WriteFile(filepath.Join(ownedDir, hookName), []byte(stub), 0o755))
	}
}

// TestHusky_MidTurnPrepare_CommitsKeepTrailer covers the real #784 failure mode:
// husky/npm prepare regenerates `.husky/_` mid-turn. Entire must install into
// `.husky/` user hooks so mid-turn commits still get Entire-Checkpoint trailers
// without waiting for the next user-prompt-submit.
func TestHusky_MidTurnPrepare_CommitsKeepTrailer(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	ownedDir, userDir := seedHuskyLayout(t, env)

	// Point hooks at the shared test binary via absolute path (GUI/agent PATH
	// is irrelevant; this matches `entire enable --absolute-git-hook-path`).
	env.WriteSettings(map[string]any{
		"enabled":                true,
		"absolute_git_hook_path": true,
		"strategy_options":       map[string]any{"filtered_fetches": true, "commit_linking": "always"},
	})

	sess := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Create files A and B", sess.TranscriptPath)
	require.NoError(t, err)

	require.True(t, strategy.IsGitHookInstalledInDir(t.Context(), env.RepoDir),
		"EnsureSetup should install Entire into .husky/ user hooks")

	for _, hookName := range strategy.ManagedGitHookNames() {
		userHook := filepath.Join(userDir, hookName)
		data, readErr := os.ReadFile(userHook)
		require.NoError(t, readErr, "Entire hook %s should live in .husky/", hookName)
		require.Contains(t, string(data), "Entire CLI hooks")

		ownedHook, readErr := os.ReadFile(filepath.Join(ownedDir, hookName))
		require.NoError(t, readErr)
		assert.NotContains(t, string(ownedHook), "Entire CLI hooks",
			"husky-owned stub %s must remain untouched", hookName)
	}

	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")
	sess.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
	})

	// Commit 1 via real git so husky stubs → .husky/ Entire hooks run.
	env.GitAdd("fileA.go")
	commitViaGit(t, env, "Add file A")
	cpID1 := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID1, "first commit should have checkpoint trailer through husky stubs")

	// Mid-turn: husky prepare regenerates .husky/_ (issue #784).
	writeHuskyStubs(t, ownedDir)
	require.True(t, strategy.IsGitHookInstalledInDir(t.Context(), env.RepoDir),
		"Entire user hooks in .husky/ must survive husky prepare")

	// Commit 2 same turn — must still get a trailer (no next-prompt recovery).
	env.GitAdd("fileB.go")
	commitViaGit(t, env, "Add file B")
	cpID2 := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID2,
		"second mid-turn commit after husky prepare must still have Entire-Checkpoint trailer")
	assert.NotEqual(t, cpID1, cpID2, "each commit should get its own checkpoint id")
}

func commitViaGit(t *testing.T, env *TestEnv, message string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-c", "core.editor=true", "commit", "-m", message)
	cmd.Dir = env.RepoDir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_COMMITTER_EMAIL=test@example.com",
		// Agent-style: no interactive TTY for prepare-commit-msg fast path.
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git commit %q failed: %s", message, out)
}

// TestHusky_PreExistingUserHook_BackedUpAndChained covers tracked `.husky/`
// territory: a pre-existing mode-0644 user hook is renamed to `.pre-entire`,
// Entire's wrapper is installed, and the backup still runs (via sh -e) on commit.
// RemoveGitHook restores the original script.
func TestHusky_PreExistingUserHook_BackedUpAndChained(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	_, userDir := seedHuskyLayout(t, env)

	sentinel := filepath.Join(env.RepoDir, "user-hook-ran")
	custom := "#!/bin/sh\ntouch \"" + sentinel + "\"\n"
	userHook := filepath.Join(userDir, "prepare-commit-msg")
	require.NoError(t, os.WriteFile(userHook, []byte(custom), 0o644))

	env.WriteSettings(map[string]any{
		"enabled":                true,
		"absolute_git_hook_path": true,
		"strategy_options":       map[string]any{"filtered_fetches": true, "commit_linking": "always"},
	})

	sess := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Touch preexisting husky hook", sess.TranscriptPath)
	require.NoError(t, err)

	backupPath := userHook + ".pre-entire"
	backupData, err := os.ReadFile(backupPath)
	require.NoError(t, err, "preexisting user hook should be backed up to .pre-entire")
	require.Equal(t, custom, string(backupData))

	installed, err := os.ReadFile(userHook)
	require.NoError(t, err)
	require.Contains(t, string(installed), "Entire CLI hooks")
	require.Contains(t, string(installed), "sh -e")

	env.WriteFile("chain.go", "package main\n")
	sess.CreateTranscript("Touch preexisting husky hook", []FileChange{
		{Path: "chain.go", Content: "package main\n"},
	})
	env.GitAdd("chain.go")
	commitViaGit(t, env, "Exercise chained husky user hook")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID, "Entire prepare-commit-msg should still add trailer")
	_, err = os.Stat(sentinel)
	require.NoError(t, err, "mode-0644 preexisting user hook must run via chain")

	removed, err := strategy.RemoveGitHook(t.Context())
	require.NoError(t, err)
	require.Greater(t, removed, 0)
	restored, err := os.ReadFile(userHook)
	require.NoError(t, err)
	require.Equal(t, custom, string(restored))
}
