package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A copy of the baseline that cannot be read must not end the search: a later
// worktree may hold the real one.
func TestBaselineSearch_SkipsAnUnreadableCopy(t *testing.T) {
	setupTestRepo(t)
	const name = "pre-task-toolu_unreadable.json"
	// A directory where the file should be: present, but not readable as one.
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "tmp", name), 0o750))
	other := t.TempDir()
	testutil.InitRepo(t, other)
	require.NoError(t, os.MkdirAll(filepath.Join(other, ".entire", "tmp"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(other, ".entire", "tmp", name), []byte(`{"tool_use_id":"toolu_unreadable"}`), 0o600))

	data, from, err := baselineSearch{"", other}.read(context.Background(), name)
	require.NoError(t, err)
	require.Equal(t, other, from)
	require.JSONEq(t, `{"tool_use_id":"toolu_unreadable"}`, string(data))
}

// A baseline that exists but cannot be read degrades new-file detection
// instead of leaving the caller with no baseline, which would claim every
// untracked file as the task's.
func TestLoadPreTaskState_UnreadableBaselineDegradesDetection(t *testing.T) {
	setupTestRepo(t)
	const toolUseID = "toolu_unreadable"
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "tmp", "pre-task-"+toolUseID+".json"), 0o750))

	state, err := LoadPreTaskState(context.Background(), toolUseID)
	require.Error(t, err)
	require.True(t, state.NewFilesUndetectable(), "an unreadable baseline must disable status-based new-file detection")
}

func TestLoadPrePromptState_UnreadableBaselineDegradesDetection(t *testing.T) {
	setupTestRepo(t)
	const sessionID = "sess-unreadable"
	require.NoError(t, os.MkdirAll(filepath.Join(".entire", "tmp"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(".entire", "tmp", "pre-prompt-"+sessionID+".json"), []byte("{not json"), 0o600))

	state, err := LoadPrePromptState(context.Background(), sessionID)
	require.Error(t, err)
	require.True(t, state.NewFilesUndetectable(), "a corrupt baseline must disable status-based new-file detection")
}
