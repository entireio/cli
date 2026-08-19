package pricing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRemoteCacheFile marshals rc to the isolated remote cache path. Call
// isolateCache(t) first so it lands under a throwaway XDG_CACHE_HOME.
func writeRemoteCacheFile(t *testing.T, rc *RemoteCache) {
	t.Helper()
	dir := filepath.Dir(remoteCachePath())
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(rc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(remoteCachePath(), data, 0o644))
}

// writeRemoteCacheRaw writes raw bytes to the cache path (for corrupt-file cases).
func writeRemoteCacheRaw(t *testing.T, data []byte) {
	t.Helper()
	dir := filepath.Dir(remoteCachePath())
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(remoteCachePath(), data, 0o644))
}

// isolateCache points userdirs.Cache at a throwaway dir for the test.
func isolateCache(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func TestLoadRemoteEntries_MissingCache(t *testing.T) {
	isolateCache(t)
	assert.Nil(t, LoadRemoteEntries(context.Background()))
}

func TestLoadRemoteEntries_CorruptCache(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheRaw(t, []byte("{not json"))
	assert.Nil(t, LoadRemoteEntries(context.Background()))
}

func TestLoadRemoteEntries_NilDoc(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: time.Now()})
	assert.Nil(t, LoadRemoteEntries(context.Background()))
}

func TestLoadRemoteEntries_UnsupportedSchemaVersion(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now(),
		Doc: &fileSchema{
			SchemaVersion: 99,
			Models: []ModelRate{
				{ID: "gpt-5.5", Provider: "openai", InputPerMTok: 7, OutputPerMTok: 11},
			},
		},
	})
	assert.Nil(t, LoadRemoteEntries(context.Background()))
}

func TestLoadRemoteEntries_DropsInvalidKeepsValid(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now(),
		Doc: &fileSchema{
			SchemaVersion: 1,
			Models: []ModelRate{
				{ID: "good-model", Provider: "test", InputPerMTok: 3, OutputPerMTok: 9},
				{ID: "bad-zero-input", Provider: "test", InputPerMTok: 0, OutputPerMTok: 9},
				{ID: "", Provider: "test", InputPerMTok: 3, OutputPerMTok: 9},
			},
		},
	})
	entries := LoadRemoteEntries(context.Background())
	require.Len(t, entries, 1)
	assert.Equal(t, "good-model", entries[0].ID)
}

func TestLoadRemoteEntries_ValidDoc(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now(),
		Doc: &fileSchema{
			SchemaVersion: 1,
			Models: []ModelRate{
				{ID: "gpt-5.5", Provider: "openai", InputPerMTok: 999, OutputPerMTok: 1000},
				{ID: "new-model", Provider: "test", InputPerMTok: 1, OutputPerMTok: 2},
			},
		},
	})
	entries := LoadRemoteEntries(context.Background())
	require.Len(t, entries, 2)
	assert.Equal(t, "gpt-5.5", entries[0].ID)
	assert.InDelta(t, 999.0, entries[0].InputPerMTok, 1e-9)
}

func TestRemoteFetchedAt(t *testing.T) {
	isolateCache(t)
	assert.True(t, RemoteFetchedAt().IsZero(), "missing cache should report zero time")

	when := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: when})
	got := RemoteFetchedAt().UTC().Truncate(time.Second)
	assert.True(t, got.Equal(when), "RemoteFetchedAt = %v, want %v", got, when)
}
