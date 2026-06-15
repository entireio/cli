package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/discovery"
)

// ResolveContextForCluster picks the local login context to authenticate
// git operations against clusterHost.
//
// It separates two concerns that used to be conflated in a single
// cluster→context binding:
//
//   - Which control plane(s) front the cluster — an objective infra fact.
//     Discovered from the cluster's /.well-known/entire-cluster.json and
//     cached in cluster_cores.json (see discovery.ClusterCoresCache) with
//     a long TTL, since a cluster's home core is near-static. On a cache
//     miss or expiry we re-fetch; if the re-fetch fails we fall back to
//     the stale cached cores rather than break the op.
//
//   - Which of the user's accounts to use — recomputed every call from the
//     live contexts, never persisted. So a user with several accounts is
//     never silently pinned to one identity.
//
// Account selection (selectContext):
//
//  1. If the active context (current_context) is issued by one of the
//     cluster's cores, use it. This is the explicit lever: `entire auth
//     use <name>` chooses the identity for every cluster that context's
//     core fronts.
//  2. Otherwise gather every local context eligible for the cluster (its
//     CoreURL is among the advertised cores):
//     - exactly one  → use it (the common single-account case);
//     - none         → error with the login hint listing the cluster's cores;
//     - more than one → error asking the user to pick with `entire auth use`,
//     rather than silently guessing an account.
//
// We never fall back to an active context whose core does NOT front the
// cluster: the cluster would reject the exchanged token as "unknown
// cluster_host", and silently authenticating a staging identity against a
// prod cluster (or vice versa) is exactly the confusion the /.well-known
// lookup exists to prevent.
//
// debugf is optional; nil suppresses debug output.
func ResolveContextForCluster(ctx context.Context, configDir, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	f, err := contexts.Load(configDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}

	coreURLs, err := resolveClusterCores(ctx, cacheDir, clusterHost, httpClient, debugf)
	if err != nil {
		return nil, err
	}

	return selectContext(f, "cluster "+clusterHost, coreURLs, debugf)
}

// ResolveClusterCores returns the trusted control-plane core URLs that
// front clusterHost, using the same cache-then-/.well-known discovery as
// ResolveContextForCluster (see resolveClusterCores). Exported for callers
// that need the cluster's trusted-core set without account selection — e.g.
// the ENTIRE_TOKEN path validates that the env token's audience is one of
// these before exchanging it, so an unverified JWT can't redirect the
// token exchange to an attacker-chosen host.
func ResolveClusterCores(ctx context.Context, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) ([]string, error) {
	if debugf == nil {
		debugf = func(string, ...any) {}
	}
	return resolveClusterCores(ctx, cacheDir, clusterHost, httpClient, debugf)
}

// resolveCachedCores is the shared cache-then-/.well-known resolution behind
// both resolveClusterCores (git clusters) and resolveAPICores (data APIs):
// read the host→cores cache, return it when fresh, otherwise discover live and
// rewrite the cache. A stale-but-present entry is used as a fallback when the
// live fetch fails, so a brief outage doesn't break a host whose cores we
// already knew. load/modify select the cache file; discover wraps the
// host-specific /.well-known fetch (and any host-specific error formatting);
// label names the resource in debug output ("cluster" / "api host").
func resolveCachedCores(
	cacheDir, host, label string,
	load func(string) (discovery.ClusterCoresCache, error),
	modify func(string, func(discovery.ClusterCoresCache) error) error,
	discover func() ([]string, error),
	debugf DebugFunc,
) ([]string, error) {
	cache, err := load(cacheDir)
	if err != nil {
		// A cache read problem must not block resolution — discover live.
		debugf("%s cache load failed: %v; discovering live", label, err)
		cache = nil
	}

	var stale []string
	if cache != nil {
		if urls, fresh, ok := cache.Get(host); ok {
			if fresh {
				debugf("%s %s cores from cache: %v", label, host, urls)
				return urls, nil
			}
			stale = urls
			debugf("%s %s cores cache expired; re-fetching /.well-known", label, host)
		}
	}

	cores, err := discover()
	if err != nil {
		if stale != nil {
			debugf("%s discovery for %s failed (%v); falling back to stale cached cores %v", label, host, err, stale)
			return stale, nil
		}
		return nil, err
	}

	if mErr := modify(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.Set(host, cores)
		return nil
	}); mErr != nil {
		// Non-fatal: we resolved the cores, the next call just re-fetches.
		debugf("%s cache write for %s failed: %v", label, host, mErr)
	}
	return cores, nil
}

// resolveClusterCores returns the control-plane core URLs that front
// clusterHost, from cluster_cores.json when fresh, otherwise via a live
// /.well-known fetch (cached, with stale fallback on failure).
func resolveClusterCores(ctx context.Context, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) ([]string, error) {
	return resolveCachedCores(cacheDir, clusterHost, "cluster",
		discovery.LoadClusterCores, discovery.ModifyClusterCores,
		func() ([]string, error) {
			body, err := Discover(ctx, clusterHost, httpClient, debugf)
			if err != nil {
				return nil, formatDiscoveryError(clusterHost, err)
			}
			return body.CoreURLs, nil
		}, debugf)
}

// selectContext applies the account-selection rules over a resource's
// advertised trusted issuers. subject is a noun phrase identifying the
// resource ("cluster nyc.entire.io" / "API host partial.to") used in
// messages, so the same rules serve both the git-cluster and data-API
// resolvers. See ResolveContextForCluster for the rationale.
func selectContext(f *contexts.File, subject string, coreURLs []string, debugf DebugFunc) (*contexts.Context, error) {
	eligible := eligibleContexts(f, coreURLs)

	// 1. Active context wins when it's eligible for this resource.
	if current := f.Find(f.CurrentContext); current != nil {
		for _, c := range eligible {
			if c.Name == current.Name {
				debugf("%s -> active context %s", subject, current.Name)
				return current, nil
			}
		}
	}

	// 2. Otherwise the eligible set decides.
	switch len(eligible) {
	case 0:
		return nil, errors.New(renderLoginHint(subject, coreURLs))
	case 1:
		debugf("%s -> sole eligible context %s", subject, eligible[0].Name)
		return eligible[0], nil
	default:
		return nil, ambiguousContextError(subject, eligible)
	}
}

// eligibleContexts returns the local contexts whose core is among coreURLs,
// de-duplicated by name. Order is unspecified — callers either use the sole
// element or report the whole set, never index [0] as a silent winner.
func eligibleContexts(f *contexts.File, coreURLs []string) []*contexts.Context {
	seen := make(map[string]bool)
	var out []*contexts.Context
	for _, coreURL := range coreURLs {
		for _, c := range f.ContextsForIssuer(coreURL) {
			if !seen[c.Name] {
				seen[c.Name] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// ambiguousContextError is returned when more than one local context could
// authenticate against the resource and none is active. We refuse to guess —
// the user picks explicitly. Names are sorted so the message is stable.
func ambiguousContextError(subject string, eligible []*contexts.Context) error {
	names := make([]string, len(eligible))
	for i, c := range eligible {
		names[i] = c.Name
	}
	sort.Strings(names)
	return fmt.Errorf("multiple login contexts can authenticate against %s (%s); choose one with `entire auth use <context>` and re-run",
		subject, strings.Join(names, ", "))
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
