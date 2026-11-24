package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
	"github.com/stretchr/testify/require"
)

func TestProjectAgentJournalRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := osroot.Shared(dir)
	require.NoError(t, err)
	t.Cleanup(func() { osroot.Forget(dir) })
	j := &projectAgentJournal{root: root, name: "pending.json"}
	s, err := j.load()
	require.NoError(t, err)
	require.Nil(t, s)
	pending := projectAgentSubmission{RunID: agentServeRunID, Action: "complete", Body: json.RawMessage(`{"result":{"findings":[]}}`)}
	require.NoError(t, j.save(&pending))
	// A different instance represents a restarted worker.
	restarted := &projectAgentJournal{root: root, name: j.name}
	s, err = restarted.load()
	require.NoError(t, err)
	require.Equal(t, pending, *s)
	info, err := root.Stat(j.name)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0o077)
	require.NoError(t, restarted.save(nil))
	s, err = j.load()
	require.NoError(t, err)
	require.Nil(t, s)
	require.NoError(t, j.save(nil))
}

func TestProjectAgentJournalRefusesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := osroot.Shared(dir)
	require.NoError(t, err)
	t.Cleanup(func() { osroot.Forget(dir) })
	require.NoError(t, osroot.WriteFile(root, "other", []byte("sensitive"), 0o600))
	if err := root.Symlink("other", "pending.json"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	j := &projectAgentJournal{root: root, name: "pending.json"}
	_, err = j.load()
	require.Error(t, err)
	b, err := osroot.ReadFileNoFollow(root, "other")
	require.NoError(t, err)
	require.Equal(t, "sensitive", string(b))
}

func TestProjectAgentJournalRefusesCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := osroot.Shared(dir)
	require.NoError(t, err)
	t.Cleanup(func() { osroot.Forget(dir) })
	require.NoError(t, osroot.WriteFile(root, "pending.json", []byte("{broken"), 0o600))
	j := &projectAgentJournal{root: root, name: "pending.json"}
	_, err = j.load()
	require.ErrorContains(t, err, "decode pending result")
	_, err = root.Stat("pending.json")
	require.False(t, os.IsNotExist(err))
}

func TestProjectAgentJournalSweepsOnlyItsWorkspaces(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root, err := userdirs.CacheRoot()
	require.NoError(t, err)
	for _, dir := range []string{"agent-work/mine/stale/repo", "agent-work/other/active/repo"} {
		require.NoError(t, osroot.MkdirAllNoSymlink(root, dir, 0o700))
	}
	j := &projectAgentJournal{workspace: "agent-work/mine"}
	require.NoError(t, j.sweepWorkspaces())
	require.NoError(t, j.sweepWorkspaces(), "sweeping an absent workspace is not an error")
	_, err = root.Stat("agent-work/mine")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = root.Stat("agent-work/other/active/repo")
	require.NoError(t, err)
}
