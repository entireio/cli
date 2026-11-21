//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestLooseObjectsMaintenancePack_FilesTouchedStayAccurate covers repositories
// where `git maintenance run --task=loose-objects` moved objects into
// objects/pack/loose-<hash>.pack. Git reads every pack in that directory, so
// the tree is clean; a reader that only accepts pack-<hash> names cannot load
// the subdirectory's tree and reports every file under it as modified.
func TestLooseObjectsMaintenancePack_FilesTouchedStayAccurate(t *testing.T) {
	t.Parallel()
	if !gitVersionAtLeast(t, 2, 30) {
		t.Skip("git maintenance's loose-objects task needs git 2.30 or later")
	}

	env := NewFeatureBranchEnv(t)
	env.WriteFile("src/a.go", "package src\n\nfunc A() {}\n")
	env.WriteFile("src/b.go", "package src\n\nfunc B() {}\n")
	env.WriteFile("src/nested/c.go", "package nested\n\nfunc C() {}\n")
	env.GitAdd("src/a.go", "src/b.go", "src/nested/c.go")
	env.GitCommit("Add src")
	srcTree := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", "HEAD:src"))

	// The first run packs the loose objects but keeps them; the second deletes
	// the loose copies that are now in a pack.
	testutil.RunGit(t, env.RepoDir, "maintenance", "run", "--task=loose-objects")
	testutil.RunGit(t, env.RepoDir, "maintenance", "run", "--task=loose-objects")
	looseCopy := filepath.Join(env.RepoDir, ".git", "objects", srcTree[:2], srcTree[2:])
	_, err := os.Stat(looseCopy)
	require.ErrorIs(t, err, os.ErrNotExist, "src tree %s must exist only in a pack", srcTree)
	looseNamedPacks, err := filepath.Glob(filepath.Join(env.RepoDir, ".git", "objects", "pack", "loose-*.pack"))
	require.NoError(t, err)
	require.NotEmpty(t, looseNamedPacks, "git maintenance should write a loose-<hash>.pack")

	// Commit with the git CLI, which writes only the changed root tree, so the
	// src tree stays in the loose-<hash> pack alone. Staging through go-git
	// would read that pack itself and fail before any hook runs.
	env.WriteFile("README.md", "# Test Repository\n\nUpdated.\n")
	testutil.RunGit(t, env.RepoDir, "add", "README.md")
	testutil.RunGit(t, env.RepoDir, "commit", "-m", "Update README")

	readOnly := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmit(readOnly.ID))
	readOnly.TranscriptBuilder.AddUserMessage("Summarize src")
	readOnly.TranscriptBuilder.AddAssistantMessage("src defines A, B and nested.C.")
	require.NoError(t, readOnly.TranscriptBuilder.WriteToFile(readOnly.TranscriptPath))
	require.NoError(t, env.SimulateStop(readOnly.ID, readOnly.TranscriptPath))
	roState, err := env.GetSessionState(readOnly.ID)
	require.NoError(t, err)
	require.Empty(t, roState.FilesTouched, "a read-only turn must not touch files")

	coding := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(coding.ID, coding.TranscriptPath))
	env.WriteFile("NOTES.md", "notes\n")
	coding.CreateTranscript("Add notes", []FileChange{{Path: "NOTES.md", Content: "notes\n"}})
	require.NoError(t, env.SimulateStop(coding.ID, coding.TranscriptPath))
	env.GitCommitWithHooks("Add notes", "NOTES.md")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, cpID, "commit should carry an Entire-Checkpoint trailer")
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpID))
	require.True(t, found, "checkpoint summary for %s", cpID)
	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(summaryContent), &summary))
	require.Equal(t, []string{"NOTES.md"}, summary.FilesTouched)
	env.AssertCheckpointExcludesSession(t, summary, readOnly.ID)
}
