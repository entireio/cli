package clusterdiscovery

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// registrableDomain returns the registrable domain (eTLD+1) of host — the
// part one organisation controls — so two hosts can be compared for "same
// operator": foo.auth.entire.io → entire.io, royalcanin.partial.to →
// partial.to, whatever.evil.com → evil.com. It is NOT "the last two labels":
// evil.co.uk → evil.co.uk, which is what stops evil.co.uk from matching
// acme.co.uk.
//
// A port is ignored, and the result is case-folded through
// normalizeClusterHost so it never disagrees with the cache key. Hosts with no
// registrable domain match only themselves: IP literals, single-label names
// (localhost), and bare public suffixes (co.uk), for which
// publicsuffix.EffectiveTLDPlusOne returns an error.
func registrableDomain(host string) string {
	host = normalizeClusterHost(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// An IPv6 literal without a port keeps its brackets past SplitHostPort.
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return domain
}

// requireSameSiteIssuers rejects an advertised issuer list that reaches outside
// the resource's own registrable domain.
//
// SECURITY: this is the trust boundary on discovery. The resource's
// /.well-known document decides which saved logins are eligible, and the
// selected login's JWT is then sent to the resource as the bearer
// (git-remote-entire), or refreshed and presented to it (data API). Nothing
// checks that the resource is entitled to that token. So a hostile
// cluster evil.com advertising `https://foo.auth.entire.io` in core_urls
// would be handed a real entire.io login token — through every selection
// tier, explicit or automatic — and through ENTIRE_TOKEN, whose aud is
// compared against this same list. Requiring every issuer to share the
// resource's registrable domain means a host can only ever be sent a token
// minted by its own operator.
//
// A mismatch is a hard error naming both sides, never a silent filter: an
// emptied list would fall through to the `entire login --server …` hint and
// send the user to log in against the very host that lied. An entry that is
// not a URL with a host fails the same way — it can match no login and is
// evidence of a malformed document, not a login server.
//
// label and host are the resource ("cluster", "evil.com"); coreURLs is its
// advertised core_urls (git) or trusted_issuers (data API).
func requireSameSiteIssuers(label, host string, coreURLs []string) error {
	site := registrableDomain(host)
	for _, coreURL := range coreURLs {
		u, err := url.Parse(strings.TrimSpace(coreURL))
		if err != nil || u.Host == "" {
			return fmt.Errorf("%s %s advertises unusable login server %q; refusing", label, host, coreURL)
		}
		if registrableDomain(u.Host) != site {
			return fmt.Errorf("%s %s advertises login server %s outside %s; refusing",
				label, host, normalizeCoreURL(coreURL), site)
		}
	}
	return nil
}
