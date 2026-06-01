package repocreds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCore counts exchange calls and returns a fresh access_token per call so
// tests can tell cache hits from upstream fetches by inspecting the value the
// Cache hands back. Handler-side validations write to t.Errorf rather than
// require.* so a failed assertion doesn't FailNow the wrong goroutine.
type stubCore struct {
	t         *testing.T
	calls     atomic.Int64
	expiresIn time.Duration
}

func (s *stubCore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			s.t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/oauth/token" {
			s.t.Errorf("path = %s, want /oauth/token", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			s.t.Errorf("ParseForm: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:token-exchange" {
			s.t.Errorf("grant_type = %s", got)
		}
		if r.PostForm.Get("subject_token") == "" {
			s.t.Errorf("subject_token empty")
		}
		if r.PostForm.Get("audience") == "" {
			s.t.Errorf("audience empty")
		}

		n := s.calls.Add(1)
		expires := s.expiresIn
		if expires == 0 {
			expires = 10 * time.Minute
		}
		body, err := json.Marshal(map[string]any{
			"access_token": fmt.Sprintf("scoped-token-%d", n),
			"expires_in":   int(expires.Seconds()),
			"token_type":   "Bearer",
		})
		if err != nil {
			s.t.Errorf("marshal: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) //nolint:errcheck // best-effort in test stub
	})
}

func newTestCache(t *testing.T, core *stubCore) *Cache {
	t.Helper()
	core.t = t
	srv := httptest.NewServer(core.handler())
	t.Cleanup(srv.Close)
	provider := func(context.Context) (string, error) { return "login-jwt", nil }
	return New(srv.URL, "https://cluster.example.com", provider, srv.Client())
}

func TestCache_MintsOnce_Reuses(t *testing.T) {
	core := &stubCore{}
	cache := newTestCache(t, core)

	t1, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	assert.Equal(t, "scoped-token-1", t1)

	t2, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	assert.Equal(t, "scoped-token-1", t2)

	assert.EqualValues(t, 1, core.calls.Load())
}

func TestCache_DistinctKeys(t *testing.T) {
	core := &stubCore{}
	cache := newTestCache(t, core)

	pullTok, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	pushTok, err := cache.Token(t.Context(), "/git/repo/ulid1", "push")
	require.NoError(t, err)
	otherRepoTok, err := cache.Token(t.Context(), "/git/repo/ulid2", "pull")
	require.NoError(t, err)

	assert.NotEqual(t, pullTok, pushTok)
	assert.NotEqual(t, pullTok, otherRepoTok)
	assert.NotEqual(t, pushTok, otherRepoTok)
	assert.EqualValues(t, 3, core.calls.Load())
}

func TestCache_RefreshesAfterExpiry(t *testing.T) {
	// 90s TTL minus 60s safety margin = 30s real cache lifetime. We force
	// expiry by reaching into the entry and rewinding expiresAt.
	core := &stubCore{expiresIn: 90 * time.Second}
	cache := newTestCache(t, core)

	first, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	assert.Equal(t, "scoped-token-1", first)

	cache.mu.Lock()
	for _, e := range cache.entries {
		e.mu.Lock()
		e.expiresAt = time.Now().Add(-time.Second)
		e.mu.Unlock()
	}
	cache.mu.Unlock()

	second, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	assert.Equal(t, "scoped-token-2", second)
	assert.EqualValues(t, 2, core.calls.Load())
}

func TestCache_ShortTTL_StillCachedForHalfLife(t *testing.T) {
	// TTL equal to the safety margin: the margin caps at ttl/2 so the token
	// is still cached for the remaining half rather than re-exchanged on
	// every call. Back-to-back calls should hit the cache.
	core := &stubCore{expiresIn: SafetyMargin}
	cache := newTestCache(t, core)

	_, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	_, err = cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)

	assert.EqualValues(t, 1, core.calls.Load())
}

func TestCache_NonPositiveTTL_NotCached(t *testing.T) {
	// A token that arrives already expired (expires_in <= 0) is handed out
	// once but never cached, so the next call re-exchanges.
	core := &stubCore{expiresIn: -time.Second}
	cache := newTestCache(t, core)

	_, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	_, err = cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)

	assert.EqualValues(t, 2, core.calls.Load())
}

func TestCache_Invalidate(t *testing.T) {
	core := &stubCore{}
	cache := newTestCache(t, core)

	_, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	cache.Invalidate("/git/repo/ulid1", "pull")
	_, err = cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)

	assert.EqualValues(t, 2, core.calls.Load())
}

func TestCache_SerializesConcurrent(t *testing.T) {
	core := &stubCore{}
	cache := newTestCache(t, core)

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	tokens := make([]string, N)
	errs := make([]error, N)
	for i := range N {
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = cache.Token(t.Context(), "/git/repo/ulid1", "pull")
		}()
	}
	wg.Wait()

	// All N callers see the same token; only one upstream exchange happened.
	for i := range N {
		require.NoErrorf(t, errs[i], "goroutine %d", i)
		assert.Equalf(t, "scoped-token-1", tokens[i], "goroutine %d", i)
	}
	assert.EqualValues(t, 1, core.calls.Load())
}

func TestCache_PropagatesLoginProviderError(t *testing.T) {
	core := &stubCore{t: t}
	srv := httptest.NewServer(core.handler())
	t.Cleanup(srv.Close)

	boom := func(context.Context) (string, error) { return "", assert.AnError }
	cache := New(srv.URL, "https://cluster.example.com", boom, srv.Client())

	_, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.ErrorIs(t, err, assert.AnError)
	assert.EqualValues(t, 0, core.calls.Load())
}

func TestCache_RejectsEmptyArgs(t *testing.T) {
	core := &stubCore{}
	cache := newTestCache(t, core)

	_, err := cache.Token(t.Context(), "", "pull")
	require.Error(t, err)
	_, err = cache.Token(t.Context(), "/git/repo/ulid1", "")
	require.Error(t, err)
}

// Defensive smoke test that the audience the cache sends to core is
// clusterURL+audienceSuffix verbatim, since the audience is the resource
// check at the receiver.
func TestCache_BuildsAudience(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		got <- r.PostForm.Get("audience")
		_, _ = w.Write([]byte(`{"access_token":"t","expires_in":600}`)) //nolint:errcheck // best-effort in test stub
	}))
	t.Cleanup(srv.Close)

	cache := New(srv.URL, "https://cluster.example.com",
		func(context.Context) (string, error) { return "login-jwt", nil },
		srv.Client())
	_, err := cache.Token(t.Context(), "/git/repo/ulid1", "pull")
	require.NoError(t, err)
	select {
	case aud := <-got:
		assert.Equal(t, "https://cluster.example.com/git/repo/ulid1", aud)
	case <-time.After(time.Second):
		t.Fatal("audience not captured")
	}
}
