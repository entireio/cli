package pricing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validRemoteDoc = `{"schema_version":1,"models":[` +
	`{"id":"gpt-5.5","provider":"openai","input_per_mtok":999,"output_per_mtok":1000}]}`

// priorInputRate is the gpt-5.5 input rate a seeded (pre-existing) cache carries,
// distinct from the embedded (5) and the fetched (999) rates so a test can tell
// which one survived.
const priorInputRate = 111.0

// seedStaleCache writes a well-formed cache with a Doc pricing gpt-5.5 at
// priorInputRate and a FetchedAt old enough to be stale, so a refresh proceeds.
func seedStaleCache(t *testing.T, etag string) {
	t.Helper()
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now().Add(-48 * time.Hour),
		ETag:      etag,
		Doc: &fileSchema{
			SchemaVersion: 1,
			Models: []ModelRate{
				{ID: "gpt-5.5", Provider: "openai", InputPerMTok: priorInputRate, OutputPerMTok: 1000},
			},
		},
	})
}

func priorInput(t *testing.T) float64 {
	t.Helper()
	rc := loadRemoteCache()
	require.NotNil(t, rc)
	require.NotNil(t, rc.Doc)
	require.NotEmpty(t, rc.Doc.Models)
	return rc.Doc.Models[0].InputPerMTok
}

func TestRefreshRemoteCache_200StoresDoc(t *testing.T) {
	isolateCache(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test helper
		io.WriteString(w, validRemoteDoc)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	require.NoError(t, RefreshRemoteCache(context.Background()))
	assert.Equal(t, int32(1), hits.Load())

	rc := loadRemoteCache()
	require.NotNil(t, rc)
	require.NotNil(t, rc.Doc)
	assert.Equal(t, 1, rc.Doc.SchemaVersion)
	assert.Equal(t, `"v1"`, rc.ETag)
	assert.False(t, rc.FetchedAt.IsZero())
	assert.Equal(t, srv.URL, rc.SourceURL)

	entries := LoadRemoteEntries(context.Background())
	require.Len(t, entries, 1)
	assert.InDelta(t, 999.0, entries[0].InputPerMTok, 1e-9)
}

func TestRefreshRemoteCache_404KeepsPrior(t *testing.T) {
	isolateCache(t)
	seedStaleCache(t, "seed")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refreshOutcomeUnchanged, res.Outcome)
	assert.InDelta(t, priorInputRate, priorInput(t), 1e-9, "prior doc must be kept on 404")
	assert.WithinDuration(t, time.Now(), loadRemoteCache().FetchedAt, time.Minute, "FetchedAt must be bumped")
}

func TestRefreshRemoteCache_TimeoutKeepsPrior(t *testing.T) {
	isolateCache(t)
	seedStaleCache(t, "seed")
	old := remoteFetchTimeout
	remoteFetchTimeout = 100 * time.Millisecond
	defer func() { remoteFetchTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test helper; client has already timed out
		io.WriteString(w, validRemoteDoc)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refreshOutcomeUnchanged, res.Outcome)
	assert.InDelta(t, priorInputRate, priorInput(t), 1e-9, "prior doc must be kept on timeout")

	// Regression (finding: "one slow pricing fetch costs a full day of
	// staleness"): a transport-level failure (this timeout) must NOT reset
	// FetchedAt to "now" — that would block any retry for a full
	// remoteRefreshInterval over one slow request. It backdates to the short
	// transportRetryBackoff floor instead, so the next periodic trigger
	// retries well within the day.
	wantFetchedAt := time.Now().Add(-(remoteRefreshInterval - transportRetryBackoff))
	assert.WithinDuration(t, wantFetchedAt, loadRemoteCache().FetchedAt, time.Minute)

	// The cache becomes eligible for retry (ShouldRefresh) within
	// transportRetryBackoff, not the full remoteRefreshInterval — this is the
	// property the finding asks for, not just the raw FetchedAt value.
	timeUntilStale := remoteRefreshInterval - time.Since(loadRemoteCache().FetchedAt)
	assert.LessOrEqual(t, timeUntilStale, transportRetryBackoff+time.Minute,
		"a transport failure must make the cache eligible for retry within transportRetryBackoff, not the full interval")
}

func TestRefreshRemoteCache_GarbageKeepsPrior(t *testing.T) {
	isolateCache(t)
	seedStaleCache(t, "seed")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test helper
		io.WriteString(w, "{ this is not valid json")
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refreshOutcomeUnchanged, res.Outcome)
	assert.InDelta(t, priorInputRate, priorInput(t), 1e-9, "prior doc must be kept on garbage body")
}

