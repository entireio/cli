package bbcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openRecord opens the fallback record of repoPath in a cache directory dir.
func openRecord(t *testing.T, dir, repoPath string) (*FallbackRecord, *fakeClock) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	r, err := openFallbackRecord(root, server, repoPath)
	require.NoError(t, err)
	clock := &fakeClock{t: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	r.now = clock.now
	return r, clock
}

func TestFetchFallbackExpires(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, clock := openRecord(t, dir, repo)
	_, ok := r.Get()
	require.False(t, ok)

	set := clock.now()
	require.NoError(t, r.Set("chunk hash mismatch"))
	fb, ok := r.Get()
	require.True(t, ok)
	require.Equal(t, set.Add(24*time.Hour), fb.Until)
	require.Equal(t, "chunk hash mismatch", fb.Reason)

	other, _ := openRecord(t, dir, repo)
	other.now = clock.now
	_, ok = other.Get()
	require.True(t, ok, "another process sees the record")

	clock.advance(24*time.Hour - time.Second)
	_, ok = r.Get()
	require.True(t, ok)
	clock.advance(time.Second)
	_, ok = r.Get()
	require.False(t, ok)
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r.name)))
	require.ErrorIs(t, err, os.ErrNotExist, "an expired record is removed")
}

func TestFetchFallbackFromTheFutureIsNotValid(t *testing.T) {
	t.Parallel()
	r, clock := openRecord(t, t.TempDir(), repo)
	require.NoError(t, r.Set("x"))
	clock.advance(-time.Minute)
	_, ok := r.Get()
	require.False(t, ok)
}

func TestFetchFallbackBoundsTheReason(t *testing.T) {
	t.Parallel()
	r, _ := openRecord(t, t.TempDir(), repo)
	require.NoError(t, r.Set(strings.Repeat("r", 5000)))
	fb, ok := r.Get()
	require.True(t, ok)
	require.Len(t, fb.Reason, maxFallbackReason)
}

// The record is per repo, and lives outside every clone: two clones of the
// same repo share it.
func TestFetchFallbackIsPerRepo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, _ := openRecord(t, dir, repo)
	require.NoError(t, r.Set("x"))
	other, _ := openRecord(t, dir, "/et/alice/other")
	_, ok := other.Get()
	require.False(t, ok)
	same, _ := openRecord(t, dir, "et/alice/repo/")
	_, ok = same.Get()
	require.True(t, ok)
}

func TestCorruptFetchFallbackIsDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, _ := openRecord(t, dir, repo)
	path := filepath.Join(dir, filepath.FromSlash(r.name))
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	_, ok := r.Get()
	require.False(t, ok)
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// Another process can write a new record between the read of an expired
// record and its removal. The new record stays.
func TestExpiredFetchFallbackKeepsANewRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, clock := openRecord(t, dir, repo)
	require.NoError(t, r.Set("old"))
	path := filepath.Join(dir, filepath.FromSlash(r.name))
	expired, err := os.ReadFile(path)
	require.NoError(t, err)

	clock.advance(25 * time.Hour)
	require.NoError(t, r.Set("new"))
	r.removeUnchanged(expired)
	fb, ok := r.Get()
	require.True(t, ok, "the new record stays")
	require.Equal(t, "new", fb.Reason)
}

func TestOpenFallbackRecordUsesTheUserCache(t *testing.T) {
	// No t.Parallel(): t.Setenv.
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	r, err := OpenFallbackRecord(server, repo)
	require.NoError(t, err)
	require.NoError(t, r.Set("x"))
	_, err = os.Stat(filepath.Join(cache, "entire", filepath.FromSlash(r.name)))
	require.NoError(t, err)

	_, err = OpenFallbackRecord("", repo)
	require.Error(t, err)
}
