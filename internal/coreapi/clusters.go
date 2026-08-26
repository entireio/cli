package coreapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/internal/entireclient/discovery"
)

// clusterRegistryTimeout bounds the single GET /api/v1/clusters a
// cluster-addressed command makes to confirm its target host. Some deadline is
// required: the coreapi HTTP client sets only a dial timeout, so a
// reachable-but-slow core would otherwise hang the command (a `git clone`
// included) with no output at all.
const clusterRegistryTimeout = 15 * time.Second

// ClusterLister is the control-plane surface a cluster-host check needs: the
// core's authoritative registry of the clusters it fronts. An interface (not
// *Client) so callers outside this package — the git remote helper's auth
// path — can be unit-tested against a fake registry.
type ClusterLister interface {
	ListClusters(ctx context.Context) (*ListClustersOutputBody, error)
}

// ErrClusterNotRegistered marks a cluster host that the consulted core does
// not front. Distinct from a registry that could not be consulted at all:
// callers may want to tell "wrong login" from "control plane unreachable"
// apart, and neither is ever a reason to fall back to asking the target host
// about itself.
var ErrClusterNotRegistered = errors.New("cluster is not in the control plane's cluster registry")

// VerifyClusterRegistered confirms clusterHost names a cluster that the core
// behind c actually fronts, and is the single gate every cluster-addressed
// credential decision passes through (git-remote-entire's auth path,
// coreapi.NewForCluster).
//
// The registry is authoritative and there is no fallback tier. The
// alternative — asking the target host to describe itself via
// /.well-known/entire-cluster.json — takes a self-reported answer at face
// value: the host under scrutiny names the login servers it would like the
// client to trust. The control plane's own catalog is the record of which
// clusters exist and what public host each has, and it is already the
// authoritative source for cell routing (cell_target.go's
// resolveRepoCellTarget), so cluster identity resolves in one place.
//
// coreOrigin names the core consulted, both as the cache key and for the error;
// pass Client.CoreOrigin() (never a re-derivation, which can name a core the
// request never touched).
//
// cacheDir enables the positive-answer cache (discovery.ClusterRegistryCache),
// which keeps a warm clone/fetch/push off the control plane's critical path and
// keeps a known cluster working through a brief core outage. Empty disables it:
// every call then asks the core. Misses and errors are never cached — see the
// cache type's doc comment.
func VerifyClusterRegistered(ctx context.Context, c ClusterLister, cacheDir, coreOrigin, clusterHost string) error {
	if strings.TrimSpace(clusterHost) == "" {
		return errors.New("cluster-addressed request requires a target cluster host")
	}
	if cacheDir != "" && clusterRegistryVerified(cacheDir, coreOrigin, clusterHost) {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, clusterRegistryTimeout)
	defer cancel()

	out, err := c.ListClusters(ctx)
	if err != nil {
		if msg := APIError(err); msg != "" {
			err = errors.New(msg)
		}
		return fmt.Errorf("consult the cluster registry on %s to verify %s: %w", coreOrigin, clusterHost, err)
	}
	if _, ok := MatchClusterByHost(out.Clusters, clusterHost); !ok {
		return fmt.Errorf("%w: %s", ErrClusterNotRegistered, notRegisteredHint(coreOrigin, clusterHost, out.Clusters))
	}
	if cacheDir != "" {
		markClusterRegistryVerified(cacheDir, coreOrigin, clusterHost)
	}
	return nil
}

// clusterRegistryVerified reports a warm cache hit for this (core, cluster)
// pair. A cache that can't be read is simply a miss: the point of the cache is
// latency, so a broken one costs a round trip, never a failure.
func clusterRegistryVerified(cacheDir, coreOrigin, clusterHost string) bool {
	cache, err := discovery.LoadClusterRegistry(cacheDir)
	if err != nil {
		return false
	}
	return cache.Verified(coreOrigin, clusterHost)
}

// markClusterRegistryVerified records a confirmed pairing. Best-effort: a
// cache we can't write just means the next operation re-asks the core, which
// is exactly what happened before the cache existed.
func markClusterRegistryVerified(cacheDir, coreOrigin, clusterHost string) {
	_ = discovery.ModifyClusterRegistry(cacheDir, func(cache discovery.ClusterRegistryCache) error { //nolint:errcheck // see doc comment: a cache write failure must not fail the operation
		cache.MarkVerified(coreOrigin, clusterHost)
		return nil
	})
}

