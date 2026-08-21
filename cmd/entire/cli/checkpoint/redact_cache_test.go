package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

// transcriptLines builds JSONL lines that all contain redactable material, so
// any splicing bug shows up as a content difference rather than passing by luck.
func transcriptLines(from, count int) string {
	var b strings.Builder
	for i := from; i < from+count; i++ {
		fmt.Fprintf(&b, `{"i":%d,"text":"connect postgres://u:pw%d@h/db token sk-live-%dabcdefghijklmnopqrst"}`+"\n", i, i, i)
	}
	return b.String()
}

// padPastCacheThreshold grows content past redactCacheMinBytes so the
// incremental path engages.
func padPastCacheThreshold(t *testing.T, content string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(content)
	i := 0
	for b.Len() < redactCacheMinBytes+1024 {
		b.WriteString(transcriptLines(1_000_000+i, 200))
		i += 200
	}
	return b.String()
}

// writeCacheEntry persists a hand-built entry so tests can simulate a stale or
// damaged record. storePrefix always stamps the current fingerprint, so it
// cannot express these cases.
func writeCacheEntry(t *testing.T, cache *redactCache, treePath string, entry redactPrefixEntry) {
	t.Helper()
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cache.path(treePath), data, 0o600))
}

func newTestRepoForCache(t *testing.T) (*git.Repository, string) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "seed.txt", "seed")
	testutil.GitAdd(t, dir, "seed.txt")
	testutil.GitCommit(t, dir, "seed")
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	return repo, dir
}

// writeAndRedact runs the production blob path and returns the redacted bytes
// that were stored.
func writeAndRedact(t *testing.T, repo *git.Repository, cache *redactCache, dir, name, content string) []byte {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	hash, _, err := createRedactedBlobFromFile(context.Background(), repo, cache, path, name)
	require.NoError(t, err)
	got, err := readBlobBytes(repo, hash)
	require.NoError(t, err)
	return got
}

// TestIncrementalRedaction_MatchesFullRedaction is the correctness core: growing
// a transcript across checkpoints must produce exactly what redacting the final
// file in one pass produces.
func TestIncrementalRedaction_MatchesFullRedaction(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	require.NotNil(t, cache)

	content := padPastCacheThreshold(t, transcriptLines(0, 100))

	// First checkpoint: nothing cached, full redaction, primes the cache.
	first := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	require.NotNil(t, cache.load("full.jsonl"), "first write should prime the cache")

	// Append and re-checkpoint several times, as a session does.
	for round := 1; round <= 4; round++ {
		content += transcriptLines(round*10_000, 50)
		got := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)

		want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
		require.NoError(t, wantErr)
		require.Equal(t, string(want), string(got),
			"round %d: incremental output must equal a full redaction", round)
	}
	require.NotEmpty(t, first)
}

// TestIncrementalRedaction_RewrittenPrefixFallsBack covers a transcript whose
// earlier bytes changed (a compaction, say). Reusing the old prefix there would
// store stale content, so it must redact everything again.
func TestIncrementalRedaction_RewrittenPrefixFallsBack(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, repo, cache, dir, "full.jsonl", content)

	// Rewrite the beginning while keeping the length identical, so only the hash
	// check can catch it.
	rewritten := []byte(content)
	marker := []byte(`{"i":0,"text":"REPLACED-CONTENT-HERE`)
	copy(rewritten, marker)
	rewritten = append(rewritten, []byte(transcriptLines(9_999, 5))...)

	got := writeAndRedact(t, repo, cache, dir, "full.jsonl", string(rewritten))
	want, wantErr := RedactBlobBytes(context.Background(), rewritten, "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got),
		"a rewritten prefix must fall back to full redaction")
}

// TestIncrementalRedaction_FingerprintMismatchFallsBack ensures output redacted
// under different rules is never spliced into a new result.
func TestIncrementalRedaction_FingerprintMismatchFallsBack(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, repo, cache, dir, "full.jsonl", content)

	entry := cache.load("full.jsonl")
	require.NotNil(t, entry)
	stale := *entry
	stale.Fingerprint = "stale-fingerprint"
	writeCacheEntry(t, cache, "full.jsonl", stale)

	content += transcriptLines(500, 20)
	got := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
}

// TestIncrementalRedaction_PartialTrailingLineNotCached documents the invariant
// that only newline-terminated content is cacheable, and that a file with a
// partial final line still redacts correctly.
func TestIncrementalRedaction_PartialTrailingLineNotCached(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	partial := content + `{"i":999,"text":"half written sk-live-999abcdefghij`

	got := writeAndRedact(t, repo, cache, dir, "full.jsonl", partial)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(partial), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
	require.Nil(t, cache.load("full.jsonl"),
		"content without a trailing newline must not be cached")
}

