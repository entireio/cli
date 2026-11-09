package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

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
// damaged record. storePrefixBytes always stamps the current fingerprint, so it
// cannot express these cases.
func writeCacheEntry(t *testing.T, cache *redactCache, treePath string, entry redactPrefixEntry) {
	t.Helper()
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, osroot.WriteFile(cache.root, cache.entryName(treePath), data, 0o600))
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

// writeAndRedact runs the incremental pipeline RedactTranscriptCached uses —
// redactIncrementally, then storePrefixBytes when it asks — against cache under
// key name, and returns the redacted bytes.
func writeAndRedact(t *testing.T, cache *redactCache, name, content string) []byte {
	t.Helper()
	ctx := context.Background()
	res, err := redactIncrementally(ctx, cache, []byte(content), name, jsonlRedactor)
	require.NoError(t, err)
	if res.StorePrefix {
		cache.storePrefixBytes(ctx, name, res.SourceHash, len(content), res.Redacted, res.Counts)
	}
	return res.Redacted
}

// TestIncrementalRedaction_MatchesFullRedaction is the correctness core: growing
// a transcript across checkpoints must produce exactly what redacting the final
// file in one pass produces.
func TestIncrementalRedaction_MatchesFullRedaction(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	require.NotNil(t, cache)

	content := padPastCacheThreshold(t, transcriptLines(0, 100))

	// First checkpoint: nothing cached, full redaction, primes the cache.
	first := writeAndRedact(t, cache, "full.jsonl", content)
	require.NotNil(t, cache.load("full.jsonl"), "first write should prime the cache")

	// Append and re-checkpoint several times, as a session does.
	for round := 1; round <= 4; round++ {
		content += transcriptLines(round*10_000, 50)
		got := writeAndRedact(t, cache, "full.jsonl", content)

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
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, "full.jsonl", content)

	// Rewrite the beginning while keeping the length identical, so only the hash
	// check can catch it.
	rewritten := []byte(content)
	marker := []byte(`{"i":0,"text":"REPLACED-CONTENT-HERE`)
	copy(rewritten, marker)
	rewritten = append(rewritten, []byte(transcriptLines(9_999, 5))...)

	got := writeAndRedact(t, cache, "full.jsonl", string(rewritten))
	want, wantErr := RedactBlobBytes(context.Background(), rewritten, "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got),
		"a rewritten prefix must fall back to full redaction")
}

// TestIncrementalRedaction_FingerprintMismatchFallsBack ensures output redacted
// under different rules is never spliced into a new result.
func TestIncrementalRedaction_FingerprintMismatchFallsBack(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, "full.jsonl", content)

	entry := cache.load("full.jsonl")
	require.NotNil(t, entry)
	stale := *entry
	stale.Fingerprint = "stale-fingerprint"
	writeCacheEntry(t, cache, "full.jsonl", stale)

	content += transcriptLines(500, 20)
	got := writeAndRedact(t, cache, "full.jsonl", content)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
}

// TestIncrementalRedaction_PartialTrailingLineNotCached documents the invariant
// that only newline-terminated content is cacheable, and that a file with a
// partial final line still redacts correctly.
func TestIncrementalRedaction_PartialTrailingLineNotCached(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	partial := content + `{"i":999,"text":"half written sk-live-999abcdefghij`

	got := writeAndRedact(t, cache, "full.jsonl", partial)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(partial), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
	require.Nil(t, cache.load("full.jsonl"),
		"content without a trailing newline must not be cached")
}

// TestIncrementalRedaction_UnchangedContentReusesPrefix covers a Stop hook that
// fires with no new transcript lines.
func TestIncrementalRedaction_UnchangedContentReusesPrefix(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	first := writeAndRedact(t, cache, "full.jsonl", content)
	second := writeAndRedact(t, cache, "full.jsonl", content)
	require.Equal(t, string(first), string(second))
}

// TestIncrementalRedaction_SkippedUnlessLargeSessionTranscript keeps the fast
// path narrow: only a large full.jsonl takes it.
func TestIncrementalRedaction_SkippedUnlessLargeSessionTranscript(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	ctx := context.Background()

	small := transcriptLines(0, 5)
	writeAndRedact(t, cache, "full.jsonl", small)
	require.Nil(t, cache.load("full.jsonl"), "small files should not be cached")

	big := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, "other.jsonl", big)
	require.Nil(t, cache.load("other.jsonl"),
		"only the session transcript filename is cached, not any .jsonl")

	require.False(t, incrementalRedactionCandidate([]byte(big), "transcript.jsonl"),
		"the regenerated compact transcript must not qualify")
	require.False(t, incrementalRedactionCandidate([]byte(big), "full.jsonl.001"),
		"chunked transcript parts must not qualify")
	require.True(t, incrementalRedactionCandidate([]byte(big), ".entire/metadata/s1/full.jsonl"))

	// A nil cache disables reuse but must still return a correct full redaction:
	// the whole-content fallback lives inside redactIncrementally so both callers
	// cannot spell it differently.
	noCacheResult, noCacheErr := redactIncrementally(ctx, nil, []byte(big), "full.jsonl", jsonlRedactor)
	require.NoError(t, noCacheErr)
	require.False(t, noCacheResult.StorePrefix, "a nil cache must not record a prefix")
	want, wantErr := RedactBlobBytes(ctx, []byte(big), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(noCacheResult.Redacted),
		"a nil cache must still redact the whole content")
}

