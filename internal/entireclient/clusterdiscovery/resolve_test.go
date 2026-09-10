package clusterdiscovery

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// coresHandler serves a fixed core_urls list (plus a jurisdiction audience,
// as every current cluster advertises one — a cached entry without it is
// deliberately treated as stale) and counts how many times /.well-known was
// hit, so tests can assert cache hits vs live fetches.
func coresHandler(t *testing.T, calls *int32, coreURLs ...string) http.HandlerFunc {
	t.Helper()
	body, err := json.Marshal(Response{CoreURLs: coreURLs, JurisdictionAudience: "https://eu.entire.io"})
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

// TestResolve_UnrelatedActiveContextUsesSoleEligibleLogin: the active context is
// on an unrelated core while exactly one saved context is eligible, so that one
// is used. Someone holding logins in two federations can clone from either
// without first retargeting every shell on the machine with `auth use`.
func TestResolve_UnrelatedActiveContextUsesSoleEligibleLogin(t *testing.T) {
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
	assert.Equal(t, "prod-eu", c.Name, "the sole login the cluster trusts is the only possible answer")
}

// TestResolve_SeveralEligibleLoginsAreAmbiguous: auto-selection settles a single
// candidate only. With two, picking one would make the acting identity depend on
// what else happens to be stored, so the resolver names both, sorted, and stops.
func TestResolve_SeveralEligibleLoginsAreAmbiguous(t *testing.T) {
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
	assert.Contains(t, err.Error(), "multiple login contexts can authenticate against cluster cluster1.entire.io")
	assert.Contains(t, err.Error(), "admin@core-us, alice@core-us", "candidates must be listed in sorted order")
	assert.Contains(t, err.Error(), "entire auth use")
}

// TestResolve_ActiveContextIneligibleAndNothingElseFits: an active context that
// the cluster doesn't trust, with no saved alternative. The user needs a new
// login, so the hint must name the servers the cluster actually trusts —
// otherwise bare `entire login` re-authenticates against the default server and
// reproduces this same error.
func TestResolve_ActiveContextIneligibleAndNothingElseFits(t *testing.T) {
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
	require.NotErrorIs(t, err, ErrNoAuthContext, "an incompatible selected login is not a logged-out state")
	assert.Contains(t, err.Error(), `"staging"`, "name the active login that was rejected")
	assert.Contains(t, err.Error(), "no other saved login does either")
	assert.Contains(t, err.Error(), "https://eu.auth.entire.io", "the trusted server is the only actionable detail")
	// The situation is already explained in the first line; don't also assert
	// "no auth context for <cluster>", which reads as logged-out.
	assert.NotContains(t, err.Error(), "no auth context for")
	assert.Contains(t, err.Error(), "entire login --server")
	// Nothing local to switch to, so offering `auth use` would be a dead end.
	assert.NotContains(t, err.Error(), "entire auth use")
}

// loginURLCoresHandler serves a discovery document that advertises a login
// server alongside the trusted issuers, the shape every cluster serves once
// it is running a build that carries ENTIRE_CORE_AUTH_BASE_URL through.
func loginURLCoresHandler(t *testing.T, calls *int32, loginURL string, coreURLs ...string) http.HandlerFunc {
	t.Helper()
	body, err := json.Marshal(Response{
		CoreURLs:             coreURLs,
		JurisdictionAudience: "https://eu.entire.io",
		LoginURL:             loginURL,
	})
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

// TestResolve_AdvertisedLoginURLIsTheWholeRemedy: when the cluster names a
// login server, the hint is one command against one host — the apex router
// dispatches to whichever regional core owns the account, so listing the
// issuers next to it would be noise the user has to triage.
func TestResolve_AdvertisedLoginURLIsTheWholeRemedy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(loginURLCoresHandler(t, nil, "https://auth.entire.io",
		"https://eu.auth.entire.io", "https://us.auth.entire.io"))
	defer srv.Close()

	_, err := ResolveContextForCluster(t.Context(), t.TempDir(), t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAuthContext)
	assert.Contains(t, err.Error(), "no auth context for cluster aws-eu-central-1.entire.io")
	assert.Contains(t, err.Error(), "Log in with `entire login`,")
	assert.NotContains(t, err.Error(), "--server")
	assert.NotContains(t, err.Error(), "It trusts these login servers")
	assert.NotContains(t, err.Error(), "https://eu.auth.entire.io")
}

// TestRenderLoginInstruction covers the three remedies a resource can produce.
func TestRenderLoginInstruction(t *testing.T) {
	t.Parallel()

	t.Run("a non-default login server is named, padding folded", func(t *testing.T) {
		t.Parallel()
		got := renderLoginInstruction(loginTargets{
			coreURLs: []string{"https://eu.auth.partial.to"},
			loginURL: " https://auth.partial.to/ ",
		})
		assert.Equal(t, "Log in with `entire login --server https://auth.partial.to`, then re-run your command.", got)
	})

	t.Run("the default login server needs no flag", func(t *testing.T) {
		t.Parallel()
		for _, advertised := range []string{api.DefaultAuthBaseURL, " " + api.DefaultAuthBaseURL + "/ "} {
			got := renderLoginInstruction(loginTargets{
				coreURLs: []string{"https://us.auth.entire.io"},
				loginURL: advertised,
			})
			assert.Equal(t, "Log in with `entire login`, then re-run your command.", got,
				"advertised as %q", advertised)
			assert.NotContains(t, got, "--server")
		}
	})

	t.Run("falls back to the trusted issuers", func(t *testing.T) {
		t.Parallel()
		got := renderLoginInstruction(loginTargets{
			coreURLs: []string{"https://eu.auth.entire.io/", "https://eu.auth.entire.io"},
		})
		assert.Contains(t, got, "It trusts these login servers: https://eu.auth.entire.io\n")
		assert.Contains(t, got, "entire login --server <url>")
	})

	t.Run("nothing advertised", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "Log in with `entire login`, then re-run your command.",
			renderLoginInstruction(loginTargets{}))
	})
}

