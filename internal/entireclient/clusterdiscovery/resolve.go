package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// ResolveContextForCluster verifies that the current login can authenticate
// git operations against clusterHost. The cluster advertises its trusted login
// servers through /.well-known/entire-cluster.json; those facts are cached in
// cluster_cores.json. A login issued by any other server is rejected with a
// fresh-login hint. debugf is optional; nil suppresses debug output.
func ResolveContextForCluster(ctx context.Context, configDir, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, error) {
	a, err := resolveClusterAuth(ctx, configDir, cacheDir, clusterHost, false, httpClient, debugf)
	if err != nil {
		return nil, err
	}
	return a.Context, nil
}

// ClusterAuth is ResolveClusterAuth's result: the selected login
// plus the cluster facts a caller needs to mint credentials for it.
type ClusterAuth struct {
	Context *contexts.Context
	// JurisdictionAudience is the cluster's jurisdiction-token audience; empty
	// when the cluster doesn't advertise one.
	JurisdictionAudience string
	// JurisdictionCoreURL is the advertised core minting for that audience —
	// the cross-jurisdiction exchange endpoint.
	JurisdictionCoreURL string
}

// ResolveClusterAuth is ResolveContextForCluster plus the cluster's
// advertised jurisdiction metadata, sharing the same cache and single
// /.well-known fetch. Because its callers cannot proceed without the
// jurisdiction audience, resolution requires one (see resolveCachedCores).
func ResolveClusterAuth(ctx context.Context, configDir, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) (*ClusterAuth, error) {
	return resolveClusterAuth(ctx, configDir, cacheDir, clusterHost, true, httpClient, debugf)
}

// resolveClusterAuth is the shared body of ResolveContextForCluster and
// ResolveClusterAuth: load contexts, resolve the cluster's cores entry,
// select the login.
func resolveClusterAuth(ctx context.Context, configDir, cacheDir, clusterHost string, requireAudience bool, httpClient *http.Client, debugf DebugFunc) (*ClusterAuth, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	// DNS hostnames are case-insensitive, so fold case before the host drives any
	// lookup: the cache key, the /.well-known fetch, and the cores→context match.
	// Without this, `aws-US-east-2.entire.io` and `aws-us-east-2.entire.io`
	// resolve as different hosts and a context determination can fail spuriously.
	clusterHost = normalizeClusterHost(clusterHost)
	f, err := contexts.Load(configDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}

	entry, err := resolveClusterCores(ctx, cacheDir, clusterHost, requireAudience, httpClient, debugf)
	if err != nil {
		return nil, err
	}

	selected, err := selectContext(f, "cluster "+clusterHost, entry.CoreURLs, debugf)
	if err != nil {
		return nil, err
	}
	return &ClusterAuth{
		Context:              selected,
		JurisdictionAudience: entry.JurisdictionAudience,
		JurisdictionCoreURL:  entry.JurisdictionCoreURL,
	}, nil
}

// ResolveClusterCores returns the cluster's discovery entry — the trusted
// control-plane core URLs that front clusterHost plus its advertised
// jurisdiction audience/core — using the same cache-then-/.well-known
// discovery as ResolveContextForCluster (see resolveClusterCores). Exported
// for callers that need the cluster facts without account selection — e.g.
// the ENTIRE_TOKEN path validates that the env token's audience is one of
// the advertised cores before exchanging it, so an unverified JWT can't
// redirect the token exchange to an attacker-chosen host. Its sole caller
// mints jurisdiction tokens, so the audience-requiring cache semantics
// apply (see resolveCachedCores).
func ResolveClusterCores(ctx context.Context, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) (*discovery.CoresEntry, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	return resolveClusterCores(ctx, cacheDir, normalizeClusterHost(clusterHost), true, httpClient, debugf)
}

// normalizeClusterHost folds a cluster host to its canonical form for use as a
// lookup key. DNS is case-insensitive, so two hosts differing only in case (or
// surrounding whitespace) name the same cluster and must resolve identically —
// for the host→cores cache, /.well-known discovery, and context determination.
func normalizeClusterHost(clusterHost string) string {
	return strings.ToLower(strings.TrimSpace(clusterHost))
}

