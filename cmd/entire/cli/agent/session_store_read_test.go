package agent_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionStoreOpenRead(t *testing.T) {
	t.Parallel()
	store, dir := newStore(t, joinResolve)
	require.NoError(t, store.WriteFile("session.jsonl", []byte("first\nsecond\n"), 0o600))
	file, err := store.OpenRead("session.jsonl")
	require.NoError(t, err)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 6))
	require.NoError(t, err)
	require.Equal(t, "first\n", string(data))
	for _, name := range []string{"../outside.jsonl", "."} {
		f, err := store.OpenRead(name)
		require.Error(t, err)
		require.Nil(t, f)
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o700))
	f, err := store.OpenRead("nested")
	require.Error(t, err)
	require.Nil(t, f)
	if err := os.Symlink("session.jsonl", filepath.Join(dir, "link.jsonl")); err != nil {
		t.Skip("symlinks unavailable")
	}
	f, err = store.OpenRead("link.jsonl")
	require.Error(t, err)
	require.Nil(t, f)
}
