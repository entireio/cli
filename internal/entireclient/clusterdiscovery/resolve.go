package clusterdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
//   - Which of the user's accounts to use — whichever the user selected, via
//     `--context`/$ENTIRE_CONTEXT for one invocation or the stored
//     current_context otherwise, and nothing else. The cluster's cores only
//     decide whether that identity is accepted; see requireActiveContext for
//     the policy and why it has no fallback tiers.
//
// In particular we never fall back to an active context whose core does NOT
// front the cluster: the cluster would reject the exchanged token as "unknown
// cluster_host", and silently authenticating a staging identity against a
// prod cluster (or vice versa) is exactly the confusion the /.well-known
// lookup exists to prevent.
//
// debugf is optional; nil suppresses debug output.
func ResolveContextForCluster(ctx context.Context, configDir, cacheDir, clusterHost string, httpClient *http.Client, debugf DebugFunc) (*contexts.Context, error) {
	a, err := resolveClusterAuth(ctx, configDir, cacheDir, clusterHost, false, httpClient, debugf)
	if err != nil {
		return nil, err
	}
	return a.Context, nil
}

// ClusterAuth is ResolveClusterAuth's result: the selected login context
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
// select the login context.
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

	selected, err := requireActiveContext(f, "cluster "+clusterHost, entry.CoreURLs, debugf)
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

// requireActiveContext resolves the login context for a resource from the ACTIVE
// context alone, and is the one place the CLI's account-selection policy lives.
// subject is a noun phrase identifying the resource ("cluster nyc.entire.io" /
// "API host partial.to") used in messages, so the same rule serves the
// git-cluster, data-API, and cell resolvers.
//
// The policy: the user decides which identity acts — `--context` or
// $ENTIRE_CONTEXT for one invocation, else `entire auth use` for the stored
// default. A resource's advertised issuers decide only whether that identity is
// *accepted*, never which one is *chosen*. So this validates, it does not
// choose — hence the name.
//
// Two implicit tiers used to sit underneath: "the sole eligible context", and an
// ambiguity error when several were eligible. Both are gone, because they made
// the acting identity depend on what *else* happened to be stored — adding a
// second login could silently change which account a command ran as, and the
// same command could act as different identities on two machines. For a question
// as consequential as "whose credentials is this running under?", a predictable
// error beats a convenient guess.
//
// See docs/architecture/upstream-host-resolution.md#account-selection.
func requireActiveContext(f *contexts.File, subject string, coreURLs []string, debugf DebugFunc) (*contexts.Context, error) {
	// An explicit --context/$ENTIRE_CONTEXT naming no saved login fails here,
	// before any eligibility talk: "that context doesn't exist" and "that context
	// isn't trusted here" are different mistakes with different fixes.
	sel, err := f.Active()
	if err != nil {
		return nil, err //nolint:wrapcheck // UnknownContextError is already a complete operator message
	}
	if sel.Context != nil && contextEligible(sel.Context, coreURLs) {
		debugf("%s -> %s", subject, describeSelection(sel))
		return sel.Context, nil
	}
	if sel.Context != nil {
		debugf("%s -> %s (%s) is not trusted here", subject, describeSelection(sel), sel.Context.CoreURL)
	}
	return nil, errors.New(renderUnusableActiveContext(subject, sel, eligibleContexts(f, coreURLs), coreURLs))
}

// describeSelection labels a resolved identity for debug output, naming the
// mechanism that chose it so a trace answers "why this login?" and not just
// "which login?".
func describeSelection(sel contexts.Selection) string {
	if sel.Explicit() {
		return fmt.Sprintf("context %s (from %s)", sel.Context.Name, sel.Source)
	}
	return "active context " + sel.Context.Name
}

// contextEligible reports whether c's core is among the resource's advertised
// trusted issuers. It is the single eligibility predicate: both the accept
// decision and the candidate list reported on failure go through it, so the two
// can never disagree about what "eligible" means.
//
// Comparison is trailing-slash- and whitespace-insensitive, matching
// trustedLoginServers' normalisation of the same URLs — a core advertised with
// padding must not be rejected here and then echoed back as trusted. A context
// with no CoreURL is never eligible: it names no issuer to match, and without
// this guard a resource advertising a blank core would match it.
func contextEligible(c *contexts.Context, coreURLs []string) bool {
	// A nil entry comes from a hand-edited or truncated contexts.json (`[null]`),
	// the same trust boundary deleteContextKeychain's blank-audience guard
	// contemplates. It is never eligible, and must not panic: eligibleContexts
	// walks every stored entry on the failure path, and that path runs inside
	// git-remote-entire during `git push`.
	if c == nil {
		return false
	}
	want := normalizeCoreURL(c.CoreURL)
	if want == "" {
		return false
	}
	return slices.ContainsFunc(coreURLs, func(coreURL string) bool {
		return normalizeCoreURL(coreURL) == want
	})
}