func TestRefreshRemoteCache_304KeepsDocBumpsFetchedAt(t *testing.T) {
	isolateCache(t)
	seedStaleCache(t, `"seed-etag"`)
	var sawIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIfNoneMatch = r.Header.Get("If-None-Match")
		if sawIfNoneMatch == `"seed-etag"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test helper
		io.WriteString(w, validRemoteDoc)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, `"seed-etag"`, sawIfNoneMatch, "stored ETag must be sent as If-None-Match")
	assert.Equal(t, refreshOutcomeNotModified, res.Outcome)
	rc := loadRemoteCache()
	assert.InDelta(t, priorInputRate, rc.Doc.Models[0].InputPerMTok, 1e-9, "prior doc must be kept on 304")
	assert.Equal(t, `"seed-etag"`, rc.ETag, "ETag must be retained on 304")
	assert.WithinDuration(t, time.Now(), rc.FetchedAt, time.Minute, "FetchedAt must be bumped on 304")
}

func TestRefreshRemoteCache_SchemaVersion99NotStored(t *testing.T) {
	isolateCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test helper
		io.WriteString(w, `{"schema_version":99,"models":[`+
			`{"id":"gpt-5.5","provider":"openai","input_per_mtok":999,"output_per_mtok":1000}]}`)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refreshOutcomeUnchanged, res.Outcome, "unsupported schema doc must not be stored")
	assert.Nil(t, LoadRemoteEntries(context.Background()), "no mergeable entries from a v99 doc")
}

func TestRefreshRemoteCache_ThrottleSkipsWhenFresh(t *testing.T) {
	isolateCache(t)
	// Fresh cache: FetchedAt now.
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now(),
		Doc: &fileSchema{
			SchemaVersion: 1,
			Models:        []ModelRate{{ID: "gpt-5.5", Provider: "openai", InputPerMTok: priorInputRate, OutputPerMTok: 1000}},
		},
	})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	require.NoError(t, RefreshRemoteCache(context.Background()))
	assert.Equal(t, int32(0), hits.Load(), "a fresh cache must skip the fetch entirely")
}

func TestRefreshRemoteCacheForce_BypassesThrottle(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now(),
		Doc: &fileSchema{
			SchemaVersion: 1,
			Models:        []ModelRate{{ID: "gpt-5.5", Provider: "openai", InputPerMTok: priorInputRate, OutputPerMTok: 1000}},
		},
	})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("ETag", `"v2"`)
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck // test helper
		io.WriteString(w, validRemoteDoc)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load(), "force must bypass the throttle and fetch")
	assert.Equal(t, refreshOutcomeUpdated, res.Outcome)
	assert.InDelta(t, 999.0, priorInput(t), 1e-9, "forced refresh must replace the doc")
}

func TestShouldRefresh(t *testing.T) {
	isolateCache(t)
	assert.True(t, ShouldRefresh(), "missing cache should refresh")

	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: time.Now()})
	assert.False(t, ShouldRefresh(), "fresh cache should not refresh")

	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: time.Now().Add(-48 * time.Hour)})
	assert.True(t, ShouldRefresh(), "stale cache should refresh")

	// A time-fresh cache from a source that no longer matches ENTIRE_PRICING_URL
	// must still refresh: it would otherwise silently block a refresh against
	// the newly configured source for up to 24h.
	t.Setenv("ENTIRE_PRICING_URL", "https://new-source.example/models.json")
	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: time.Now(), SourceURL: "https://old-source.example/models.json"})
	assert.True(t, ShouldRefresh(), "fresh cache from a different source should still refresh")

	// A time-fresh cache whose source DOES match the current URL is still
	// correctly skipped — the source check must not make every cache stale.
	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: time.Now(), SourceURL: "https://new-source.example/models.json"})
	assert.False(t, ShouldRefresh(), "fresh cache from the current source should not refresh")

	// A pre-existing cache with no SourceURL recorded (written before this field
	// existed) must not be treated as a source mismatch — only a genuine,
	// non-empty different source should force a refresh.
	writeRemoteCacheFile(t, &RemoteCache{FetchedAt: time.Now(), SourceURL: ""})
	assert.False(t, ShouldRefresh(), "fresh legacy cache with no recorded source should not refresh")
}

// TestRefreshRemoteCache_SourceChangeDiscardsMismatchedFallback proves that when
// ENTIRE_PRICING_URL has changed since the cache was written, a failed fetch
// against the NEW source does not fall back to the OLD source's Doc/ETag under
// the new source's name — it must correctly end up with no usable cache instead
// of silently mislabeled stale data.
func TestRefreshRemoteCache_SourceChangeDiscardsMismatchedFallback(t *testing.T) {
	isolateCache(t)
	writeRemoteCacheFile(t, &RemoteCache{
		FetchedAt: time.Now().Add(-48 * time.Hour),
		ETag:      "old-source-etag",
		SourceURL: "https://old-source.example/models.json",
		Doc: &fileSchema{
			SchemaVersion: 1,
			Models: []ModelRate{
				{ID: "gpt-5.5", Provider: "openai", InputPerMTok: priorInputRate, OutputPerMTok: 1000},
			},
		},
	})
	var gotIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotFound) // the new source has nothing (yet)
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_PRICING_URL", srv.URL)

	res, err := RefreshRemoteCacheForce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, refreshOutcomeUnchanged, res.Outcome)
	assert.Empty(t, gotIfNoneMatch, "must not send the old source's ETag to the new source")

	rc := loadRemoteCache()
	require.NotNil(t, rc)
	assert.Equal(t, srv.URL, rc.SourceURL)
	assert.Nil(t, rc.Doc, "old source's Doc must not survive under the new SourceURL")
	assert.Empty(t, rc.ETag, "old source's ETag must not survive under the new SourceURL")
	assert.Nil(t, LoadRemoteEntries(context.Background()), "no usable remote entries after a source change with no successful fetch")
}
