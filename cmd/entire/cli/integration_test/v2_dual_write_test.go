//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestV2DualWrite_FullWorkflow verifies that when checkpoints_v2 is enabled,
// a full session workflow (prompt → stop → commit) writes checkpoint data
// to both v1 and v2 refs.
func TestV2DualWrite_FullWorkflow(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-test")

	// Initialize with checkpoints_v2 enabled
	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
	})

	// Start session
	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add greeting function")
	require.NoError(t, err)

	// Create a file and transcript
	env.WriteFile("greet.go", "package main\n\nfunc Greet() string { return \"hello\" }")
	session.CreateTranscript(
		"Add greeting function",
		[]FileChange{{Path: "greet.go", Content: "package main\n\nfunc Greet() string { return \"hello\" }"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	// User commits (triggers prepare-commit-msg + post-commit → condensation)
	env.GitCommitWithShadowHooks("Add greeting function", "greet.go")

	// Get checkpoint ID from commit trailer
	cpIDStr := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpIDStr, "checkpoint ID should be in commit trailer")

	cpID, err := id.NewCheckpointID(cpIDStr)
	require.NoError(t, err)
	cpPath := cpID.Path()

	// ========================================
	// Verify v1 branch (existing behavior)
	// ========================================
	assert.True(t, env.BranchExists(paths.MetadataBranchName),
		"v1 metadata branch should exist")

	v1Summary, found := env.ReadFileFromBranch(paths.MetadataBranchName, cpPath+"/"+paths.MetadataFileName)
	require.True(t, found, "v1 root metadata.json should exist")
	assert.Contains(t, v1Summary, cpIDStr)

	// ========================================
	// Verify v2 /main ref
	// ========================================
	assert.True(t, env.RefExists(paths.V2MainRefName),
		"v2 /main ref should exist")

	// Root CheckpointSummary
	mainSummary, found := env.ReadFileFromRef(paths.V2MainRefName, cpPath+"/"+paths.MetadataFileName)
	require.True(t, found, "v2 /main root metadata.json should exist")

	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(mainSummary), &summary))
	assert.Equal(t, cpID, summary.CheckpointID)
	assert.Len(t, summary.Sessions, 1)

	// Session metadata
	mainSessionMeta, found := env.ReadFileFromRef(paths.V2MainRefName, cpPath+"/0/"+paths.MetadataFileName)
	require.True(t, found, "v2 /main session metadata.json should exist")
	assert.Contains(t, mainSessionMeta, session.ID)

	// Prompts
	mainPrompts, found := env.ReadFileFromRef(paths.V2MainRefName, cpPath+"/0/"+paths.PromptFileName)
	require.True(t, found, "v2 /main prompt.txt should exist")
	assert.Contains(t, mainPrompts, "Add greeting function")

	// Transcript should NOT be on /main
	_, found = env.ReadFileFromRef(paths.V2MainRefName, cpPath+"/0/"+paths.TranscriptFileName)
	assert.False(t, found, "full.jsonl should NOT be on v2 /main")

	// ========================================
	// Verify v2 /full/current ref
	// ========================================
	assert.True(t, env.RefExists(paths.V2FullCurrentRefName),
		"v2 /full/current ref should exist")

	// Transcript should be on /full/current
	fullTranscript, found := env.ReadFileFromRef(paths.V2FullCurrentRefName, cpPath+"/0/"+paths.TranscriptFileName)
	require.True(t, found, "full.jsonl should exist on v2 /full/current")
	assert.Contains(t, fullTranscript, "Greet")

	// Content hash should be co-located with transcript
	fullHash, found := env.ReadFileFromRef(paths.V2FullCurrentRefName, cpPath+"/0/"+paths.ContentHashFileName)
	require.True(t, found, "content_hash.txt should exist on v2 /full/current")
	assert.True(t, strings.HasPrefix(fullHash, "sha256:"))

	// Metadata should NOT be on /full/current
	_, found = env.ReadFileFromRef(paths.V2FullCurrentRefName, cpPath+"/0/"+paths.MetadataFileName)
	assert.False(t, found, "metadata.json should NOT be on v2 /full/current")
}

// TestV2DualWrite_Disabled verifies that when checkpoints_v2 is NOT enabled,
// no v2 refs are created.
func TestV2DualWrite_Disabled(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-disabled")

	// Initialize WITHOUT checkpoints_v2
	env.InitEntire()

	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add helper")
	require.NoError(t, err)

	env.WriteFile("helper.go", "package main\n\nfunc Helper() {}")
	session.CreateTranscript(
		"Add helper",
		[]FileChange{{Path: "helper.go", Content: "package main\n\nfunc Helper() {}"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	env.GitCommitWithShadowHooks("Add helper", "helper.go")

	// v1 should exist
	assert.True(t, env.BranchExists(paths.MetadataBranchName),
		"v1 metadata branch should exist")

	// v2 refs should NOT exist
	assert.False(t, env.RefExists(paths.V2MainRefName),
		"v2 /main ref should NOT exist when v2 is disabled")
	assert.False(t, env.RefExists(paths.V2FullCurrentRefName),
		"v2 /full/current ref should NOT exist when v2 is disabled")
}

// TestV2DualWrite_StopTimeFinalization verifies that stop-time transcript
// finalization also updates v2 refs when checkpoints_v2 is enabled.
func TestV2DualWrite_StopTimeFinalization(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.WriteFile(".gitignore", ".entire/\n")
	env.GitAdd("README.md")
	env.GitAdd(".gitignore")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/v2-finalize")

	env.InitEntireWithOptions(map[string]any{
		"checkpoints_v2": true,
	})

	// Start session and create first checkpoint
	session := env.NewSession()
	err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create main file")
	require.NoError(t, err)

	env.WriteFile("main.go", "package main\n\nfunc main() {}")
	session.CreateTranscript(
		"Create main file",
		[]FileChange{{Path: "main.go", Content: "package main\n\nfunc main() {}"}},
	)
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	// Mid-session commit (checkpoint condensed, but transcript is provisional)
	env.GitCommitWithShadowHooks("Add main.go", "main.go")

	cpIDStr := env.GetLatestCheckpointIDFromHistory()
	require.NotEmpty(t, cpIDStr)

	cpID, err := id.NewCheckpointID(cpIDStr)
	require.NoError(t, err)
	cpPath := cpID.Path()

	// Continue session with more work
	err = env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add tests")
	require.NoError(t, err)

	env.WriteFile("main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}")
	// Rebuild transcript with both turns (CreateTranscript replaces)
	session.CreateTranscript(
		"Add tests",
		[]FileChange{
			{Path: "main.go", Content: "package main\n\nfunc main() {}"},
			{Path: "main_test.go", Content: "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}"},
		},
	)

	// Stop finalizes the transcript for all turn checkpoints
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	require.NoError(t, err)

	// After stop-time finalization, /full/current should have the finalized transcript
	fullTranscript, found := env.ReadFileFromRef(paths.V2FullCurrentRefName, cpPath+"/0/"+paths.TranscriptFileName)
	require.True(t, found, "full.jsonl should exist on /full/current after finalization")
	assert.Contains(t, fullTranscript, "main")
}
