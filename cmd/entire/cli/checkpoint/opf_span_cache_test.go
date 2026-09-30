package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

var testOPFKey = strings.Repeat("ab", 32)

func TestOPFSpanCache_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cache, err := OPFSpanCacheAt(dir)
	require.NoError(t, err)

	_, ok := cache.LoadOPFSpans(testOPFKey)
	assert.False(t, ok, "a missing entry must read as not cached")

	want := map[string][]redact.Span{
		strings.Repeat("cd", 32): {{Start: 0, End: 5, Label: "private_person"}},
		strings.Repeat("ef", 32): {},
	}
	require.NoError(t, cache.StoreOPFSpans(testOPFKey, want))
	got, ok := cache.LoadOPFSpans(testOPFKey)
	require.True(t, ok)
	assert.Equal(t, want[strings.Repeat("cd", 32)], got[strings.Repeat("cd", 32)])
	_, present := got[strings.Repeat("ef", 32)]
	assert.True(t, present, "a leaf with no spans must still be recorded as scanned")

	info, err := os.Stat(filepath.Join(dir, OPFSpanCacheDirName, testOPFKey+".json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestOPFSpanCache_RejectsKeysThatAreNotDigests(t *testing.T) {
	t.Parallel()
	cache, err := OPFSpanCacheAt(t.TempDir())
	require.NoError(t, err)
	for _, key := range []string{"", "../escape", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		require.Error(t, cache.StoreOPFSpans(key, nil), "key %q", key)
		_, ok := cache.LoadOPFSpans(key)
		assert.False(t, ok, "key %q", key)
	}
}

func TestOPFSpanCache_CorruptEntryReadsAsMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cache, err := OPFSpanCacheAt(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, OPFSpanCacheDirName, testOPFKey+".json"), []byte("{not json"), 0o600))
	_, ok := cache.LoadOPFSpans(testOPFKey)
	assert.False(t, ok)
}

func TestPruneOPFSpanCache_RemovesOnlyOldEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cache, err := OPFSpanCacheAt(dir)
	require.NoError(t, err)
	oldKey, newKey := strings.Repeat("0a", 32), strings.Repeat("0b", 32)
	require.NoError(t, cache.StoreOPFSpans(oldKey, nil))
	require.NoError(t, cache.StoreOPFSpans(newKey, nil))
	now := time.Now()
	oldPath := filepath.Join(dir, OPFSpanCacheDirName, oldKey+".json")
	require.NoError(t, os.Chtimes(oldPath, now.Add(-2*OPFSpanCacheMaxAge), now.Add(-2*OPFSpanCacheMaxAge)))

	removed, err := PruneOPFSpanCache(dir, now, OPFSpanCacheMaxAge)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	_, ok := cache.LoadOPFSpans(oldKey)
	assert.False(t, ok, "the stale entry must be gone")
	_, ok = cache.LoadOPFSpans(newKey)
	assert.True(t, ok, "a recent entry must survive")
}

// The cache must live in the repository it was opened for, whatever the
// process's working directory is. Resolving it from the working directory once
// wrote test results into the developer's own checkout.
func TestOPFSpanCacheForRepo_UsesTheRepositoryNotTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })

	cache, err := OPFSpanCacheForRepo(repo)
	require.NoError(t, err)
	require.NoError(t, cache.StoreOPFSpans(testOPFKey, nil))

	_, err = os.Stat(filepath.Join(dir, ".git", OPFSpanCacheDirName, testOPFKey+".json"))
	require.NoError(t, err, "the entry must be written under the repository's own git dir")
}
