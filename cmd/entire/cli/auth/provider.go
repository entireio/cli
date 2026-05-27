package auth

import (
	"os"
	"strings"
	"sync"
)

// ProviderVersionEnvVar selects which OAuth surface this CLI talks to.
//
// Recognised values:
//
//   - "v1" (or unset / unrecognised) — current device-flow surface
//   - "v2"                            — next-generation device-flow surface
//
// Read once at process startup via CurrentProvider; later flips within
// the same process are intentionally ignored. Tests inject via
// SetProviderForTest rather than mutating the env mid-run.
const ProviderVersionEnvVar = "ENTIRE_AUTH_PROVIDER_VERSION"

// Provider captures the per-surface bits of OAuth wiring.
//
// STSPath is the RFC 8693 token-exchange endpoint. v1 is the legacy
// single-host surface where the auth and data API live at the same
// origin; the same-host shortcut in tokenmanager.Token always wins and
// STS is never invoked, so v1.STSPath is left empty. v2 exposes a
// dedicated STS path because it's used in split-host deployments
// (e.g. us.auth.partial.to mints, partial.to consumes).
//
// AuthTokensPath is the base path for the auth-tokens management
// endpoint family (list / revoke). Routed at the api.Client layer via
// (*api.Client).WithAuthTokensPath so the provider table is the single
// source of truth — no env-var duplication between auth/ and api/.
type Provider struct {
	ClientID       string
	DeviceCodePath string
	TokenPath      string
	STSPath        string
	AuthTokensPath string
}

var providers = map[string]Provider{
	"v1": { //nolint:gosec // OAuth client_id and endpoint paths, not credentials
		ClientID:       "entire-cli",
		DeviceCodePath: "/oauth/device/code",
		TokenPath:      "/oauth/token",
		AuthTokensPath: "/api/v1/auth/tokens",
	},
	"v2": { //nolint:gosec // OAuth client_id and endpoint paths, not credentials
		// Matches an OIDC-standard auth server's discovery doc — confirmed
		// against us.auth.partial.to's /.well-known/openid-configuration.
		// Device authorization, token poll, and RFC 8693 exchange all hit
		// the standard endpoints; grant_type differentiates token vs
		// exchange at the shared /oauth/token endpoint.
		ClientID:       "entire-cli",
		DeviceCodePath: "/device_authorization",
		TokenPath:      "/oauth/token",
		STSPath:        "/oauth/token",
		// API token management lives on the data API (not the auth host).
		// auth.go / logout.go pass api.AuthBaseURL() for the keyring key,
		// but the AuthTokensPath calls should route to api.BaseURL() in
		// split-host setups — see TODO in auth.go's newAuthHostAPIClient.
		AuthTokensPath: "/api/v1/auth/tokens",
	},
}

// resolveProvider returns the Provider matching version. Defaulting
// (rather than erroring) on unrecognised values keeps old binaries safe
// if a future v3 ever lands. Pure function — no env reads — so unit
// tests can exercise the routing table without env-var gymnastics.
func resolveProvider(version string) Provider {
	switch strings.TrimSpace(version) {
	case "v2":
		return providers["v2"]
	default:
		return providers["v1"]
	}
}

var (
	providerOnce     sync.Once
	resolvedProvider Provider

	// providerForTest, when non-nil, short-circuits CurrentProvider so
	// tests can install a specific Provider without racing the
	// process-wide sync.Once (which freezes the first observation
	// forever). Mutated only via SetProviderForTest. Production code
	// never reads this var.
	providerForTest *Provider
	providerTestMu  sync.Mutex
)

// CurrentProvider returns the active Provider for this process.
//
// Resolution: read ENTIRE_AUTH_PROVIDER_VERSION exactly once on first
// call, freeze the result, and return the same Provider on every
// subsequent call. Tests that need a different provider must use
// SetProviderForTest before any auth call constructs the singleton.
func CurrentProvider() Provider {
	providerTestMu.Lock()
	override := providerForTest
	providerTestMu.Unlock()
	if override != nil {
		return *override
	}
	providerOnce.Do(func() {
		resolvedProvider = resolveProvider(os.Getenv(ProviderVersionEnvVar))
	})
	return resolvedProvider
}

// SetProviderForTest installs p as the Provider returned by
// CurrentProvider for the duration of the test, and registers a
// t.Cleanup to remove the override. Test-only.
//
// Takes a tiny interface rather than *testing.T so production builds
// don't import testing.
func SetProviderForTest(t interface {
	Helper()
	Cleanup(f func())
}, p Provider) {
	t.Helper()
	providerTestMu.Lock()
	prev := providerForTest
	providerForTest = &p
	providerTestMu.Unlock()
	t.Cleanup(func() {
		providerTestMu.Lock()
		providerForTest = prev
		providerTestMu.Unlock()
	})
}