// TestRedactCache_IgnoresCorruptEntry proves a damaged cache file degrades to a
// full redaction rather than failing the checkpoint.
func TestRedactCache_IgnoresCorruptEntry(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, "full.jsonl", content)

	require.NoError(t, osroot.WriteFile(cache.root, cache.entryName("full.jsonl"), []byte("{not json"), 0o600))
	require.Nil(t, cache.load("full.jsonl"))

	content += transcriptLines(700, 10)
	got := writeAndRedact(t, cache, "full.jsonl", content)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
}

// TestRedactCache_MissingPrefixFileFallsBack covers a cache entry pointing at a
// prefix file that no longer exists.
func TestRedactCache_MissingPrefixFileFallsBack(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, "full.jsonl", content)

	entry := cache.load("full.jsonl")
	require.NotNil(t, entry)
	broken := *entry
	broken.RedactedFile = "no-such-prefix.prefix"
	writeCacheEntry(t, cache, "full.jsonl", broken)

	content += transcriptLines(800, 10)
	got := writeAndRedact(t, cache, "full.jsonl", content)
	want, wantErr := RedactBlobBytes(context.Background(), []byte(content), "full.jsonl", false)
	require.NoError(t, wantErr)
	require.Equal(t, string(want), string(got))
}

// errStopStore stands in for whatever stopped a store before its entry landed.
var errStopStore = errors.New("store stopped before its entry")

// storeStoppingBeforeEntry runs one checkpoint whose cache store stops between
// its payload write and its entry write. stop runs in place of the entry write:
// returning an error is a failed write, panicking is a process killed between
// the two, so nothing after the payload write runs.
func storeStoppingBeforeEntry(t *testing.T, cache *redactCache, key, content string, stop func() error) {
	t.Helper()
	original := writeCacheFile
	defer func() { writeCacheFile = original }()

	var payloadWritten, entryReached bool
	writeCacheFile = func(root *os.Root, name string, data []byte, perm fs.FileMode) error {
		if name == cache.entryName(key) {
			entryReached = true
			return stop()
		}
		if err := original(root, name, data, perm); err != nil {
			return err
		}
		payloadWritten = true
		return nil
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); !ok || !errors.Is(err, errStopStore) {
					panic(r)
				}
			}
		}()
		writeAndRedact(t, cache, key, content)
	}()
	require.True(t, payloadWritten, "the store must write its payload before stopping, or this covers nothing")
	require.True(t, entryReached, "the store never reached its entry write")
}

// requireReusesIntactPrefix runs the checkpoint after a store that stopped
// early. It must match a full redaction and still reuse the last complete
// prefix, which covered reusedBytes of the source.
func requireReusesIntactPrefix(t *testing.T, cache *redactCache, key, content string, reusedBytes int) {
	t.Helper()
	ctx := context.Background()
	var sawBytes int
	res, err := redactIncrementally(ctx, cache, []byte(content), key, func(ctx context.Context, b []byte) (redact.RedactedBytes, error) {
		sawBytes += len(b)
		return jsonlRedactor(ctx, b)
	})
	require.NoError(t, err)
	want, err := RedactBlobBytes(ctx, []byte(content), key, false)
	require.NoError(t, err)
	require.Equal(t, string(want), string(res.Redacted),
		"a store that stopped before its entry must not pair the previous entry with a newer payload")
	require.Equal(t, len(content)-reusedBytes, sawBytes, "the last complete prefix must still be reused")
}

// TestRedactCache_FailedEntryWriteKeepsPrefixIntact: a store whose entry write
// fails must leave the previous entry and its payload usable together.
func TestRedactCache_FailedEntryWriteKeepsPrefixIntact(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	const key = "full.jsonl"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, key, content)

	grown := content + transcriptLines(10_000, 50)
	storeStoppingBeforeEntry(t, cache, key, grown, func() error { return errStopStore })

	requireReusesIntactPrefix(t, cache, key, grown+transcriptLines(20_000, 50), len(content))
}

