package auth

import (
	"net/url"
	"sync/atomic"

	"github.com/entireio/auth-go/tokenmanager"
)

const schemeHTTP = "http"

// ErrNotLoggedIn re-exports tokenmanager.ErrNotLoggedIn so callers in
// the cli package can errors.Is against it without an extra import.
var ErrNotLoggedIn = tokenmanager.ErrNotLoggedIn

// insecureHTTPOverride records the --insecure-http-auth opt-in. Read by
// every per-context token manager as it is built; call EnableInsecureHTTP
// before resolving tokens in the same process or the override has no
// effect. Loopback hosts are always permitted regardless of this flag.
var insecureHTTPOverride atomic.Bool

// EnableInsecureHTTP relaxes the token managers' HTTPS guard so
// non-loopback http:// resources (and the login server's STS endpoint) are
// permitted during token resolution. The CLI calls this when the user
// passes --insecure-http-auth to a command that hits the data API on a
// private network (e.g. a split-host local-dev box where both hosts are
// plain HTTP).
func EnableInsecureHTTP() {
	insecureHTTPOverride.Store(true)
}

// insecureHTTPEnabled reports whether EnableInsecureHTTP was called. The
// per-context providers read this so --insecure-http-auth still relaxes
// the HTTPS guard for a non-loopback http:// core.
func insecureHTTPEnabled() bool {
	return insecureHTTPOverride.Load()
}

// isLoopbackHTTP reports whether u is an http:// URL pointing at a
// loopback hostname (localhost, 127.0.0.1, ::1). Used to scope the
// "auto-permit insecure HTTP" path on the tokenmanager so production
// misconfigurations fail loudly while loopback-only local-dev flows
// keep working.
func isLoopbackHTTP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != schemeHTTP {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
