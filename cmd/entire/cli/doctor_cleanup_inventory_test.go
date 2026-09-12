package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestCanDeleteShadowBranch_RejectsIncompleteInventory(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	state := &session.State{SessionID: "protected", BaseCommit: "abc123456789", StartedAt: time.Now(), StepCount: 1}
	require.NoError(t, strategy.SaveSessionState(t.Context(), state))
	name := checkpoint.ShadowBranchNameForCommit(state.BaseCommit, "")
	file := filepath.Join(dir, ".git", session.SessionStateDirName, "protected.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"session_id":`), 0o600))
	allowed, err := strategy.CanDeleteShadowBranch(t.Context(), name, "other")
	require.Error(t, err)
	require.False(t, allowed)
	require.NoError(t, strategy.SaveSessionState(t.Context(), state))
	allowed, err = strategy.CanDeleteShadowBranch(t.Context(), name, "other")
	require.NoError(t, err)
	require.False(t, allowed)
}
