package clusterdiscovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// coresHandler serves a fixed core_urls list and counts how many times
// /.well-known was hit, so tests can assert cache hits vs live fetches.
func coresHandler(t *testing.T, calls *int32, coreURLs ...string) http.HandlerFunc {
	t.Helper()
	body, err := json.Marshal(Response{CoreURLs: coreURLs})
	require.NoError(t, err)
	return func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		assert.Equal(t, Path, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) //nolint:errcheck // test
	}
}

// TestResolve_ActiveContextWinsWhenEligible: the active context is issued
// by one of the cluster's cores, so it is used even though another eligible
// context exists for the same core.
func TestResolve_ActiveContextWinsWhenEligible(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "bob@eu",
		Contexts: []*contexts.Context{
			{Name: "alice@eu", CoreURL: "https://eu.auth.entire.io", Handle: "alice", KeychainService: "kc:alice"},
			{Name: "bob@eu", CoreURL: "https://eu.auth.entire.io", Handle: "bob", KeychainService: "kc:bob"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "bob@eu", c.Name, "active eligible context must win over other same-core accounts")
}

// TestResolve_SoleEligibleContextUsedDespiteUnrelatedActive: the active
// context is on an unrelated core, but the user has exactly one context
// eligible for the cluster — use it (the 99% single-account case).
func TestResolve_SoleEligibleContextUsedDespiteUnrelatedActive(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "paul@unrelated",
		Contexts: []*contexts.Context{
			{Name: "paul@unrelated", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:unrelated"},
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)
}

// TestResolve_AmbiguousMultipleEligibleErrors: two same-core accounts are
// eligible and neither is active — refuse to guess, list both, and tell the
// user to pick. This is the footgun this redesign closes.
func TestResolve_AmbiguousMultipleEligibleErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://core-us.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "paul@unrelated",
		Contexts: []*contexts.Context{
			{Name: "alice@core-us", CoreURL: "https://core-us.entire.io", Handle: "alice", KeychainService: "kc:alice"},
			{Name: "admin@core-us", CoreURL: "https://core-us.entire.io", Handle: "admin", KeychainService: "kc:admin"},
			{Name: "paul@unrelated", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:unrelated"},
		},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "cluster1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple login contexts")
	assert.Contains(t, err.Error(), "admin@core-us")
	assert.Contains(t, err.Error(), "alice@core-us")
	assert.Contains(t, err.Error(), "entire auth use")
}

// TestResolve_NoEligibleContextReturnsLoginHint: discovery succeeds but the
// user has no context for any advertised core.
func TestResolve_NoEligibleContextReturnsLoginHint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "staging",
		Contexts: []*contexts.Context{
			{Name: "staging", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:staging"},
		},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no auth context for cluster aws-eu-central-1.entire.io")
	assert.Contains(t, err.Error(), "https://eu.auth.entire.io")
	assert.Contains(t, err.Error(), "entire login")
	assert.Contains(t, err.Error(), "ENTIRE_AUTH_BASE_URL")
}

// TestResolve_CoresCachedAcrossCalls: the first call hits /.well-known and
// caches the cores; the second is served from cluster_cores.json with no
// network hit.
func TestResolve_CoresCachedAcrossCalls(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(coresHandler(t, &calls, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	cacheDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod-eu",
		Contexts: []*contexts.Context{
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "first call fetches /.well-known")

	c2, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c2.Name)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "second call is served from the cores cache")

	// The cores fact is persisted; the account choice is not.
	cache, err := discovery.LoadClusterCores(cacheDir)
	require.NoError(t, err)
	urls, fresh, ok := cache.Get("aws-eu-central-1.entire.io")
	require.True(t, ok)
	assert.True(t, fresh)
	assert.Equal(t, []string{"https://eu.auth.entire.io"}, urls)
}

// TestResolve_StaleCacheFallbackOnDiscoveryFailure: an expired cache entry
// is used when the live re-fetch fails, so a brief cluster outage doesn't
// break an operation whose cores we already knew.
func TestResolve_StaleCacheFallbackOnDiscoveryFailure(t *testing.T) {
	t.Parallel()
	// Server is closed immediately so any discovery attempt fails.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := hostPinningClient(t, srv)
	srv.Close()

	configDir := t.TempDir()
	cacheDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod-eu",
		Contexts: []*contexts.Context{
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}))
	// Seed an EXPIRED cores entry.
	require.NoError(t, discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c["aws-eu-central-1.entire.io"] = &discovery.CoresEntry{
			CoreURLs:  []string{"https://eu.auth.entire.io"},
			FetchedAt: time.Now().Add(-discovery.ClusterCoresTTL - time.Hour),
		}
		return nil
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", client, t.Logf)
	require.NoError(t, err, "should fall back to stale cores when re-fetch fails")
	assert.Equal(t, "prod-eu", c.Name)
}

// TestResolve_Unreachable: transport failure with no cached cores surfaces
// the "doesn't look like a cluster" message.
func TestResolve_Unreachable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := hostPinningClient(t, srv)
	srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "current",
		Contexts: []*contexts.Context{
			{Name: "current", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:current"},
		},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "missing.example.com", client, t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.example.com doesn't look like a cluster, or it is unreachable")
}

// TestResolve_503: HTTP 503 from /.well-known points at the admin, not the
// user.
func TestResolve_503(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no issuers", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		Contexts: []*contexts.Context{{Name: "x", CoreURL: "https://x.example", Handle: "x", KeychainService: "kc:x"}},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "rc.partial.to", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not advertise any trusted login servers")
	assert.Contains(t, err.Error(), "cluster administrator")
}