// TestResolve_NoActiveContextReturnsLoginHint: genuinely logged out (no
// current_context) — the plain login hint, still naming the cluster's servers.
func TestResolve_NoActiveContextReturnsLoginHint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	_, err := ResolveContextForCluster(t.Context(), t.TempDir(), t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAuthContext)
	assert.Contains(t, err.Error(), "no auth context for cluster aws-eu-central-1.entire.io")
	assert.Contains(t, err.Error(), "https://eu.auth.entire.io")
	assert.Contains(t, err.Error(), "entire login --server")
	// With no active login there is nothing to report as rejected.
	assert.NotContains(t, err.Error(), "does not accept your active login")
}

// TestResolve_DanglingCurrentContextFallsBackToSoleEligibleLogin: current_context
// naming a context that no longer exists is not an identity, so selection falls
// through to the sole saved login the cluster trusts rather than dead-ending.
func TestResolve_DanglingCurrentContextFallsBackToSoleEligibleLogin(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "deleted-long-ago",
		Contexts: []*contexts.Context{
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}))

	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)
}

// TestResolve_ContextWithoutCoreURLIsNeverEligible: a context carrying no
// issuer matches nothing, even against a resource advertising an empty core URL.
// Without the guard, normalizeCoreURL("") == normalizeCoreURL("") would make a
// blank context authenticate anything.
//
// It must also not be OFFERED as a candidate. The accept check and the candidate
// list are one predicate for this reason: when they were two, this input
// produced "does not accept your active login \"blank\". These saved logins can
// authenticate it: blank" — telling the user to switch to the login they were
// already on.
func TestResolve_ContextWithoutCoreURLIsNeverEligible(t *testing.T) {
	t.Parallel()
	f := &contexts.File{
		CurrentContext: "blank",
		Contexts:       []*contexts.Context{{Name: "blank", Handle: "paul", KeychainService: "kc:blank"}},
	}
	_, err := selectLoginContext(f, "cluster c.entire.io", "c.entire.io", loginTargets{coreURLs: []string{""}}, t.Logf)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "These saved logins can authenticate it",
		"a context rejected by the accept check must not be offered as a candidate")
	assert.NotContains(t, err.Error(), "entire auth use")
}