// TestIncrementalRedaction_UnchangedContentReusesPrefix covers a Stop hook that
// fires with no new transcript lines.
func TestIncrementalRedaction_UnchangedContentReusesPrefix(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	first := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	second := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	require.Equal(t, string(first), string(second))
}

// TestIncrementalRedaction_SkippedUnlessLargeSessionTranscript keeps the fast
// path narrow: only a large full.jsonl takes it.
func TestIncrementalRedaction_SkippedUnlessLargeSessionTranscript(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	ctx := context.Background()

	small := transcriptLines(0, 5)
	writeAndRedact(t, repo, cache, dir, "full.jsonl", small)
	require.Nil(t, cache.load("full.jsonl"), "small files should not be cached")

	big := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, repo, cache, dir, "other.jsonl", big)
	require.Nil(t, cache.load("other.jsonl"),
		"only the session transcript filename is cached, not any .jsonl")

	require.False(t, incrementalRedactionCandidate([]byte(big), "transcript.jsonl"),
		"the regenerated compact transcript must not qualify")
	require.False(t, incrementalRedactionCandidate([]byte(big), "full.jsonl.001"),
		"chunked transcript parts must not qualify")
	require.True(t, incrementalRedactionCandidate([]byte(big), ".entire/metadata/s1/full.jsonl"))

	noCacheResult, noCacheErr := redactIncrementally(ctx, repo, nil, []byte(big), "full.jsonl")
	require.NoError(t, noCacheErr)
	require.Nil(t, noCacheResult.Redacted,
		"a nil cache must disable the incremental path")
}

// TestRedactCache_IgnoresCorruptEntry proves a damaged cache file degrades to a
// full redaction rather than failing the checkpoint.
func TestRedactCache_IgnoresCorruptEntry(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, repo, cache, dir, "full.jsonl", content)

	require.NoError(t, os.WriteFile(cache.path("full.jsonl"), []byte("{not json"), 0o600))
	require.Nil(t, cache.load("full.jsonl"))

	content += transcriptLines(700, 10)
	got := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
}

// TestRedactCache_MissingBlobFallsBack covers a cache entry pointing at an object
// that is no longer reachable (pruned, or a different clone).
func TestRedactCache_MissingBlobFallsBack(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, repo, cache, dir, "full.jsonl", content)

	entry := cache.load("full.jsonl")
	require.NotNil(t, entry)
	broken := *entry
	missing := sha256.Sum256([]byte("no such blob"))
	broken.RedactedBlob = hex.EncodeToString(missing[:])[:40]
	writeCacheEntry(t, cache, "full.jsonl", broken)

	content += transcriptLines(800, 10)
	got := writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
}

// openCodeExport builds an OpenCode-shaped transcript: a single JSON object
// written to the same full.jsonl path other agents write JSONL to, sized past the
// cache threshold and newline-terminated like a redirected stdout.
//
// Uses few, large messages rather than many small ones: the single-JSON-value
// path is field-aware per leaf, so leaf count -- not total size -- dominates its
// cost, and thousands of tiny leaves make this test minutes long.
func openCodeExport(t *testing.T, minBytes int) string {
	t.Helper()
	type msg struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	var msgs []msg
	var body []byte
	for i := 0; ; i++ {
		msgs = append(msgs, msg{
			ID: fmt.Sprintf("msg_%06d", i),
			Text: fmt.Sprintf("token sk-live-%dabcdefghij postgres://u:pw%d@h/db ", i, i) +
				strings.Repeat("the quick brown fox jumps over the lazy dog ", 600),
		})
		var err error
		body, err = json.Marshal(map[string]any{
			"info":     map[string]any{"id": "ses_abc", "title": "session"},
			"messages": msgs,
		})
		require.NoError(t, err)
		if len(body) > minBytes {
			break
		}
	}
	return string(body) + "\n"
}

// TestIncrementalRedaction_SingleJSONValueNeverCached is the guard for the
// OpenCode shape. Splicing a fragment of a single JSON object would redact it
// with raw entropy detection instead of the field-aware whole-document pass, so
// it must never enter the cache no matter how large it gets or that it lands on
// the full.jsonl path.
func TestIncrementalRedaction_SingleJSONValueNeverCached(t *testing.T) {
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := openCodeExport(t, redactCacheMinBytes+1024)
	require.Greater(t, len(content), redactCacheMinBytes, "fixture must exceed the size gate")
	require.True(t, strings.HasSuffix(content, "\n"), "fixture must pass the newline gate")
	require.False(t, incrementalRedactionCandidate([]byte(content), "full.jsonl"),
		"a single JSON value on the transcript path must not qualify")

	// A real write must leave nothing for a later checkpoint to splice onto.
	writeAndRedact(t, repo, cache, dir, "full.jsonl", content)
	require.Nil(t, cache.load("full.jsonl"),
		"a single-JSON-value transcript must leave no cache entry")
}
