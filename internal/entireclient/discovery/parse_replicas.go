package discovery

import (
	"net"
	"strings"
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
// sensitive-header preservation across redirects: an in-cluster hop is
// safe to carry Authorization, anything else must be treated as a new
// trust boundary.
func HostInCluster(host, cluster string) bool {
	h := stripPort(strings.ToLower(host))
	c := stripPort(strings.ToLower(cluster))
	return h == c || strings.HasSuffix(h, "."+c)
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