// TestRedactCache_InterruptedStoreKeepsPrefixIntact is the same guarantee for a
// process killed between the payload and entry writes.
func TestRedactCache_InterruptedStoreKeepsPrefixIntact(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	const key = "full.jsonl"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, key, content)

	grown := content + transcriptLines(10_000, 50)
	storeStoppingBeforeEntry(t, cache, key, grown, func() error { panic(errStopStore) })

	requireReusesIntactPrefix(t, cache, key, grown+transcriptLines(20_000, 50), len(content))
}

// TestRedactCache_ShortPayloadFallsBack covers a payload shorter than its entry
// recorded. Cache writes do not fsync, so a power loss can leave one empty or
// cut short, and reusing it would drop the lines it no longer holds.
func TestRedactCache_ShortPayloadFallsBack(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	ctx := context.Background()
	const key = "full.jsonl"

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	writeAndRedact(t, cache, key, content)
	entry := cache.load(key)
	require.NotNil(t, entry)
	payload := cache.name + "/" + entry.RedactedFile
	full, err := osroot.ReadFile(cache.root, payload)
	require.NoError(t, err)

	grown := content + transcriptLines(30_000, 20)
	want, err := RedactBlobBytes(ctx, []byte(grown), key, false)
	require.NoError(t, err)

	// A cut at a line boundary passes the newline check on its own.
	firstLine := full[:bytes.IndexByte(full, '\n')+1]
	for _, short := range [][]byte{nil, firstLine} {
		require.NoError(t, osroot.WriteFile(cache.root, payload, short, 0o600))
		res, err := redactIncrementally(ctx, cache, []byte(grown), key, jsonlRedactor)
		require.NoError(t, err)
		require.Equal(t, string(want), string(res.Redacted),
			"a %d-byte payload under an entry for %d bytes must not be reused", len(short), len(full))
	}
}

// TestRedactCache_KeepsOnePayloadPerKey: every store writes a new payload file,
// so it must remove the key's others: the one the previous entry named, an
// orphan from a store that stopped before its entry, and the fixed-name file
// older CLIs wrote. Another key's payload stays.
func TestRedactCache_KeepsOnePayloadPerKey(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))
	const key = "full.jsonl"

	keyHash := strings.TrimSuffix(path.Base(cache.entryName(key)), ".json")
	otherHash := strings.TrimSuffix(path.Base(cache.entryName("other/full.jsonl")), ".json")
	otherPayload := otherHash + "-kept.prefix"
	for _, name := range []string{keyHash + ".prefix", keyHash + "-orphan.prefix", otherPayload} {
		require.NoError(t, osroot.WriteFile(cache.root, cache.name+"/"+name, []byte("stale\n"), 0o600))
	}

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	for round := range 3 {
		content += transcriptLines(round*10_000, 20)
		writeAndRedact(t, cache, key, content)
	}

	entry := cache.load(key)
	require.NotNil(t, entry)
	files, err := osroot.ReadDir(cache.root, cache.name)
	require.NoError(t, err)
	var payloads []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".prefix") {
			payloads = append(payloads, f.Name())
		}
	}
	require.ElementsMatch(t, []string{entry.RedactedFile, otherPayload}, payloads)
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
	withSmallRedactCacheThreshold(t)
	_, dir := newTestRepoForCache(t)
	cache := newRedactCache(filepath.Join(dir, ".git"))

	content := openCodeExport(t, redactCacheMinBytes+1024)
	require.Greater(t, len(content), redactCacheMinBytes, "fixture must exceed the size gate")
	require.True(t, strings.HasSuffix(content, "\n"), "fixture must pass the newline gate")
	require.False(t, incrementalRedactionCandidate([]byte(content), "full.jsonl"),
		"a single JSON value on the transcript path must not qualify")

	// A real write must leave nothing for a later checkpoint to splice onto.
	writeAndRedact(t, cache, "full.jsonl", content)
	require.Nil(t, cache.load("full.jsonl"),
		"a single-JSON-value transcript must leave no cache entry")
}

// withSmallRedactCacheThreshold lowers the size gate for one test so fixtures can
// be kilobytes instead of a megabyte. What these tests exercise is the splice
// logic, not the threshold; paying real redaction cost on megabyte fixtures cost
// ~5 minutes under -race across this file and timed out CI. The one test that
// cares about the gate itself sets its own sizes.
func withSmallRedactCacheThreshold(t *testing.T) {
	t.Helper()
	original := redactCacheMinBytes
	redactCacheMinBytes = 8 << 10 // 8KiB
	t.Cleanup(func() { redactCacheMinBytes = original })
}

// TestRedactCacheMinBytes_ProductionDefault pins the shipped threshold, since
// every other test in this file overrides it and would not notice a change.
func TestRedactCacheMinBytes_ProductionDefault(t *testing.T) {
	require.Equal(t, 1<<20, redactCacheMinBytes,
		"production default must stay 1MiB; tests override it via withSmallRedactCacheThreshold")
}
