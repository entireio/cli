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

	return selectContext(f, clusterHost, coreURLs, debugf)
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

// resolveClusterCores returns the control-plane core URLs that front
// clusterHost, from cluster_cores.json when fresh, otherwise via a live
// /.well-known fetch (which is then cached). A stale-but-present cache
// entry is used as a fallback when the live fetch fails, so a brief
// cluster outage doesn't break an operation whose cores we already knew.
func resolveClusterCores(ctx context.Context, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) ([]string, error) {
	cache, err := discovery.LoadClusterCores(cacheDir)
	if err != nil {
		// A cache read problem must not block resolution — discover live.
		debugf("cluster-cores cache load failed: %v; discovering live", err)
		cache = nil
	}

	var stale []string
	if cache != nil {
		if urls, fresh, ok := cache.Get(clusterHost); ok {
			if fresh {
				debugf("cluster %s cores from cache: %v", clusterHost, urls)
				return urls, nil
			}
			stale = urls
			debugf("cluster %s cores cache expired; re-fetching /.well-known", clusterHost)
		}
	}

	body, err := Discover(ctx, clusterHost, httpClient, debugf)
	if err != nil {
		if stale != nil {
			debugf("discovery for %s failed (%v); falling back to stale cached cores %v", clusterHost, err, stale)
			return stale, nil
		}
		return nil, formatDiscoveryError(clusterHost, err)
	}

	if mErr := discovery.ModifyClusterCores(cacheDir, func(c discovery.ClusterCoresCache) error {
		c.Set(clusterHost, body.CoreURLs)
		return nil
	}); mErr != nil {
		// Non-fatal: we resolved the cores, the next call just re-fetches.
		debugf("cluster-cores cache write for %s failed: %v", clusterHost, mErr)
	}
	return body.CoreURLs, nil
}

// selectContext applies the account-selection rules over the cluster's
// advertised cores. See ResolveContextForCluster for the rationale.
func selectContext(f *contexts.File, clusterHost string, coreURLs []string, debugf DebugFunc) (*contexts.Context, error) {
	eligible := eligibleContexts(f, coreURLs)

	// 1. Active context wins when it's eligible for this cluster.
	if current := f.Find(f.CurrentContext); current != nil {
		for _, c := range eligible {
			if c.Name == current.Name {
				debugf("cluster %s -> active context %s", clusterHost, current.Name)
				return current, nil
			}
		}
	}

	// 2. Otherwise the eligible set decides.
	switch len(eligible) {
	case 0:
		return nil, errors.New(RenderLoginHint(clusterHost, coreURLs))
	case 1:
		debugf("cluster %s -> sole eligible context %s", clusterHost, eligible[0].Name)
		return eligible[0], nil
	default:
		return nil, ambiguousContextError(clusterHost, eligible)
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
// authenticate against the cluster and none is active. We refuse to guess —
// the user picks explicitly. Names are sorted so the message is stable.
func ambiguousContextError(clusterHost string, eligible []*contexts.Context) error {
	names := make([]string, len(eligible))
	for i, c := range eligible {
		names[i] = c.Name
	}
	sort.Strings(names)
	return fmt.Errorf("multiple login contexts can authenticate against cluster %s (%s); choose one with `entire auth use <context>` and re-run",
		clusterHost, strings.Join(names, ", "))
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