// resolveCachedCores is the shared cache-then-/.well-known resolution behind
// both resolveClusterCores (git clusters) and resolveAPICores (data APIs):
// read the host→cores cache, return it when fresh, otherwise discover live and
// rewrite the cache. A stale-but-present entry is used as a fallback when the
// live fetch fails, so a brief outage doesn't break a host whose cores we
// already knew. load/modify select the cache file; discover wraps the
// host-specific /.well-known fetch (and any host-specific error formatting);
// label names the resource in debug output ("cluster" / "api host").
//
// requireAudience marks callers that cannot proceed without the entry's
// jurisdiction audience (both git auth paths). For them, an entry
// cached before the cluster advertised an audience is treated as stale so
// the upgrade is picked up immediately instead of after the 24h TTL — and
// an audience-less entry is NOT used as the discovery-failure fallback:
// returning it would make the caller misdiagnose a transient discovery
// failure as "this cluster doesn't do jurisdiction tokens".
func resolveCachedCores(
	cacheDir, host, label string,
	requireAudience bool,
	load func(string) (discovery.ClusterCoresCache, error),
	modify func(string, func(discovery.ClusterCoresCache) error) error,
	discover func() (discovery.CoresEntry, error),
	debugf DebugFunc,
) (*discovery.CoresEntry, error) {
	cache, err := load(cacheDir)
	if err != nil {
		// A cache read problem must not block resolution — discover live.
		debugf("%s cache load failed: %v; discovering live", label, err)
		cache = nil
	}

	var stale *discovery.CoresEntry
	if cache != nil {
		if entry, fresh, ok := cache.GetEntry(host); ok {
			preAudience := requireAudience && entry.JurisdictionAudience == ""
			if fresh && !preAudience {
				debugf("%s %s cores from cache: %v", label, host, entry.CoreURLs)
				return entry, nil
			}
			stale = entry
			debugf("%s %s cores cache expired or pre-audience; re-fetching /.well-known", label, host)
		}
	}

	fetched, err := discover()
	if err != nil {
		if stale != nil && (!requireAudience || stale.JurisdictionAudience != "") {
			debugf("%s discovery for %s failed (%v); falling back to stale cached cores %v", label, host, err, stale.CoreURLs)
			return stale, nil
		}
		return nil, err
	}

	if mErr := modify(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.SetEntry(host, fetched)
		return nil
	}); mErr != nil {
		// Non-fatal: we resolved the cores, the next call just re-fetches.
		debugf("%s cache write for %s failed: %v", label, host, mErr)
	}
	return &fetched, nil
}

// resolveClusterCores returns the control-plane core URLs that front
// clusterHost plus its advertised jurisdiction audience/core, from
// cluster_cores.json when fresh, otherwise via a live /.well-known fetch
// (cached, with stale fallback on failure). requireAudience: see
// resolveCachedCores.
func resolveClusterCores(ctx context.Context, cacheDir, clusterHost string, requireAudience bool, httpClient *http.Client, debugf DebugFunc) (*discovery.CoresEntry, error) {
	return resolveCachedCores(cacheDir, clusterHost, "cluster", requireAudience,
		discovery.LoadClusterCores, discovery.ModifyClusterCores,
		func() (discovery.CoresEntry, error) {
			body, err := Discover(ctx, clusterHost, httpClient, debugf)
			if err != nil {
				return discovery.CoresEntry{}, formatDiscoveryError(clusterHost, err)
			}
			return discovery.CoresEntry{
				CoreURLs:             body.CoreURLs,
				JurisdictionAudience: body.JurisdictionAudience,
				JurisdictionCoreURL:  body.JurisdictionCoreURL,
			}, nil
		}, debugf)
}

// selectContext verifies that the one current login is accepted by the
// resource's advertised login servers.
func selectContext(f *contexts.File, subject string, coreURLs []string, debugf DebugFunc) (*contexts.Context, error) {
	current := f.Find(f.CurrentContext)
	if current == nil {
		return nil, errors.New(renderLoginHint(subject, coreURLs))
	}
	for _, coreURL := range coreURLs {
		if strings.TrimRight(current.CoreURL, "/") == strings.TrimRight(coreURL, "/") {
			debugf("%s -> current login", subject)
			return current, nil
		}
	}
	return nil, errors.New(renderLoginHint(subject, coreURLs))
}

// formatDiscoveryError turns a Discover error into the message
// operators have always seen at this layer. Kept here (not on the
// sentinels themselves) so the package's errors stay machine-readable
// while the caller-facing strings remain centralised.
func formatDiscoveryError(clusterHost string, err error) error {
	switch {
	case errors.Is(err, ErrUnreachable):
		return fmt.Errorf("%s doesn't look like a cluster, or it is unreachable: %w", clusterHost, err)
	case errors.Is(err, ErrNoIssuers):
		return fmt.Errorf("cluster %s does not advertise any trusted login servers (HTTP 503 from %s); contact the cluster administrator",
			clusterHost, Path)
	case errors.Is(err, ErrNoCoreURLs):
		return fmt.Errorf("cluster %s advertises no trusted core URLs (empty list at %s); contact the cluster administrator",
			clusterHost, Path)
	default:
		return fmt.Errorf("cluster discovery for %s: %w", clusterHost, err)
	}
}
