package checkpoint

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

	"github.com/stretchr/testify/require"
)

// jsonlRedactor is the production pipeline the in-memory paths use.
func jsonlRedactor(_ context.Context, b []byte) (redact.RedactedBytes, error) {
	return redact.JSONLBytes(b)
}

// countingRedactor is jsonlRedactor plus a tally of the bytes it was handed,
// which is how these tests tell reuse from a silent full redaction.
func countingRedactor(saw *int) func(context.Context, []byte) (redact.RedactedBytes, error) {
	return func(ctx context.Context, b []byte) (redact.RedactedBytes, error) {
		*saw += len(b)
		return jsonlRedactor(ctx, b)
	}
}

// failingRedactor stands in for a degraded scanner.
func failingRedactor(context.Context, []byte) (redact.RedactedBytes, error) {
	return redact.RedactedBytes{}, redact.ErrScannerDegraded
}

// requireMatchesFullRedaction pins the property the whole cache rests on.
func requireMatchesFullRedaction(t *testing.T, content string, got redact.RedactedBytes, msgAndArgs ...any) {
	t.Helper()
	want, err := redact.JSONLBytes([]byte(content))
	require.NoError(t, err)
	require.Equal(t, string(want.Bytes()), string(got.Bytes()), msgAndArgs...)
}

// TestRedactTranscriptCached_MatchesFullRedaction is the correctness core
// for the in-memory paths (condensation and the Stop finalize rewrite): growing a
// transcript across checkpoints must produce exactly what one whole-transcript
// pass produces.
func TestRedactTranscriptCached_MatchesFullRedaction(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-abc"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))

	for round := range 5 {
		if round > 0 {
			content += transcriptLines(round*10_000, 50)
		}
		got, err := RedactTranscriptCached(
			ctx, repo, session, []byte(content), jsonlRedactor)
		require.NoError(t, err)

		requireMatchesFullRedaction(t, content, got,
			"round %d: incremental output must equal a whole-transcript redaction", round)
	}
}

// TestRedactTranscriptCached_ActuallyReusesPrefix proves the fast path is
// engaged rather than silently falling back — otherwise the test above would
// pass just as happily with the cache doing nothing.
func TestRedactTranscriptCached_ActuallyReusesPrefix(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-reuse"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptCached(ctx, repo, session, []byte(content), jsonlRedactor)
	require.NoError(t, err)

	// Second round: count the bytes the redactor is handed. With reuse it sees
	// only the appended suffix, never the whole transcript.
	appended := transcriptLines(50_000, 40)
	grown := content + appended

	var sawBytes int
	counting := countingRedactor(&sawBytes)
	got, err := RedactTranscriptCached(ctx, repo, session, []byte(grown), counting)
	require.NoError(t, err)

	require.Equal(t, len(appended), sawBytes,
		"only the appended suffix should be redacted, not the whole %d-byte transcript", len(grown))

	requireMatchesFullRedaction(t, grown, got)
}

// TestRedactTranscriptCached_SessionsAreIsolated: two concurrent sessions in
// one worktree must not share a prefix.
func TestRedactTranscriptCached_SessionsAreIsolated(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()

	contentA := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptCached(ctx, repo, "sess-A", []byte(contentA), jsonlRedactor)
	require.NoError(t, err)

	contentB := padPastCacheThreshold(t, transcriptLines(500_000, 100))
	var sawBytes int
	counting := countingRedactor(&sawBytes)
	got, err := RedactTranscriptCached(ctx, repo, "sess-B", []byte(contentB), counting)
	require.NoError(t, err)
	require.Equal(t, len(contentB), sawBytes, "session B must not reuse session A's prefix")

	requireMatchesFullRedaction(t, contentB, got)
}

// TestRedactTranscriptCached_OptsOutWithoutRepoOrSession covers the callers
// that deliberately decline caching (the per-subagent transcript passes a nil
// repo). Output must still be correct.
func TestRedactTranscriptCached_OptsOutWithoutRepoOrSession(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	want, wantErr := redact.JSONLBytes([]byte(content))
	require.NoError(t, wantErr)

	t.Run("nil repo", func(t *testing.T) {
		got, err := RedactTranscriptCached(ctx, nil, "s1", []byte(content), jsonlRedactor)
		require.NoError(t, err)
		require.Equal(t, string(want.Bytes()), string(got.Bytes()))
	})

	t.Run("empty session", func(t *testing.T) {
		got, err := RedactTranscriptCached(ctx, repo, "", []byte(content), jsonlRedactor)
		require.NoError(t, err)
		require.Equal(t, string(want.Bytes()), string(got.Bytes()))
	})
}

