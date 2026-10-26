package strategy

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitializeSession_RecordsSessionHome(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "init.txt", "init\n")
	testutil.GitAdd(t, dir, "init.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	ctx := t.Context()
	s := &ManualCommitStrategy{}
	const sessionID = "record-session-home"

	// New session, then a resumed turn: both turn-start paths record.
	for range 2 {
		require.NoError(t, session.RecordSessionHome(session.SessionHome{SessionID: sessionID, WorktreePath: t.TempDir()}))
		require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "prompt", ""))
		state, err := s.loadSessionState(ctx, sessionID)
		require.NoError(t, err)
		require.NotNil(t, state)

		homes, err := session.ListSessionHomes(time.Hour)
		require.NoError(t, err)
		require.Len(t, homes, 1)
		assert.Equal(t, state.WorktreePath, homes[0].WorktreePath)
		assert.Equal(t, state.Owner, homes[0].Owner)
	}
}