// notRegisteredHint renders the body of the not-registered error: which core
// answered, what it does front, and the one action that can fix it. Built as a
// string (rather than inlined into fmt.Errorf) so the multi-line, punctuated
// operator message stays out of the error-string literal.
func notRegisteredHint(coreOrigin, clusterHost string, clusters []Cluster) string {
	return fmt.Sprintf("%s does not front %s.\nKnown clusters: %s\nIf another saved login fronts it, switch with `entire auth use <context>` (`entire auth contexts` lists them).",
		coreOrigin, clusterHost, describeClusters(clusters))
}

// describeClusters renders the registry's public hosts for the
// not-registered error, so the user can see what the core they're logged into
// does front — typically enough to spot a typo or a staging/prod mixup.
func describeClusters(clusters []Cluster) string {
	hosts := make([]string, 0, len(clusters))
	for _, cl := range clusters {
		host, err := HostFromPublicURL(cl.PublicUrl)
		if err != nil {
			continue
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return "(none)"
	}
	return strings.Join(hosts, ", ")
}

// MatchClusterByHost finds the registry cluster whose public host equals
// clusterHost (case-insensitive). The cluster's apiUrl + jurisdiction are the
// authoritative cell coordinates.
func MatchClusterByHost(clusters []Cluster, clusterHost string) (Cluster, bool) {
	want := strings.ToLower(strings.TrimSpace(clusterHost))
	if want == "" {
		return Cluster{}, false
	}
	for _, cl := range clusters {
		host, err := HostFromPublicURL(cl.PublicUrl)
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(host), want) {
			return cl, true
		}
	}
	return Cluster{}, false
}

// HostFromPublicURL extracts the bare cluster host from a cluster's public_url
// (with or without a scheme) and runs it through ValidateClusterHost, the same
// anti-token-leak guard the positional <cluster-host> arg uses. Kept separate
// so the ListClusters → host mapping is unit-testable without a live catalog.
func HostFromPublicURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty public_url")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse public_url %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("public_url %q has no host", raw)
	}
	// Reject anything beyond scheme://host[:port]. url.Parse demotes the
	// `host@evil.com` userinfo trick into u.User (leaving u.Host=evil.com) and
	// stashes a trailing path in u.Path, neither of which ValidateClusterHost
	// would otherwise see. A bare "/" path is tolerated: publicUrl is a trusted
	// catalog field, and a trailing slash (https://host/) is benign — rejecting
	// it would silently drop the cluster and could leave a picker with no
	// regions. Anything richer (a real path, query, fragment, userinfo) is still
	// refused, since the host flows into clone URLs and the STS audience.
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("public_url %q must be scheme://host[:port] only", raw)
	}
	if err := ValidateClusterHost(u.Host); err != nil {
		return "", err
	}
	return u.Host, nil
}

// clusterHostLabelRe matches one DNS label: alphanumeric, internal hyphens
// allowed, no leading/trailing hyphen.
var clusterHostLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// ValidateClusterHost rejects a cluster host that is anything other than a
// bare DNS name or IP with an optional :port. The host is concatenated as
// "https://"+host into the clone URL and the STS audience
// (entireclient/repocreds), so a value carrying URL metacharacters can redirect
// the request — and the repo-scoped basic-auth token it carries — somewhere
// other than the intended cluster. Classic case:
// `aws-us-east-2.entire.io@evil.com`, which Go's URL parser reads as
// host=evil.com with the real cluster demoted to userinfo, leaking the token
// to evil.com. We parse the host the same way the rest of the code does and
// require it to round-trip to a bare host with no userinfo, path, query, or
// fragment, then confirm the hostname is a valid IP or DNS name. This is
// cheap client-side defense-in-depth and doesn't depend on the server's STS
// invalid_target canonicalization catching the trick.
func ValidateClusterHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return errors.New("cluster host is empty")
	}
	u, err := url.Parse("https://" + host)
	if err != nil {
		return fmt.Errorf("%q is not a valid host", host)
	}
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host != host {
		return fmt.Errorf("%q must be a bare host[:port] (no scheme, userinfo, path, query, or fragment)", host)
	}
	hostname := u.Hostname()
	if net.ParseIP(hostname) != nil {
		return nil
	}
	for _, label := range strings.Split(hostname, ".") {
		if !clusterHostLabelRe.MatchString(label) {
			return fmt.Errorf("%q is not a valid DNS name or IP", host)
		}
	}
	return nil
}