// TestRedactTranscriptCached_RedactorErrorPropagates: a degraded scanner
// must fail the write rather than store under-scanned content.
func TestRedactTranscriptCached_RedactorErrorPropagates(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	content := padPastCacheThreshold(t, transcriptLines(0, 100))

	_, err := RedactTranscriptCached(ctx, repo, "sess-err", []byte(content), failingRedactor)
	require.ErrorIs(t, err, redact.ErrScannerDegraded)
}

// TestRedactTranscriptCached_SuffixErrorPropagates is the same guarantee on
// the reuse path: a failure redacting the appended lines must not silently ship
// the reused prefix alone.
func TestRedactTranscriptCached_SuffixErrorPropagates(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-suffix"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptCached(ctx, repo, session, []byte(content), jsonlRedactor)
	require.NoError(t, err)

	grown := content + transcriptLines(80_000, 10)
	_, err = RedactTranscriptCached(ctx, repo, session, []byte(grown), failingRedactor)
	require.ErrorIs(t, err, redact.ErrScannerDegraded)
}

// TestRedactTranscriptCached_SingleJSONValueNotSpliced guards the OpenCode
// shape: a single JSON object has no line structure, so splicing it would drop
// out of field-aware redaction into raw entropy detection over a fragment.
func TestRedactTranscriptCached_SingleJSONValueNotSpliced(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()

	content := openCodeExport(t, redactCacheMinBytes+1024)

	_, err := RedactTranscriptCached(ctx, repo, "sess-oc", []byte(content), jsonlRedactor)
	require.NoError(t, err)

	cache := repoRedactCache(repo)
	require.NotNil(t, cache)
	require.Nil(t, cache.load(transcriptCacheKey("sess-oc")),
		"a single JSON value must never be cached for splicing")
}

// TestRedactTranscriptCached_PrefixSurvivesGC pins why the in-memory prefix is a
// file rather than a git blob. A whole-transcript blob matches no chunk the store
// writes (it chunks at agent.MaxChunkSize), so it stays unreachable and `git gc`
// prunes it -- silently reverting every later checkpoint to a full redaction.
func TestRedactTranscriptCached_PrefixSurvivesGC(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, dir := newTestRepoForCache(t)
	ctx := context.Background()
	const session = "sess-gc"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptCached(ctx, repo, session, []byte(content), jsonlRedactor)
	require.NoError(t, err)

	testutil.RunGit(t, dir, "gc", "--prune=all", "--quiet")

	// Reopen: gc rewrote the object store underneath the cached handle.
	reopened, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)

	grown := content + transcriptLines(90_000, 40)
	var sawBytes int
	counting := countingRedactor(&sawBytes)
	got, err := RedactTranscriptCached(ctx, reopened, session, []byte(grown), counting)
	require.NoError(t, err)

	require.Equal(t, len(grown)-len(content), sawBytes,
		"the prefix must still be reusable after git gc pruned unreachable objects")
	requireMatchesFullRedaction(t, grown, got)
}

// TestRedactCache_EntryWithNoPayloadFallsBack covers a cache record naming
// neither a prefix file nor a blob. load() rejects that shape, so this exercises
// readPrefix's own guard and confirms the degradation is a full redaction rather
// than a failed checkpoint.
func TestRedactCache_EntryWithNoPayloadFallsBack(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	require.NotNil(t, cache)
	ctx := context.Background()

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	const treePath = "full.jsonl"

	// Hand-built: storePrefix always names a payload, so it cannot express this.
	writeCacheEntry(t, cache, treePath, redactPrefixEntry{
		Fingerprint: redactionFingerprint(),
		SourceBytes: len(content) / 2,
		SourceHash:  hashBytes([]byte(content[:len(content)/2])),
	})
	require.Nil(t, cache.load(treePath), "load must reject an entry with no payload")

	got, err := redactIncrementally(ctx, repo, cache, []byte(content), treePath, testRedactor)
	require.NoError(t, err)
	want, wantErr := RedactBlobBytes(ctx, []byte(content), treePath, false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got.Redacted),
		"a payload-less entry must degrade to a full redaction")
}
