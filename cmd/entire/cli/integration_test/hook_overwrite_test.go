//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookOverwrite_MidTurnWipe_NextPromptRecovers simulates the scenario from
// https://github.com/entireio/cli/issues/784:
//
// Flow:
//  1. Prompt 1: agent creates files, commits via hooks → checkpoint trailer ✓
//  2. Mid-turn: third-party tool (husky/lefthook) overwrites git hooks
//  3. Agent commits again (no hooks fire) → NO trailer
//  4. Prompt 2 starts (user-prompt-submit) → EnsureSetup reinstalls hooks
//  5. Agent commits via hooks → checkpoint trailer ✓ (hooks restored)
//
// The key insight: GitCommitWithShadowHooks invokes the binary directly (simulating
// working hooks), while GitAdd+GitCommit uses go-git without hooks (simulating
// overwritten hooks where `entire` is never called).
func TestHookOverwrite_MidTurnWipe_NextPromptRecovers(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	hooksDir := filepath.Join(env.RepoDir, ".git", "hooks")

	sess := env.NewSession()

	// === Prompt 1: normal flow, hooks work ===
	err := env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Create files A and B", sess.TranscriptPath)
	require.NoError(t, err)

	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")

	sess.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
	})

	// First commit — hooks are intact, binary is invoked → trailer added
	env.GitCommitWithShadowHooks("Add file A", "fileA.go")
	cpID1 := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID1, "first commit should have checkpoint trailer")

	// === Simulate husky/lefthook overwriting hooks mid-turn ===
	// This is what happens when an agent runs `npm install` and husky's
	// `prepare` lifecycle script reinstalls its own hooks.
	for _, hookName := range strategy.ManagedGitHookNames() {
		hookPath := filepath.Join(hooksDir, hookName)
		huskyContent := "#!/bin/sh\n# husky - do not edit\n. \"$(dirname \"$0\")/_/husky.sh\"\n"
		err := os.WriteFile(hookPath, []byte(huskyContent), 0o755)
		require.NoError(t, err)
	}

	// Verify hooks are overwritten
	require.False(t, strategy.IsGitHookInstalledInDir(t.Context(), env.RepoDir),
		"hooks should be detected as overwritten")

	// Second commit — hooks are gone, use plain go-git commit (no binary invoked).
	// This simulates the real-world situation after husky/lefthook has overwritten
	// our hooks: a commit is made where git would run a third-party hook that does
	// not call `entire`, so from Entire's perspective no hooks run and no trailer
	// is added.
	env.GitAdd("fileB.go")
	env.GitCommit("Add file B")
	cpID2 := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	assert.Empty(t, cpID2,
		"second commit should NOT have trailer (hooks were overwritten, entire never called)")

	// End prompt 1
	err = env.SimulateStop(sess.ID, sess.TranscriptPath)
	require.NoError(t, err)

	// === Prompt 2: same session, next turn — EnsureSetup should reinstall hooks ===

	env.WriteFile("fileC.go", "package main\n\nfunc C() {}\n")

	sess.CreateTranscript("Create file C", []FileChange{
		{Path: "fileC.go", Content: "package main\n\nfunc C() {}\n"},
	})

	err = env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(
		sess.ID, "Create file C", sess.TranscriptPath)
	require.NoError(t, err)

	// Verify hooks were reinstalled by EnsureSetup
	require.True(t, strategy.IsGitHookInstalledInDir(t.Context(), env.RepoDir),
		"hooks should be reinstalled by EnsureSetup at prompt 2 start")

	// Verify overwritten hooks were backed up (chaining preserved)
	for _, hookName := range strategy.ManagedGitHookNames() {
		backupPath := filepath.Join(hooksDir, hookName+".pre-entire")
		_, err := os.Stat(backupPath)
		assert.NoError(t, err, "backup %s.pre-entire should exist after reinstall", hookName)
	}

	// Third commit — hooks restored, agent commits (no TTY) → trailer added via fast path
	env.GitCommitWithShadowHooksAsAgent("Add file C", "fileC.go")
	cpID3 := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	assert.NotEmpty(t, cpID3,
		"third commit should have trailer (hooks reinstalled by prompt 2)")

	// Checkpoint IDs should be distinct
	if cpID3 != "" {
		assert.NotEqual(t, cpID1, cpID3,
			"checkpoint IDs from prompt 1 and prompt 2 should be distinct")
	}
}
