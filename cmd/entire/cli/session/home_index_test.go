package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateHomeIndex points the per-user cache at a fresh temp dir and returns
// the index directory. Not parallel: t.Setenv.
func isolateHomeIndex(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	return filepath.Join(cache, "entire", homeIndexDirName)
}

func TestSessionHome_RecordAndList(t *testing.T) {
	isolateHomeIndex(t)
	wt := t.TempDir()
	owner := &proclive.Identity{PID: 42, Start: "s", Host: "h"}
	require.NoError(t, RecordSessionHome(SessionHome{SessionID: "sess-1", WorktreePath: wt, Owner: owner}))

	homes, err := ListSessionHomes(time.Hour)
	require.NoError(t, err)
	require.Len(t, homes, 1)
	assert.Equal(t, "sess-1", homes[0].SessionID)
	assert.Equal(t, wt, homes[0].WorktreePath)
	assert.Equal(t, owner, homes[0].Owner)
	assert.WithinDuration(t, time.Now(), homes[0].UpdatedAt, time.Minute)
}

func TestSessionHome_RecordRejectsInvalid(t *testing.T) {
	isolateHomeIndex(t)
	require.Error(t, RecordSessionHome(SessionHome{SessionID: "../escape", WorktreePath: t.TempDir()}))
	require.Error(t, RecordSessionHome(SessionHome{SessionID: "sess-1", WorktreePath: "relative/path"}))
}

func TestSessionHome_ListSkipsOldAndRemovesInvalid(t *testing.T) {
	dir := isolateHomeIndex(t)
	wt := t.TempDir()
	require.NoError(t, RecordSessionHome(SessionHome{SessionID: "sess-live", WorktreePath: wt}))
	write := func(name string, h SessionHome) {
		data, err := json.Marshal(h)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	write("sess-old.json", SessionHome{SessionID: "sess-old", WorktreePath: wt, UpdatedAt: time.Now().Add(-2 * time.Hour)})
	write("sess-gone.json", SessionHome{SessionID: "sess-gone", WorktreePath: wt, UpdatedAt: time.Now().Add(-homePruneAge - time.Hour)})
	write("sess-renamed.json", SessionHome{SessionID: "sess-other", WorktreePath: wt, UpdatedAt: time.Now()})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sess-corrupt.json"), []byte("{"), 0o600))

	homes, err := ListSessionHomes(time.Hour)
	require.NoError(t, err)
	require.Len(t, homes, 1)
	assert.Equal(t, "sess-live", homes[0].SessionID)

	for name, kept := range map[string]bool{
		"sess-old.json": true, "sess-gone.json": false, "sess-renamed.json": false, "sess-corrupt.json": false,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		assert.Equal(t, kept, err == nil, name)
	}
}

func TestSessionHome_SymlinkedDirRefused(t *testing.T) {
	dir := isolateHomeIndex(t)
	target := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(dir), 0o700))
	require.NoError(t, os.Symlink(target, dir))

	require.Error(t, RecordSessionHome(SessionHome{SessionID: "sess-1", WorktreePath: t.TempDir()}))
	_, err := ListSessionHomes(time.Hour)
	require.Error(t, err)
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