// normalizeCoreURL folds a core URL to the form core URLs are compared and
// displayed in: whitespace and trailing slashes are insignificant, so
// `https://core.example/ ` and `https://core.example` are the same issuer. The
// single normaliser behind contextEligible, activeLoginLabel, and
// trustedLoginServers, so the accept decision and every message agree.
func normalizeCoreURL(coreURL string) string {
	return strings.TrimRight(strings.TrimSpace(coreURL), "/")
}

// eligibleContexts returns the saved contexts this resource would accept, for
// reporting only — requireActiveContext never picks from it. Filtering f.Contexts
// once (rather than iterating coreURLs and collecting per-issuer matches) makes
// duplicates structurally impossible, so no de-duplication is needed even when a
// resource advertises the same core twice.
func eligibleContexts(f *contexts.File, coreURLs []string) []*contexts.Context {
	var out []*contexts.Context
	for _, c := range f.Contexts {
		if contextEligible(c, coreURLs) {
			out = append(out, c)
		}
	}
	return out
}

// renderUnusableActiveContext explains why no identity is available, in the
// terms of what the user can actually do about it — a wrong remedy here is a
// dead end. Two independent facts pick the message: whether an identity resolved
// at all (is there a login to report as rejected?) and whether any saved login
// is eligible (is the fix a local switch, or a new login?).
//
// Naming the login matters because "not logged in" is actively misleading for
// someone who IS logged in, just to a federation this resource doesn't trust; it
// sends them to `entire login`, which reproduces the same failure. The
// no-identity cases must NOT use that phrasing, which is why they get their own
// first lines rather than an interpolated-away clause.
//
// The remedy also tracks where the identity came from: someone who passed
// `--context` needs to change that argument, not run `auth use`, which would
// leave the flag still overriding it on the next run.
//
// Returns a string, matching renderLoginHint and leaving the single errors.New
// to the caller.
func renderUnusableActiveContext(subject string, sel contexts.Selection, eligible []*contexts.Context, coreURLs []string) string {
	names := strings.Join(contextNames(eligible), ", ")
	switchHint := "Switch with `entire auth use <context>`, then re-run your command."
	if sel.Explicit() {
		switchHint = fmt.Sprintf("Name one with `--context <context>` (or %s), then re-run your command.", contexts.EnvContextVar)
	}
	switch {
	case sel.Context == nil && len(eligible) > 0:
		// current_context unset or dangling, but a saved login would work. An
		// explicit selection can't land here: it either matched or already errored.
		return fmt.Sprintf("no active auth context for %s.\nThese saved logins can authenticate it: %s\n%s",
			subject, names, switchHint)
	case sel.Context == nil:
		return renderLoginHint(subject, coreURLs)
	case len(eligible) > 0:
		return fmt.Sprintf("%s does not accept %s.\nThese saved logins can authenticate it: %s\n%s",
			subject, loginLabel(sel), names, switchHint)
	default:
		return fmt.Sprintf("%s does not accept %s, and no other saved login does either.\n%s",
			subject, loginLabel(sel), renderLoginInstruction(coreURLs))
	}
}

// loginLabel names the rejected login and how it was chosen — "your active
// login" reads as a mistake in stored state, which is wrong when the user named
// the context on this very command line.
//
// The core URL degrades away when the metadata carries none, rather than
// emitting a dangling `()`. A core-less context is never eligible, so it always
// arrives here.
func loginLabel(sel contexts.Selection) string {
	role := "your active login"
	if sel.Explicit() {
		role = "the login selected by " + sel.Source
	}
	if core := normalizeCoreURL(sel.Context.CoreURL); core != "" {
		return fmt.Sprintf("%s %q (%s)", role, sel.Context.Name, core)
	}
	return fmt.Sprintf("%s %q", role, sel.Context.Name)
}

// contextNames returns the contexts' names, sorted so error messages are stable
// across saves and across runs.
func contextNames(cs []*contexts.Context) []string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	slices.Sort(names)
	return names
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