// TestResolve_EligibilityIgnoresWhitespaceAndTrailingSlash: the accept check and
// the trusted-server list share one normaliser, so a padded advertised core is
// accepted rather than rejected-then-echoed-back-as-trusted.
func TestResolve_EligibilityIgnoresWhitespaceAndTrailingSlash(t *testing.T) {
	t.Parallel()
	f := &contexts.File{
		CurrentContext: "eu",
		Contexts:       []*contexts.Context{{Name: "eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:eu"}},
	}
	c, err := selectLoginContext(f, "cluster c.entire.io", "c.entire.io", loginTargets{coreURLs: []string{" https://eu.auth.entire.io/ "}}, t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "eu", c.Name)
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
	entry, fresh, ok := cache.GetEntry("aws-eu-central-1.entire.io")
	require.True(t, ok)
	assert.True(t, fresh)
	assert.Equal(t, []string{"https://eu.auth.entire.io"}, entry.CoreURLs)
}

// TestResolve_ClusterHostCaseInsensitive: a mixed-case cluster host resolves
// the same context as its lowercase form and caches under the canonical
// (lowercased) key, since DNS hosts are case-insensitive.
func TestResolve_ClusterHostCaseInsensitive(t *testing.T) {
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

	c, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "AWS-EU-Central-1.Entire.IO", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)

	// Cached under the canonical lowercase host, so the lowercase form is a hit.
	cache, err := discovery.LoadClusterCores(cacheDir)
	require.NoError(t, err)
	_, _, ok := cache.GetEntry("aws-eu-central-1.entire.io")
	assert.True(t, ok, "cores cached under the lowercased host key")

	c2, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c2.Name)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "lowercase form hits the cache the mixed-case call populated")
}

// TestResolve_StaleCacheFallbackOnDiscoveryFailure: an expired cache entry
// is used when the live re-fetch fails, so a brief cluster outage doesn't
// break an operation whose cores we already knew.
func TestResolve_StaleCacheFallbackOnDiscoveryFailure(t *testing.T) {
	t.Parallel()
	// Pinned at a host that never answers, so any discovery attempt fails.
	client := deadPinningClient(t)

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

// TestResolve_PreAudienceCacheRefetched: a fresh cache entry that predates
// the jurisdiction_audience field is re-fetched immediately (not after the
// 24h TTL), so clients pick up a cluster upgrade right away.
func TestResolve_PreAudienceCacheRefetched(t *testing.T) {
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
	// Seed a FRESH entry without a jurisdiction audience (pre-upgrade shape).
	require.NoError(t, discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c["aws-eu-central-1.entire.io"] = &discovery.CoresEntry{
			CoreURLs:  []string{"https://eu.auth.entire.io"},
			FetchedAt: time.Now(),
		}
		return nil
	}))

	got, err := ResolveClusterAuth(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "pre-audience entry must trigger a live re-fetch")
	assert.Equal(t, "https://eu.entire.io", got.JurisdictionAudience)

	// The refreshed entry now carries the audience: served from cache.
	_, err = ResolveClusterAuth(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "audience-bearing entry must be served from cache")
}

