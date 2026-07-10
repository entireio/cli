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
	assert.WithinDuration(t, time.Now(), loadRemoteCache().FetchedAt, time.Minute)
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
}
