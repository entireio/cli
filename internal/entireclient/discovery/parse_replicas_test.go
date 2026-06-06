package discovery

import (
	"slices"
	"testing"
)

func TestParseReplicas(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"empty entries", ",,,", nil},
		{"single", "https://n1.c.to", []string{"https://n1.c.to"}},
		{"multiple", "https://n1.c.to,https://n2.c.to,https://n3.c.to", []string{
			"https://n1.c.to", "https://n2.c.to", "https://n3.c.to",
		}},
		{"whitespace around entries", " https://n1.c.to ,\thttps://n2.c.to\n", []string{
			"https://n1.c.to", "https://n2.c.to",
		}},
		{"skip empty entries", "https://n1.c.to,,https://n2.c.to,", []string{
			"https://n1.c.to", "https://n2.c.to",
		}},
		{"non-default port", "https://n1.c.to:8443,https://n2.c.to", []string{
			"https://n1.c.to:8443", "https://n2.c.to",
		}},
		{"http scheme (sim)", "http://127.0.0.1:10001,http://127.0.0.1:10002", []string{
			"http://127.0.0.1:10001", "http://127.0.0.1:10002",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseReplicas(tt.header)
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseReplicas(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestHostInCluster(t *testing.T) {
	tests := []struct {
		host, cluster string
		want          bool
	}{
		{"n1.eu.partial.to", "eu.partial.to", true},
		{"eu.partial.to", "eu.partial.to", true},
		{"EU.PARTIAL.TO", "eu.partial.to", true},
		{"n1.eu.partial.to", "EU.PARTIAL.TO", true},
		{"evil.com", "eu.partial.to", false},
		{"evileu.partial.to", "eu.partial.to", false}, // suffix but not subdomain
		{"partial.to", "eu.partial.to", false},        // parent domain, not a subdomain
		{"127.0.0.1", "127.0.0.1:9999", true},
		{"other.com", "127.0.0.1:9999", false},
		// Port on the host side too — caller may pass a raw Host header
		// rather than a pre-stripped url.URL.Hostname().
		{"node1.eu.partial.to:8443", "eu.partial.to", true},
		{"node1.eu.partial.to:8443", "eu.partial.to:443", true},
		{"eu.partial.to:8443", "eu.partial.to", true},
		{"evil.com:443", "eu.partial.to:443", false},
		// Specificity floor: a bare public suffix as the cluster host must
		// not wildcard-match all of its subdomains.
		{"anything.io", "io", false},              // *.io is a public suffix — too broad
		{"io", "io", true},                        // exact match is still fine (degenerate)
		{"victim.co.uk", "co.uk", false},          // multi-label public suffix
		{"acquired.entire.io", "entire.io", true}, // registrable domain — wildcard OK
		{"entire.io", "entire.io", true},          // exact
		{"a.b.entire.io", "entire.io", true},      // deeper subdomain
		// IP clusters: exact match only, never wildcard — a host that
		// merely ends with the IP string must not be treated as in-cluster.
		{"evil.127.0.0.1", "127.0.0.1", false},
		{"127.0.0.1", "127.0.0.1", true}, // exact still in-cluster
	}
	for _, tt := range tests {
		got := HostInCluster(tt.host, tt.cluster)
		if got != tt.want {
			t.Errorf("HostInCluster(%q, %q) = %v, want %v", tt.host, tt.cluster, got, tt.want)
		}
	}
}