// TestResolve_PreAudienceEntryNotUsedAsDiscoveryFallback: when the forced
// pre-audience re-fetch fails, the audience-less stale entry must NOT be
// served to an audience-requiring caller — that would misdiagnose a
// transient discovery failure as "cluster doesn't do jurisdiction tokens".
func TestResolve_PreAudienceEntryNotUsedAsDiscoveryFallback(t *testing.T) {
	t.Parallel()
	client := deadPinningClient(t) // discovery fails

	configDir := t.TempDir()
	cacheDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod-eu",
		Contexts: []*contexts.Context{
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}))
	// FRESH entry, but pre-audience.
	require.NoError(t, discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c["aws-eu-central-1.entire.io"] = &discovery.CoresEntry{
			CoreURLs:  []string{"https://eu.auth.entire.io"},
			FetchedAt: time.Now(),
		}
		return nil
	}))

	_, err := ResolveClusterAuth(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", client, t.Logf)
	require.Error(t, err, "discovery failure must surface, not an empty audience")
	assert.Contains(t, err.Error(), "unreachable")

	// The audience-agnostic resolver still gets the stale-cores fallback.
	c, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", client, t.Logf)
	require.NoError(t, err, "cores-only callers keep the stale fallback")
	assert.Equal(t, "prod-eu", c.Name)
}

// TestResolve_AudienceAgnosticCallersSkipPreAudienceRefetch: a fresh entry
// without an audience is served from cache for callers that don't need the
// audience — no forced re-fetch.
func TestResolve_AudienceAgnosticCallersSkipPreAudienceRefetch(t *testing.T) {
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
	// SetEntry stamps the current schema version, so the only thing this
	// entry is missing is the audience — which is what the test is about.
	require.NoError(t, discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.SetEntry("aws-eu-central-1.entire.io", discovery.CoresEntry{
			CoreURLs: []string{"https://eu.auth.entire.io"},
		})
		return nil
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls), "cores-only caller must be served from the fresh cache")
}

// TestResolve_OlderSchemaCacheRefetched: a fresh entry written before the
// client knew about login_url is re-fetched immediately rather than pinning
// the user to the old multi-server hint for a full TTL — a warm cache is
// exactly what the people this change helps already have. The rewritten entry
// is current, so the next call is served from cache even though this cluster
// advertises no login server.
func TestResolve_OlderSchemaCacheRefetched(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(loginURLCoresHandler(t, &calls, "https://auth.partial.to", "https://eu.auth.entire.io"))
	defer srv.Close()

	// No saved logins, so resolution ends in the login hint — the message the
	// re-fetch is supposed to improve.
	configDir := t.TempDir()
	cacheDir := t.TempDir()
	// Seed a FRESH pre-versioning entry: audience present (so the audience
	// rule can't be what forces the re-fetch), no login server, no version.
	require.NoError(t, discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c["aws-eu-central-1.entire.io"] = &discovery.CoresEntry{
			CoreURLs:             []string{"https://eu.auth.entire.io"},
			JurisdictionAudience: "https://eu.entire.io",
			FetchedAt:            time.Now(),
		}
		return nil
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.ErrorIs(t, err, ErrNoAuthContext)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "pre-versioning entry must trigger a live re-fetch")
	// The whole point: the freshly discovered login server reaches the hint.
	assert.Contains(t, err.Error(), "entire login --server https://auth.partial.to")

	_, err = ResolveContextForCluster(t.Context(), configDir, cacheDir, "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "the rewritten entry is current and must not re-fetch again")
	// Served from cache, and the hint is unchanged — the login server survives
	// the round-trip through cluster_cores.json rather than living only in the
	// response that fetched it.
	assert.Contains(t, err.Error(), "entire login --server https://auth.partial.to")

	cache, cacheErr := discovery.LoadClusterCores(cacheDir)
	require.NoError(t, cacheErr)
	cached, fresh, ok := cache.GetEntry("aws-eu-central-1.entire.io")
	require.True(t, ok)
	assert.True(t, fresh)
	assert.Equal(t, "https://auth.partial.to", cached.LoginURL)
	assert.Equal(t, discovery.CoresSchemaVersion, cached.SchemaVersion)
}

// TestResolve_Unreachable: transport failure with no cached cores surfaces
// the "doesn't look like a cluster" message.
func TestResolve_Unreachable(t *testing.T) {
	t.Parallel()
	client := deadPinningClient(t)

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

// TestResolve_ContextOverrideSelectsWithoutMutatingState: --context acts as a
// saved login for one invocation. This is what makes cross-federation work
// possible without `entire auth use`, whose effect is global and sticky — it
// would retarget every other terminal, worktree, and background git hook on the
// machine until switched back.
//
// Mutates the process-wide override, so no t.Parallel.
func TestResolve_ContextOverrideSelectsWithoutMutatingState(t *testing.T) {
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	stored := &contexts.File{
		CurrentContext: "paul@unrelated",
		Contexts: []*contexts.Context{
			{Name: "paul@unrelated", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:unrelated"},
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}
	require.NoError(t, contexts.Save(configDir, stored))

	contexts.SetFlagOverrideForTest(t, "prod-eu")
	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)

	// The whole point: the stored default is untouched, so the next command in
	// another shell still acts as the login the user actually selected.
	reloaded, err := contexts.Load(configDir)
	require.NoError(t, err)
	assert.Equal(t, "paul@unrelated", reloaded.CurrentContext,
		"an override must not persist; that is what distinguishes it from `auth use`")
}

// TestResolve_IneligibleOverrideBlamesTheFlagNotTheStoredDefault: an explicitly
// named context the resource rejects is a hard error, never a fall-through to
// the sole eligible login — the user asked for that identity by name, so acting
// as another behind their back is the failure the override exists to prevent.
//
// And telling them to run `auth use` sends them to change the wrong thing: the
// flag would still override it on the next run.
func TestResolve_IneligibleOverrideBlamesTheFlagNotTheStoredDefault(t *testing.T) {
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod-eu",
		Contexts: []*contexts.Context{
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
			{Name: "staging", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:staging"},
		},
	}))

	contexts.SetFlagOverrideForTest(t, "staging")
	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err, "prod-eu is the sole eligible login, and must still not be substituted")
	assert.Contains(t, err.Error(), "the login selected by --context")
	assert.Contains(t, err.Error(), "prod-eu", "offer the login that would work")
	assert.Contains(t, err.Error(), "--context <context>")
	assert.NotContains(t, err.Error(), "entire auth use",
		"`auth use` cannot fix a run whose identity comes from the flag")
}

