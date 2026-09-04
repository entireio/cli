package clusterdiscovery

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

func TestRegistrableDomain(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"foo.auth.entire.io":         "entire.io",
		"whatever.evil.com":          "evil.com",
		"royalcanin.partial.to":      "partial.to",
		"AWS-EU-Central-1.Entire.IO": "entire.io",
		"entire.io":                  "entire.io",
		// A port is not part of the domain.
		"localhost:8080":     "localhost",
		"127.0.0.1:9000":     "127.0.0.1",
		"auth.acme.com:8443": "acme.com",
		// No registrable domain: match only themselves.
		"localhost":  "localhost",
		"127.0.0.1":  "127.0.0.1",
		"[::1]:9000": "::1",
		"[::1]":      "::1",
		"::1":        "::1",
		// eTLD+1, not "last two labels": co.uk is a public suffix.
		"evil.co.uk":       "evil.co.uk",
		"www.acme.co.uk":   "acme.co.uk",
		"co.uk":            "co.uk",
		"trailing.dot.io.": "dot.io",
	}
	for host, want := range cases {
		assert.Equal(t, want, registrableDomain(host), "registrableDomain(%q)", host)
	}
	assert.NotEqual(t, registrableDomain("evil.co.uk"), registrableDomain("acme.co.uk"),
		"two registrants under one public suffix must not compare equal")
}

func TestRequireSameSiteIssuers(t *testing.T) {
	t.Parallel()

	t.Run("same site passes, across regions, ports, and schemes", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, requireSameSiteIssuers("cluster", "aws-eu-central-1.entire.io",
			[]string{"https://eu.auth.entire.io", " https://us.auth.entire.io/ "}))
		require.NoError(t, requireSameSiteIssuers("cluster", "localhost:8080", []string{"http://localhost:9000"}))
		require.NoError(t, requireSameSiteIssuers("cluster", "127.0.0.1:8080", []string{"http://127.0.0.1:9000"}))
		require.NoError(t, requireSameSiteIssuers("cluster", "git.acme.com", []string{"https://auth.acme.com"}))
	})

	t.Run("one foreign entry refuses the whole list, naming both sides", func(t *testing.T) {
		t.Parallel()
		err := requireSameSiteIssuers("cluster", "evil.com",
			[]string{"https://auth.evil.com", "https://foo.auth.entire.io"})
		require.Error(t, err)
		assert.Equal(t, "cluster evil.com advertises login server https://foo.auth.entire.io outside evil.com; refusing", err.Error())
	})

	t.Run("public suffix is not a shared site", func(t *testing.T) {
		t.Parallel()
		require.Error(t, requireSameSiteIssuers("cluster", "evil.co.uk", []string{"https://auth.acme.co.uk"}))
	})

	t.Run("IP literals match exactly", func(t *testing.T) {
		t.Parallel()
		require.Error(t, requireSameSiteIssuers("cluster", "127.0.0.1:8080", []string{"http://localhost:9000"}))
	})

	t.Run("an entry with no host is refused rather than skipped", func(t *testing.T) {
		t.Parallel()
		err := requireSameSiteIssuers("API host", "partial.to", []string{"https://us.auth.partial.to", ""})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `API host partial.to advertises unusable login server ""`)
	})
}

// TestResolve_ForeignIssuerIsRefused: the threat this gate exists for. A
// cluster the user did not log into advertises a real entire.io core, so a
// saved entire.io login IS eligible by the issuer match alone — and its JWT is
// what git-remote-entire would send the cluster as the bearer. Discovery must
// refuse before any selection tier runs, and must not send the user off to log
// in against the lying host.
func TestResolve_ForeignIssuerIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://foo.auth.entire.io"))
	defer srv.Close()

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod",
		Contexts:       []*contexts.Context{{Name: "prod", CoreURL: "https://foo.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"}},
	}))

	_, err := ResolveContextForCluster(t.Context(), configDir, t.TempDir(), "git.evil.com", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Equal(t, "cluster git.evil.com advertises login server https://foo.auth.entire.io outside evil.com; refusing", err.Error())
	assert.NotContains(t, err.Error(), "entire login")
}

// The data-API path shares the gate: an API host's trusted_issuers must be its
// own.
func TestResolveContextForAPI_ForeignIssuerIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(apiHandler(t, "https://us.auth.partial.to"))
	defer srv.Close()

	configDir := t.TempDir()
	partialContexts(t, configDir)

	_, err := ResolveContextForAPI(t.Context(), configDir, t.TempDir(), "api.evil.com", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)
	assert.Equal(t, "API host api.evil.com advertises login server https://us.auth.partial.to outside evil.com; refusing", err.Error())
	require.NotErrorIs(t, err, ErrDiscoveryUnavailable, "a refusal is not a fallback case")
}

// TestResolve_PoisonedCacheIsRefusedOnRead: the gate runs on the entry handed
// out, not only on the fetch, so a cores entry planted in cluster_cores.json is
// rejected without a network round-trip rather than trusted for its TTL.
func TestResolve_PoisonedCacheIsRefusedOnRead(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	require.NoError(t, discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.SetEntry("git.evil.com", discovery.CoresEntry{
			CoreURLs:             []string{"https://foo.auth.entire.io"},
			JurisdictionAudience: "https://eu.entire.io",
			FetchedAt:            time.Now(),
		})
		return nil
	}))

	configDir := t.TempDir()
	require.NoError(t, contexts.Save(configDir, &contexts.File{
		CurrentContext: "prod",
		Contexts:       []*contexts.Context{{Name: "prod", CoreURL: "https://foo.auth.entire.io", Handle: "paul", KeychainService: "kc:prod"}},
	}))

	// deadPinningClient: any fetch fails, so a refusal proves the cached entry
	// was inspected and rejected, not re-fetched.
	_, err := ResolveContextForCluster(t.Context(), configDir, cacheDir, "git.evil.com", deadPinningClient(t), t.Logf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside evil.com; refusing")
	require.NotErrorIs(t, err, ErrUnreachable, "the cache entry must be refused before any fetch is attempted")
}

// TestResolve_ForeignIssuerIsNeverCached: a refused document must not land in
// the cache, or the next call would read it back and refuse it again for a
// different reason than the one that matters.
func TestResolve_ForeignIssuerIsNeverCached(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(coresHandler(t, nil, "https://foo.auth.entire.io"))
	defer srv.Close()
	cacheDir := t.TempDir()

	_, err := ResolveContextForCluster(t.Context(), t.TempDir(), cacheDir, "git.evil.com", hostPinningClient(t, srv), t.Logf)
	require.Error(t, err)

	cache, err := discovery.LoadClusterCores(cacheDir)
	require.NoError(t, err)
	if cache != nil {
		_, _, ok := cache.GetEntry("git.evil.com")
		assert.False(t, ok, "a refused discovery document must not be cached")
	}
}
