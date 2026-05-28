package auth

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/entireio/auth-go/tokenmanager"
	"github.com/entireio/cli/cmd/entire/cli/api"
)

// TokenRequest is the entire-CLI alias of tokenmanager.TokenRequest so
// callers don't have to import the underlying package for the common
// case. The two types are interchangeable.
type TokenRequest = tokenmanager.TokenRequest

// ErrNotLoggedIn re-exports tokenmanager.ErrNotLoggedIn so callers in
// the cli package can errors.Is against it without an extra import.
var ErrNotLoggedIn = tokenmanager.ErrNotLoggedIn

var (
	managerOnce sync.Once
	manager     *tokenmanager.Manager
	errManager  error

	// managerForTest, when non-nil, is returned by defaultManager()
	// instead of constructing the production manager. Guarded by
	// managerTestMu so concurrent tests calling Set/defaultManager
	// don't race. Production code never reads this var.
	managerForTest *tokenmanager.Manager
	managerTestMu  sync.Mutex

	// insecureHTTPOverride records the --insecure-http-auth opt-in.
	// Read once when the package-level manager is built (sync.Once);
	// callers must invoke EnableInsecureHTTP before any TokenForResource
	// call in the same process or the override has no effect. Loopback
	// hosts are always permitted regardless of this flag.
	insecureHTTPOverride atomic.Bool
)

// EnableInsecureHTTP relaxes the package-level manager's HTTPS guard so
// non-loopback http:// resources (and the auth host's STS endpoint) are
// permitted during token resolution. The CLI calls this when the user
// passes --insecure-http-auth to a command that hits the data API on a
// private network (e.g. a split-host local-dev box where both hosts are
// plain HTTP).
//
// Call before any TokenForResource invocation — the manager is built
// lazily on first use and the AllowInsecureHTTP setting is frozen at
// that point.
func EnableInsecureHTTP() {
	insecureHTTPOverride.Store(true)
}

// SetManagerForTest installs mgr as the manager returned by
// defaultManager() and returns a cleanup function. Test-only.
func SetManagerForTest(t interface{ Helper() }, mgr *tokenmanager.Manager) func() {
	t.Helper()
	managerTestMu.Lock()
	prev := managerForTest
	managerForTest = mgr
	managerTestMu.Unlock()
	return func() {
		managerTestMu.Lock()
		managerForTest = prev
		managerTestMu.Unlock()
	}
}

// defaultManager returns the package-level Manager built from this
// CLI's identity (current provider, AuthBaseURL, NewStore service
// name). Constructed lazily on first use so any env-var setup
// (ENTIRE_AUTH_BASE_URL, ENTIRE_AUTH_PROVIDER_VERSION) lands before
// construction. sync.Once means later env-var changes within the same
// process are ignored; tests bypass the singleton via SetManagerForTest.
func defaultManager() (*tokenmanager.Manager, error) {
	managerTestMu.Lock()
	override := managerForTest
	managerTestMu.Unlock()
	if override != nil {
		return override, nil
	}
	managerOnce.Do(func() {
		provider := CurrentProvider()
		issuer := api.AuthBaseURL()
		m, err := tokenmanager.New(tokenmanager.Config{
			Issuer:    issuer,
			ClientID:  provider.ClientID,
			STSPath:   provider.STSPath,
			Store:     NewStore(),
			UserAgent: provider.ClientID,
			Scope:     "cli",
			// Auto-permit loopback http:// for local development. The
			// EnableInsecureHTTP setter adds non-loopback http on top
			// so split-host private-network dev (e.g. http://devbox.internal)
			// works under --insecure-http-auth. Anything else must be
			// https:// — STS ships the user's core token in the request
			// body and would leak in clear otherwise.
			AllowInsecureHTTP: isLoopbackHTTP(issuer) || insecureHTTPOverride.Load(),
		})
		manager = m
		if err != nil {
			errManager = fmt.Errorf("build token manager: %w", err)
		}
	})
	return manager, errManager
}

// TokenForResource returns a bearer token suitable for use against
// resourceBaseURL, performing an RFC 8693 token exchange when the
// stored core token's audience doesn't already cover that resource.
// See tokenmanager.Manager.Token for the full resolution rules.
func TokenForResource(ctx context.Context, resourceBaseURL string) (string, error) {
	m, err := defaultManager()
	if err != nil {
		return "", err
	}
	return m.TokenForResource(ctx, resourceBaseURL) //nolint:wrapcheck // shim returns the lib error verbatim
}

// Token is the full-control entry point. Use TokenForResource for the
// common case; this exists so callers can override the wire-level
// Audience, RequestedTokenType, or Scope per call.
func Token(ctx context.Context, req TokenRequest) (string, error) {
	m, err := defaultManager()
	if err != nil {
		return "", err
	}
	return m.Token(ctx, req) //nolint:wrapcheck // shim returns the lib error verbatim
}

// isLoopbackHTTP reports whether u is an http:// URL pointing at a
// loopback hostname (localhost, 127.0.0.1, ::1). Used to scope the
// "auto-permit insecure HTTP" path on the tokenmanager so production
// misconfigurations fail loudly while loopback-only local-dev flows
// keep working.
func isLoopbackHTTP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