// TestResolve_UnknownOverrideFailsBeforeEligibility: "that context doesn't exist"
// and "that context isn't trusted here" are different mistakes. Reporting the
// second for the first would send the user hunting a trust problem over a typo,
// and must never silently fall back to current_context.
func TestResolve_UnknownOverrideFailsBeforeEligibility(t *testing.T) {
	srv := httptest.NewServer(coresHandler(t, nil, "https://eu.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod-eu",
		Contexts:       []*contexts.Context{{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"}},
	}))

	contexts.SetFlagOverrideForTest(t, "prod-eü")
	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	var unknown *contexts.UnknownContextError
	require.ErrorAs(t, err, &unknown, "callers must be able to tell a bad name from a trust failure")
	assert.Contains(t, err.Error(), "prod-eu", "list what is actually saved")
	assert.NotContains(t, err.Error(), "does not accept")
}

// TestResolve_NilStoredContextDoesNotPanic: a hand-edited or truncated
// contexts.json can contain `{"contexts":[null]}`. Every reader of f.Contexts
// must skip it rather than dereference it — this path runs inside
// git-remote-entire during `git push`, where a panic is the worst outcome
// available.
//
// eligibleContexts walks every stored entry to build the candidate list, so the
// nil is reached both when nothing can match and when the real entry is picked.
func TestResolve_NilStoredContextDoesNotPanic(t *testing.T) {
	t.Parallel()
	f := &contexts.File{
		CurrentContext: "gone",
		Contexts: []*contexts.Context{
			nil,
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}

	// Empty coreURLs is the case the old loop shape never dereferenced at all.
	_, err := selectLoginContext(f, "cluster c.entire.io", "c.entire.io", loginTargets{}, t.Logf)
	require.Error(t, err)

	// And with a matching core, the nil entry must be skipped while the real one
	// is auto-selected.
	c, err := selectLoginContext(f, "cluster c.entire.io", "c.entire.io", loginTargets{coreURLs: []string{"https://eu.auth.entire.io"}}, t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name, "the valid entry is still the sole candidate")
}

// A nil entry must also not panic the selection itself, which resolves through
// contexts.File.Find before any eligibility check.
func TestResolve_NilStoredContextIsSelectable(t *testing.T) {
	t.Parallel()
	f := &contexts.File{
		CurrentContext: "prod-eu",
		Contexts: []*contexts.Context{
			nil,
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}
	c, err := selectLoginContext(f, "cluster c.entire.io", "c.entire.io", loginTargets{coreURLs: []string{"https://eu.auth.entire.io"}}, t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)
}

// captureAutoSelectNotice redirects the auto-selection notice into a buffer for
// one test. Callers MUST NOT call t.Parallel: the writer is package-global.
func captureAutoSelectNotice(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := autoSelectNoticeW
	autoSelectNoticeW = &buf
	t.Cleanup(func() { autoSelectNoticeW = prev })
	return &buf
}

// TestResolve_AutoSelectionIsAnnounced: acting as a login the user did not
// choose is exactly the surprise the tier was removed over, so it is never
// silent. The identity the user did choose is not announced — that is the
// expected case, and a line on every command would be noise.
//
// Swaps the package-global writer, so no t.Parallel.
func TestResolve_AutoSelectionIsAnnounced(t *testing.T) {
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

	buf := captureAutoSelectNotice(t)
	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)
	assert.Equal(t, "Using context 'prod-eu'.\n", buf.String())

	// The stored default now fits, so nothing is auto-selected and nothing is said.
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod-eu",
		Contexts: []*contexts.Context{
			{Name: "paul@unrelated", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:unrelated"},
			{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
		},
	}))
	buf.Reset()
	_, err = ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Empty(t, buf.String(), "the active context acting is not news")
}

// TestResolve_ExplicitSelectionIsNotAnnounced: the user named the identity on
// this very command, so repeating it back is noise — and nothing was chosen for
// them, which is the only thing the notice reports.
//
// Swaps the process-wide override and the package-global writer, so no t.Parallel.
func TestResolve_ExplicitSelectionIsNotAnnounced(t *testing.T) {
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

	buf := captureAutoSelectNotice(t)
	contexts.SetFlagOverrideForTest(t, "prod-eu")
	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "aws-eu-central-1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "prod-eu", c.Name)
	assert.Empty(t, buf.String())
}

// TestResolve_NoNoticeOnFailure: an error already explains itself, and a
// "Using context" line above one would name a login that never acted.
//
// Swaps the package-global writer, so no t.Parallel.
func TestResolve_NoNoticeOnFailure(t *testing.T) {
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

	buf := captureAutoSelectNotice(t)
	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "cluster1.entire.io", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Empty(t, buf.String())
}

// TestAutoSelectNoticeWriter_DefaultsToStderr: the notice must never reach
// stdout, which carries the git remote-helper protocol inside
// git-remote-entire. Changing the default to io.Discard would delete the
// feature in production while the suite above stays green.
//
// Pinned by descriptor rather than pointer equality with os.Stderr: under
// `go test -json` the testing package replaces the os.Stderr variable after
// package init, so the value captured at init no longer compares equal even
// though it is the process's real stderr. A buffer still fails this.
//
// Not parallel: reads the package-global writer that the tests above swap.
func TestAutoSelectNoticeWriter_DefaultsToStderr(t *testing.T) {
	f, ok := autoSelectNoticeW.(*os.File)
	if !ok || f.Fd() != uintptr(syscall.Stderr) {
		t.Fatalf("autoSelectNoticeW default = %T, want the process stderr", autoSelectNoticeW)
	}
}

// TestResolve_AutoSelectionOnlyForEntireSites: a self-hosted cluster passes the
// same-site gate (auth.acme.com is acme.com's own), and a single saved acme.com
// login is eligible — but acme.com is not a site we choose credentials for
// unasked. The user is told which login works and switches explicitly; the
// stored default and --context are unaffected.
//
// Swaps the process-wide override and the package-global writer, so no
// t.Parallel.
func TestResolve_AutoSelectionOnlyForEntireSites(t *testing.T) {
	srv := httptest.NewServer(coresHandler(t, nil, "https://auth.acme.com"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod",
		Contexts: []*contexts.Context{
			{Name: "prod", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"},
			{Name: "acme", CoreURL: "https://auth.acme.com", Handle: "paul", KeychainService: "kc:acme"},
		},
	}))

	buf := captureAutoSelectNotice(t)
	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "git.acme.com", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err, "acme is the sole eligible login, and must still not be chosen unasked")
	assert.Contains(t, err.Error(), `cluster git.acme.com does not accept your active login "prod"`)
	assert.Contains(t, err.Error(), "These saved logins can authenticate it: acme")
	assert.Contains(t, err.Error(), "entire auth use")
	assert.Empty(t, buf.String(), "nothing was selected, so nothing is announced")

	// Named explicitly, the same login works — the allowlist gates only the
	// choice made for the user, never the one they made.
	contexts.SetFlagOverrideForTest(t, "acme")
	c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "git.acme.com", hostPinningClient(t, srv), t.Logf)
	require.NoError(t, err)
	assert.Equal(t, "acme", c.Name)
	assert.Empty(t, buf.String())
}

