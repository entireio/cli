package discovery

import (
	"net"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// ParseReplicas parses the X-Entire-Replicas header value into a list of
// replica URIs. The header is a comma-separated list of URIs; surrounding
// whitespace on each entry is trimmed and empty entries are skipped. An empty
// or missing header yields a nil slice.
func ParseReplicas(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	uris := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			uris = append(uris, p)
		}
	}
	if len(uris) == 0 {
		return nil
	}
	return uris
}

// HostInCluster reports whether host equals the cluster's entry domain or
// is a subdomain of it. Both values are compared by hostname only — any
// port is stripped from either side before comparison. Used to scope
// sensitive-header preservation across redirects and the replica trust
// boundary: an in-cluster hop is safe to carry Authorization, anything
// else must be treated as a new trust boundary.
//
// The subdomain (wildcard) match is only honored when the cluster host is
// strictly more specific than its own public suffix — see
// clusterAllowsSubdomains. Without that floor a cluster host of "io" or
// "co.uk" (a bare eTLD) would make every subdomain of an entire TLD
// "in cluster", so a relinquished or misconfigured subdomain anywhere
// under that suffix could inherit credential-carry trust.
func HostInCluster(host, cluster string) bool {
	h := stripPort(strings.ToLower(host))
	c := stripPort(strings.ToLower(cluster))
	if c == "" {
		return false
	}
	if h == c {
		return true
	}
	if !clusterAllowsSubdomains(c) {
		return false
	}
	return strings.HasSuffix(h, "."+c)
}

// clusterAllowsSubdomains reports whether cluster is specific enough that
// treating its subdomains as same-cluster is safe: it must be strictly
// more specific than its own public suffix (a registrable domain or
// deeper), never a bare public suffix like "io"/"com"/"co.uk" and never a
// single-label name (which is its own public suffix).
//
// IP literals are rejected outright: an IP has no subdomain semantics, so
// only the exact-host match in HostInCluster applies. Allowing the
// wildcard for an IP cluster would let a replica URL like
// https://evil.127.0.0.1 string-suffix-match the cluster IP and wrongly
// inherit credential-carry trust.
//
// cluster is expected to already be lowercased and port-stripped.
func clusterAllowsSubdomains(cluster string) bool {
	if net.ParseIP(cluster) != nil {
		return false
	}
	if !strings.Contains(cluster, ".") {
		return false
	}
	// PublicSuffix returns the eTLD ("io" for "entire.io", "co.uk" for
	// "x.co.uk"). For domains absent from the list the prevailing rule is
	// "*", yielding the rightmost label — so a bare made-up TLD is also
	// rejected as too broad. The cluster is registrable (safe) only when
	// it extends beyond that suffix.
	suffix, _ := publicsuffix.PublicSuffix(cluster)
	return cluster != suffix
}

// stripPort returns s with any trailing :port removed. If s does not parse as
// host:port (e.g. bare hostname, IPv6 without brackets), it's returned
// unchanged.
func stripPort(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}
