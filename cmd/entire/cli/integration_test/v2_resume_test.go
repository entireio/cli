//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestV2Resume_SwitchBranchWithSession verifies that resume works when
// checkpoints_v2 is enabled. The session transcript should be read from
// v2 refs (/main for metadata, /full/* for raw transcript).
func TestV2Resume_SwitchBranchWithSession(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repo with feature branch
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-resume-test")

	// Initialize with checkpoints_v2 enabled
	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
	})

	// Create a session
	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create hello script")
	require.NoError(t, err)

	content := "puts 'Hello from v2 session'"
	env.WriteFile("hello.rb", content)

	session.CreateTranscript(
		"Create hello script",
		[]FileChange{{Path: "hello.rb", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	// Commit (triggers dual-write to v1 + v2)
	env.GitCommitWithShadowHooks("Create hello script", "hello.rb")

	featureBranch := env.GetCurrentBranch()

	// Switch to master
	env.GitCheckoutBranch("master")
	assert.Equal(t, "master", env.GetCurrentBranch())

	// Run resume to switch back to feature branch
	output, err := env.RunResume(featureBranch)
	require.NoError(t, err, "resume failed: %s", output)

	// Verify we switched back
	assert.Equal(t, featureBranch, env.GetCurrentBranch())

	// Verify output contains session info and resume command
	assert.Contains(t, output, "Restored session", "output should contain 'Restored session'")
	assert.Contains(t, output, "claude -r", "output should contain resume command")

	// Verify transcript was restored
	transcriptFiles, err := filepath.Glob(filepath.Join(env.ClaudeProjectDir, "*.jsonl"))
	require.NoError(t, err)
	assert.NotEmpty(t, transcriptFiles, "transcript should be restored to Claude project dir")
}

// TestV2Resume_AlreadyOnBranch verifies that resume works on the current branch
// when checkpoints_v2 is enabled.
func TestV2Resume_AlreadyOnBranch(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-resume-same-branch")

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
	})

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create test file")
	require.NoError(t, err)

	content := "console.log('test')"
	env.WriteFile("test.js", content)

	session.CreateTranscript(
		"Create test file",
		[]FileChange{{Path: "test.js", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create test file", "test.js")

	currentBranch := env.GetCurrentBranch()

	// Run resume on the branch we're already on
	output, err := env.RunResume(currentBranch)
	require.NoError(t, err, "resume failed: %s", output)

	assert.Contains(t, output, "Restored session")
	assert.Contains(t, output, "claude -r")
}

// TestV2Resume_FallsBackToV1 verifies that when checkpoints_v2 is enabled but
// checkpoint data only exists on v1, resume falls back to reading from v1.
func TestV2Resume_FallsBackToV1(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-fallback-test")

	// First commit WITHOUT v2 (v1 only)
	env.InitEntire()

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create initial file")
	require.NoError(t, err)

	content := "# v1 only content"
	env.WriteFile("v1file.md", content)

	session.CreateTranscript(
		"Create initial file",
		[]FileChange{{Path: "v1file.md", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create initial file", "v1file.md")

	// Now enable v2 in settings (simulates upgrade)
	settingsPath := filepath.Join(env.RepoDir, ".entire", paths.SettingsFileName)
	settingsData, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(settingsData, &settings))
	stratOpts, _ := settings["strategy_options"].(map[string]any)
	if stratOpts == nil {
		stratOpts = make(map[string]any)
	}
	stratOpts["checkpoints_v2"] = true
	settings["strategy_options"] = stratOpts
	updatedData, err := json.MarshalIndent(settings, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, updatedData, 0o644))

	featureBranch := env.GetCurrentBranch()
	env.GitCheckoutBranch("master")

	// Resume should fall back to v1 since data was written before v2 was enabled
	output, err := env.RunResume(featureBranch)
	require.NoError(t, err, "resume failed: %s", output)

	assert.Equal(t, featureBranch, env.GetCurrentBranch())
	assert.Contains(t, output, "Restored session")
	assert.Contains(t, output, "claude -r")
}

// TestCheckpointsVersion2Resume_SwitchBranchWithSession verifies that resume
// works in strict v2-only mode. The session should be restorable from v2 refs
// even though no v1 committed metadata was written.
func TestCheckpointsVersion2Resume_SwitchBranchWithSession(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-only-resume-test")

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_version": 2,
	})

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create hello script in v2 only mode")
	require.NoError(t, err)

	content := "puts 'Hello from v2-only session'"
	env.WriteFile("hello_v2_only.rb", content)

	session.CreateTranscript(
		"Create hello script in v2 only mode",
		[]FileChange{{Path: "hello_v2_only.rb", Content: content}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Create hello script in v2 only mode", "hello_v2_only.rb")

	cpIDStr := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpIDStr, "checkpoint ID should be in commit trailer")

	cpID, err := id.NewCheckpointID(cpIDStr)
	require.NoError(t, err)
	_, found := env.ReadFileFromBranch(paths.MetadataBranchName, cpID.Path()+"/"+paths.MetadataFileName)
	assert.False(t, found, "v1 committed checkpoint metadata should not exist when checkpoints_version is 2")

	featureBranch := env.GetCurrentBranch()
	env.GitCheckoutBranch("master")
	assert.Equal(t, "master", env.GetCurrentBranch())

	output, err := env.RunResume(featureBranch)
	require.NoError(t, err, "resume failed: %s", output)

	assert.Equal(t, featureBranch, env.GetCurrentBranch())
	assert.Contains(t, output, "Restored session", "output should contain 'Restored session'")
	assert.Contains(t, output, "claude -r", "output should contain resume command")

	transcriptFiles, err := filepath.Glob(filepath.Join(env.ClaudeProjectDir, "*.jsonl"))
	require.NoError(t, err)
	assert.NotEmpty(t, transcriptFiles, "transcript should be restored to Claude project dir")
}
