package checkpoint

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

// High-entropy values the entropy layer redacts, one placeholder each.
var countsSecrets = []string{
	"Xk9mP2qR7vL4nW8jT3hY6bF1cD5gA0sZ",
	"Qw3eR5tY7uI9oP1aS3dF5gH7jK9lZ2xC",
	"Mn8bV6cX4zL2kJ0hG8fD6sA4pO2iU9yT",
}

// transcriptWithSecrets is a transcript holding the first n countsSecrets,
// redacted the way checkpoint writers redact.
func transcriptWithSecrets(t *testing.T, n int) redact.RedactedBytes {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"type":"user","text":"start"}` + "\n")
	for _, s := range countsSecrets[:n] {
		b.WriteString(`{"type":"assistant","text":"found ` + s + ` in the log"}` + "\n")
	}
	out, err := redact.JSONLBytes([]byte(b.String()))
	require.NoError(t, err)
	require.NotNil(t, out.Counts())
	return out
}

// redactionCountsStore is the store surface these tests use. Both persistent
// backends are tested, since each could shadow the shared tree writer.
type redactionCountsStore interface {
	Write(ctx context.Context, req WriteRequest) error
	Read(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*Metadata, error)
}

func forEachCountsBackend(t *testing.T, run func(t *testing.T, store redactionCountsStore)) {
	t.Helper()
	backends := []struct {
		name     string
		newStore func(repo *git.Repository) redactionCountsStore
	}{
		{"git-branch store", func(repo *git.Repository) redactionCountsStore { return NewGitStore(repo, DefaultV1Refs()) }},
		{"git-refs store", func(repo *git.Repository) redactionCountsStore { return newGitRefsStore(repo) }},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			repo, _ := setupBranchTestRepo(t)
			run(t, b.newStore(repo))
		})
	}
}

func writeCountsSession(t *testing.T, store redactionCountsStore, cpID id.CheckpointID, sessionID string, transcript redact.RedactedBytes) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   transcript,
		Prompts:      []string{"do the thing"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
}

func sessionRedaction(t *testing.T, store redactionCountsStore, cpID id.CheckpointID, index int) *redact.Counts {
	t.Helper()
	meta, err := store.ReadSessionMetadata(context.Background(), cpID, index)
	require.NoError(t, err)
	return meta.Redaction
}

func rootRedaction(t *testing.T, store redactionCountsStore, cpID id.CheckpointID) *redact.Counts {
	t.Helper()
	summary, err := store.Read(context.Background(), cpID)
	require.NoError(t, err)
	return summary.Redaction
}

func TestWrite_StoresRedactionCountsPerSessionAndSumsThem(t *testing.T) {
	t.Parallel()
	forEachCountsBackend(t, func(t *testing.T, store redactionCountsStore) {
		cpID := id.MustCheckpointID("a1b2c3d4e5f6")
		one, two := transcriptWithSecrets(t, 1), transcriptWithSecrets(t, 2)
		writeCountsSession(t, store, cpID, "session-one", one)
		writeCountsSession(t, store, cpID, "session-two", two)

		require.Equal(t, one.Counts(), sessionRedaction(t, store, cpID, 0))
		require.Equal(t, two.Counts(), sessionRedaction(t, store, cpID, 1))
		root := rootRedaction(t, store, cpID)
		require.Equal(t, one.Counts().Add(two.Counts()), root)
		require.Equal(t, 3, root.Replacements)

		meta, err := store.ReadSessionMetadata(context.Background(), cpID, 1)
		require.NoError(t, err)
		data, err := json.Marshal(meta)
		require.NoError(t, err)
		for _, s := range countsSecrets {
			require.NotContains(t, string(data), s)
		}
	})
}

// The root total is left out rather than understated when a session's stored
// transcript has no counts, as with one written by an older CLI. A session
// that stored no transcript has nothing to count and does not block it.
func TestWrite_RootRedactionCountsNeedEveryStoredTranscriptCounted(t *testing.T) {
	t.Parallel()
	forEachCountsBackend(t, func(t *testing.T, store redactionCountsStore) {
		cpID := id.MustCheckpointID("b2c3d4e5f6a1")
		counted := transcriptWithSecrets(t, 1)
		writeCountsSession(t, store, cpID, "session-counted", counted)
		writeCountsSession(t, store, cpID, "session-no-transcript", redact.RedactedBytes{})
		require.Nil(t, sessionRedaction(t, store, cpID, 1), "a session without a transcript has no counts")
		require.Equal(t, counted.Counts(), rootRedaction(t, store, cpID))

		writeCountsSession(t, store, cpID, "session-uncounted", redact.AlreadyRedacted([]byte(`{"text":"old"}`+"\n")))
		require.Nil(t, sessionRedaction(t, store, cpID, 2))
		require.Equal(t, counted.Counts(), sessionRedaction(t, store, cpID, 0))
		require.Nil(t, rootRedaction(t, store, cpID))
	})
}

// Turn-end finalize replaces a checkpoint's transcript with the full session,
// so the counts must describe the transcript that ends up stored.
func TestUpdate_TranscriptRewriteReplacesRedactionCounts(t *testing.T) {
	t.Parallel()
	forEachCountsBackend(t, func(t *testing.T, store redactionCountsStore) {
		ctx := context.Background()
		cpID := id.MustCheckpointID("c3d4e5f6a1b2")
		writeCountsSession(t, store, cpID, "session-one", transcriptWithSecrets(t, 1))

		full := transcriptWithSecrets(t, 3)
		require.NoError(t, store.Write(ctx, SessionTranscript{CheckpointID: cpID, SessionID: "session-one", Transcript: full}))
		require.Equal(t, full.Counts(), sessionRedaction(t, store, cpID, 0))
		require.Equal(t, full.Counts(), rootRedaction(t, store, cpID))

		// Identical bytes without counts still describe the stored transcript.
		require.NoError(t, store.Write(ctx, SessionTranscript{CheckpointID: cpID, SessionID: "session-one", Transcript: redact.AlreadyRedacted(full.Bytes())}))
		require.Equal(t, full.Counts(), sessionRedaction(t, store, cpID, 0))

		// Different bytes without counts: the old counts no longer apply.
		require.NoError(t, store.Write(ctx, SessionTranscript{CheckpointID: cpID, SessionID: "session-one", Transcript: redact.AlreadyRedacted([]byte(`{"text":"rewritten"}` + "\n"))}))
		require.Nil(t, sessionRedaction(t, store, cpID, 0))
		require.Nil(t, rootRedaction(t, store, cpID))
	})
}

// Checkpoints written before counts existed carry no redaction key; they must
// read as unknown, and a metadata file without counts must not gain the key.
func TestMetadata_WithoutRedactionCountsReadsAsUnknown(t *testing.T) {
	t.Parallel()
	var meta Metadata
	require.NoError(t, json.Unmarshal([]byte(`{"checkpoint_id":"a1b2c3d4e5f6","session_id":"s","strategy":"manual-commit","created_at":"2025-12-01T12:34:56Z","checkpoints_count":1,"files_touched":[]}`), &meta))
	require.Nil(t, meta.Redaction)

	var summary CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(`{"checkpoint_id":"a1b2c3d4e5f6","strategy":"manual-commit","checkpoints_count":1,"files_touched":[],"sessions":[]}`), &summary))
	require.Nil(t, summary.Redaction)

	for _, v := range []any{Metadata{}, CheckpointSummary{}} {
		data, err := json.Marshal(v)
		require.NoError(t, err)
		require.NotContains(t, string(data), `"redaction"`)
	}
}

// The prefix cache splices a reused redacted prefix onto a freshly redacted
// suffix; the counts it returns must equal a full redaction's, round after
// round, while the prefix is actually reused.
func TestRedactTranscriptCached_CountsMatchFullRedaction(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()

	var redactedBytes int
	redactor := func(_ context.Context, b []byte) (redact.RedactedBytes, error) {
		redactedBytes += len(b)
		return redact.JSONLBytes(b)
	}

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	for round := range 4 {
		appended := ""
		if round > 0 {
			appended = transcriptLines(round*10_000, 50)
			content += appended
		}
		redactedBytes = 0
		got, err := RedactTranscriptCached(ctx, repo, "session-counts", []byte(content), redactor)
		require.NoError(t, err)

		want, err := redact.JSONLBytes([]byte(content))
		require.NoError(t, err)
		require.Equal(t, string(want.Bytes()), string(got.Bytes()))
		require.NotNil(t, got.Counts(), "round %d", round)
		require.Positive(t, got.Counts().Replacements, "the transcript holds secrets in every line")
		require.Equal(t, want.Counts(), got.Counts(), "round %d", round)
		if round > 0 {
			require.Equal(t, len(appended), redactedBytes, "round %d must reuse the cached prefix", round)
		}
	}

	// Nothing appended: the cached prefix alone is the result.
	redactedBytes = 0
	got, err := RedactTranscriptCached(ctx, repo, "session-counts", []byte(content), redactor)
	require.NoError(t, err)
	want, err := redact.JSONLBytes([]byte(content))
	require.NoError(t, err)
	require.Zero(t, redactedBytes)
	require.Equal(t, want.Counts(), got.Counts())
}

// A cache entry without counts (left by a build from before counts existed)
// cannot produce a total, so it is not reused; the next store records counts
// and reuse resumes.
func TestRedactTranscriptCached_EntryWithoutCountsIsNotReused(t *testing.T) {
	withSmallRedactCacheThreshold(t)
	repo, _ := newTestRepoForCache(t)
	ctx := context.Background()
	const sessionID = "session-legacy-entry"

	var redactedBytes int
	redactor := func(_ context.Context, b []byte) (redact.RedactedBytes, error) {
		redactedBytes += len(b)
		return redact.JSONLBytes(b)
	}

	content := padPastCacheThreshold(t, transcriptLines(0, 100))
	_, err := RedactTranscriptCached(ctx, repo, sessionID, []byte(content), redactor)
	require.NoError(t, err)

	cache := repoRedactCache(repo)
	key := transcriptCacheKey(sessionID)
	entry := cache.load(key)
	require.NotNil(t, entry)
	require.NotNil(t, entry.Counts, "a store must record the prefix's counts")
	legacy := *entry
	legacy.Counts = nil
	writeCacheEntry(t, cache, key, legacy)

	content += transcriptLines(20_000, 50)
	redactedBytes = 0
	got, err := RedactTranscriptCached(ctx, repo, sessionID, []byte(content), redactor)
	require.NoError(t, err)
	require.Equal(t, len(content), redactedBytes, "an entry without counts must not be reused")
	want, err := redact.JSONLBytes([]byte(content))
	require.NoError(t, err)
	require.Equal(t, want.Counts(), got.Counts())

	appended := transcriptLines(30_000, 50)
	content += appended
	redactedBytes = 0
	_, err = RedactTranscriptCached(ctx, repo, sessionID, []byte(content), redactor)
	require.NoError(t, err)
	require.Equal(t, len(appended), redactedBytes, "reuse resumes once counts are stored")
}