// TestResolve_AutoSelectionFiresForEachEntireSite: the three federations on the
// allowlist, including the motivating case (active login on entire.io, cluster
// on partial.to) and local dev on a port.
//
// Swaps the package-global writer, so no t.Parallel.
func TestResolve_AutoSelectionFiresForEachEntireSite(t *testing.T) {
	cases := []struct {
		clusterHost, core, want string
	}{
		{"royalcanin.partial.to", "https://eu.auth.partial.to", "eu-staging"},
		{"aws-eu-central-1.entire.io", "https://eu.auth.entire.io", "prod-eu"},
		{"localhost:8080", "http://localhost:9000", "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.clusterHost, func(t *testing.T) {
			srv := httptest.NewServer(coresHandler(t, nil, tc.core))
			defer srv.Close()

			configDir := t.TempDir()
			require.NoError(t, contexts.Save(configDir, &contexts.File{
				CurrentContext: "au",
				Contexts: []*contexts.Context{
					{Name: "au", CoreURL: "https://au.auth.entire.io", Handle: "paul", KeychainService: "kc:au"},
					{Name: "eu-staging", CoreURL: "https://eu.auth.partial.to", Handle: "paul", KeychainService: "kc:eu-staging"},
					{Name: "prod-eu", CoreURL: "https://eu.auth.entire.io", Handle: "paul", KeychainService: "kc:prod-eu"},
					{Name: "dev", CoreURL: "http://localhost:9000", Handle: "paul", KeychainService: "kc:dev"},
				},
			}))

			buf := captureAutoSelectNotice(t)
			c, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), tc.clusterHost, hostPinningClient(t, srv), t.Logf)
			require.NoError(t, err)
			assert.Equal(t, tc.want, c.Name)
			assert.Equal(t, "Using context '"+tc.want+"'.\n", buf.String())
		})
	}
}

func TestAutoSelectAllowed(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"aws-eu-central-1.entire.io", "royalcanin.partial.to", "localhost", "localhost:8080", "Entire.IO"} {
		assert.True(t, autoSelectAllowed(host), host)
	}
	for _, host := range []string{"git.acme.com", "entire.io.evil.com", "127.0.0.1", "notentire.io", "partial.to.attacker.net"} {
		assert.False(t, autoSelectAllowed(host), host)
	}
}
