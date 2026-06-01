package clusterdiscovery

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// TestResolveContextForClusterBindingShortCircuits: an explicit
// cluster_contexts binding is used directly — no /.well-known hit.
func TestResolveContextForClusterBindingShortCircuits(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "current",
		ClusterContexts: map[string]string{
			"bound.example.com": "us",
		},
		Contexts: []*contexts.Context{
			{Name: "current", CoreURL: "https://current.example", Handle: "alice", KeychainService: "kc:current"},
			{Name: "us", CoreURL: "https://us.auth.entire.io", Handle: "bob", KeychainService: "kc:us"},
		},
	}))

	// Fail-loud HTTP client: any actual request is a bug.
	failClient := &http.Client{Transport: failTransport(t)}

	c, err := ResolveContextForCluster(t.Context(), configDir, "bound.example.com", failClient, t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "us", c.Name)
}

// TestResolveContextForClusterDiscoversAndAutoBinds: no binding, no
// fallback to current_context — instead we hit /.well-known, match
// the first advertised core URL against a local context, and persist
// the binding for next time.
func TestResolveContextForClusterDiscoversAndAutoBinds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		assert.Equal(t, Path, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"core_urls":["https://eu.auth.entire.io","https://us.auth.entire.io"]}`)) //nolint:errcheck // test
	}))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		// current_context points at a STAGING login — the bug we're
		// fixing is that this used to silently get reused against a
		// prod cluster host. The PROD login below should be the one
		// discovery picks via the well-known match.
		CurrentContext: "staging-eu",
		Contexts: []*contexts.Context{
			{Name: "staging-eu", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:staging"},
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod-eu"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name, "should pick the prod context, NOT current_context")

	// Auto-bound: the second call doesn't hit /.well-known.
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "first call hits discovery once")
	c2, err := ResolveContextForCluster(t.Context(), configDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c2.Name)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "auto-bind should skip discovery on the second call")
}

// TestResolveContextForClusterPrefersCurrentAmongSameCoreMatches: when a
// core has several accounts (alice@core, bob@core) and bob is the active
// context, discovery must auto-bind to bob, not to whichever was saved
// first — otherwise a clone silently authenticates as the wrong user.
func TestResolveContextForClusterPrefersCurrentAmongSameCoreMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"core_urls":["https://eu.auth.entire.io"]}`)) //nolint:errcheck // test
	}))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "bob@eu",
		Contexts: []*contexts.Context{
			{Name: "alice@eu", CoreURL: "https://eu.auth.entire.io", Handle: "alice", KeychainService: "kc:alice"},
			{Name: "bob@eu", CoreURL: "https://eu.auth.entire.io", Handle: "bob", KeychainService: "kc:bob"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "bob@eu", c.Name, "should prefer the active context among same-core matches")
}

// TestResolveContextForClusterNoMatchReturnsLoginHint: discovery
// succeeds but the user has no context for any of the advertised core
// URLs. Error message tells them which login URL to use — that's the
// whole point of having /.well-known.
func TestResolveContextForClusterNoMatchReturnsLoginHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"core_urls":["https://eu.auth.entire.io"]}`)) //nolint:errcheck // test
	}))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "staging",
		Contexts: []*contexts.Context{
			{Name: "staging", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:staging"},
		},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no auth context for cluster aws-eu-central-1.entire.io")
	assert.Contains(t, err.Error(), "https://eu.auth.entire.io")
	assert.Contains(t, err.Error(), "entire login")
	assert.Contains(t, err.Error(), "ENTIRE_AUTH_BASE_URL")
}

// TestResolveContextForClusterStaleBindingFallsThrough: a binding that
// names a non-existent context is treated as if no binding exists —
// we discover, match, and rebind to a real context.
func TestResolveContextForClusterStaleBindingFallsThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"core_urls":["https://eu.auth.entire.io"]}`)) //nolint:errcheck // test
	}))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "current",
		ClusterContexts: map[string]string{
			"aws-eu-central-1.entire.io": "deleted",
		},
		Contexts: []*contexts.Context{
			{Name: "current", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:current"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "current", c.Name)
}

// TestResolveContextForClusterUnreachable: transport failure surfaces
// the "doesn't look like a cluster" message — the user can't tell a
// typo from a real-but-down cluster, and either way the next step is
// the same.
func TestResolveContextForClusterUnreachable(t *testing.T) {
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

	_, err := ResolveContextForCluster(t.Context(), configDir, "missing.example.com", client, t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.example.com doesn't look like a cluster, or it is unreachable")
}

// TestResolveContextForCluster503: HTTP 503 from /.well-known is a
// distinct operator surface — the cluster is up but has no trusted
// issuers configured. The error must point at the admin, not the user.
func TestResolveContextForCluster503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no issuers", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		Contexts: []*contexts.Context{{Name: "x", CoreURL: "https://x.example", Handle: "x", KeychainService: "kc:x"}},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, "rc.partial.to", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not advertise any trusted login servers")
	assert.Contains(t, err.Error(), "cluster administrator")
}

// failTransport returns a RoundTripper that fails the test if invoked —
// used by the binding-short-circuit test to prove we never hit the
// network.
func failTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected HTTP call: binding should have short-circuited discovery")
		return nil, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
