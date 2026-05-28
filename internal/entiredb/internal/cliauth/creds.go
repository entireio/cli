package cliauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/internal/entiredb/client/admincreds"
	"github.com/entireio/cli/internal/entiredb/client/clusterdiscovery"
	"github.com/entireio/cli/internal/entiredb/client/contexts"
	"github.com/entireio/cli/internal/entiredb/client/httpclient"
	"github.com/entireio/cli/internal/entiredb/tokenstore"
)

// httpClientTimeout is the timeout used for HTTP requests in the CLI.
const httpClientTimeout = 30 * time.Second

// NewHTTPClient creates an HTTP client with the given TLS verification
// setting. The transport is wrapped with crossJurisRoundTripper so any
// CLI request automatically participates in the 421-follow +
// cross-juris token-exchange dance. Per-origin token cache lives on
// the returned client's transport — re-using one client across calls
// amortises the exchange across cross-juris ops in the same
// invocation.
func NewHTTPClient(skipTLSVerify bool) *http.Client {
	return &http.Client{
		Timeout:   httpClientTimeout,
		Transport: newCrossJurisRoundTripper(httpclient.NewTransport(skipTLSVerify)),
	}
}

// ResolveContext picks the context to use for cfg's cluster: the
// `cluster_contexts[host]` binding when set, otherwise current_context.
// Returns nil with a directive-style error when contexts.json doesn't
// resolve so callers don't paper over an unauth'd state.
func ResolveContext(cfg Config) (*contexts.Context, error) {
	return ResolveContextForHost(cfg, cfg.CredentialHost())
}

// ResolveContextForHost is the per-cluster-host variant of ResolveContext:
// callers pass the host the user typed on the command line (e.g. the cluster
// parsed out of `<cluster>/<org>/<repo>`) instead of relying on the ambient
// cfg.CredentialHost(). Returns nil with a directive-style error when
// contexts.json doesn't resolve.
//
// Note: this honours contexts.File.Resolve's current_context fallback for
// unknown hosts. New callers that take an explicit cluster host from the
// CLI should use ResolveClusterContext, which hits the cluster's
// /.well-known/entire-cluster.json on an unbound host instead of silently
// reusing the ambient login (which lets a staging context masquerade as a
// prod credential).
func ResolveContextForHost(cfg Config, host string) (*contexts.Context, error) {
	f, err := contexts.Load(cfg.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	c := f.Resolve(host)
	if c == nil {
		return nil, fmt.Errorf("no logged-in context for %s, please login first with 'entire-core auth login' or set ENTIRE_TOKEN", host)
	}
	return c, nil
}

// ResolveClusterContext is the cluster-host-aware resolver: it honours
// an explicit cluster_contexts[host] binding when present, and otherwise
// hits the cluster's /.well-known/entire-cluster.json to discover its
// trusted issuers and match against existing local contexts. A discovery
// match is auto-bound for next time. Returns a fatal-ready error
// (including the login hint) when no local context matches the
// cluster's advertised issuers.
//
// Use this for any CLI command that takes a cluster host on the
// command line (e.g. `entire-repo mirror create … <cluster-host>`,
// repo dial flows). The contrast with ResolveContextForHost is that
// this never silently falls back to current_context — see the package
// docs on clusterdiscovery.ResolveContextForCluster for the rationale.
func ResolveClusterContext(ctx context.Context, cfg Config, clusterHost string, httpClient *http.Client) (*contexts.Context, error) {
	c, err := clusterdiscovery.ResolveContextForCluster(ctx, cfg.ConfigDir, clusterHost, httpClient, Debugf)
	if err != nil {
		return nil, fmt.Errorf("resolve context for cluster %s: %w", clusterHost, err)
	}
	return c, nil
}

// ResolveCoreURL picks the entire-core URL the CLI should hit for STS
// exchange and refresh: the resolved context's CoreURL wins (the JWT's
// signed `iss` claim — authoritative). Falls back to ENTIRE_CORE_AUTH_BASE_URL
// for ENTIRE_TOKEN bypass and pre-login bootstrap, when no context yet
// exists. Trailing slashes normalised away.
//
// Without this, a binding to a context whose CoreURL is `eu` plus
// ENTIRE_CORE_AUTH_BASE_URL=us would post EU-issued JWTs to US — the
// exchange would (correctly) reject them. The context's CoreURL is the
// only URL where the JWT was minted.
func ResolveCoreURL(cfg Config) string {
	if f, err := contexts.Load(cfg.ConfigDir); err == nil {
		if c := f.Resolve(cfg.CredentialHost()); c != nil && c.CoreURL != "" {
			return strings.TrimRight(c.CoreURL, "/")
		}
	}
	return strings.TrimRight(cfg.EntireCoreBaseURL, "/")
}

// GetTokenForHost retrieves the resolved context's access token from the
// keyring. ENTIRE_TOKEN, when set, bypasses the credential store
// entirely — useful when the keyring is unavailable or for CI runners
// injecting a workload JWT directly.
func GetTokenForHost(cfg Config) (string, error) {
	if envToken := os.Getenv("ENTIRE_TOKEN"); envToken != "" {
		return envToken, nil
	}

	c, err := ResolveContext(cfg)
	if err != nil {
		return "", err
	}
	encodedToken, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil {
		return "", fmt.Errorf("failed to get token from keyring: %w", err)
	}
	token, _ := tokenstore.DecodeTokenWithExpiration(encodedToken)
	return token, nil
}

// ResolveAdminToken returns an ops-access JWT for the admin surface. The
// caller may have a logged-in context whose login JWT can be exchanged
// at core, or a cluster-local break-glass JWT stashed by `entiredb admin
// break-glass`, or both. When both are available the operator is
// prompted; selecting break-glass skips the core round-trip entirely.
//
// clusterURL is the cluster's full base URL the issued ops token will be
// bound to — always cfg.EntireBaseURL regardless of which connect helper
// called us. The exchange stamps aud=clusterURL+/admin on the issued
// token, so a token minted for cluster A fails the audience check at
// cluster B. Break-glass tokens are issued by the local cluster against
// its own audience and bypass the exchange — clusterURL is ignored in
// that branch.
func ResolveAdminToken(ctx context.Context, cfg Config, clusterURL string) (string, error) {
	c, ctxErr := tryResolveContext(cfg)
	hasBreakGlass := BreakGlassTokenAvailable(cfg)

	switch {
	case c == nil && !hasBreakGlass:
		return "", fmt.Errorf("no admin credentials for %s; log in with 'entire-core auth login' or run 'entiredb admin break-glass' (%w)", cfg.CredentialHost(), ctxErr)
	case c == nil && hasBreakGlass:
		return ReadBreakGlassToken(cfg)
	case c != nil && !hasBreakGlass:
		loginJWT, err := LoginJWTFor(c)
		if err != nil {
			return "", err
		}
		return ExchangeOpsToken(ctx, cfg, c.CoreURL, loginJWT, clusterURL)
	}

	choice, err := promptCredentialChoice(c)
	if err != nil {
		return "", err
	}
	if choice == "break-glass" {
		return ReadBreakGlassToken(cfg)
	}
	loginJWT, err := LoginJWTFor(c)
	if err != nil {
		return "", err
	}
	return ExchangeOpsToken(ctx, cfg, c.CoreURL, loginJWT, clusterURL)
}

// tryResolveContext is the silent-error variant of ResolveContext for
// ResolveAdminToken's branch logic — we want to distinguish "no context"
// from "load failed" without surfacing the directive-style error when a
// break-glass token would have done the job anyway.
func tryResolveContext(cfg Config) (*contexts.Context, error) {
	f, err := contexts.Load(cfg.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("load contexts: %w", err)
	}
	c := f.Resolve(cfg.CredentialHost())
	if c == nil {
		return nil, errors.New("no context resolves for this cluster")
	}
	return c, nil
}

// BreakGlassTokenAvailable reports whether a break-glass token has been
// stashed for this host. Distinct from ReadBreakGlassToken, which fails
// loudly when missing — this is a peek for the prompt.
func BreakGlassTokenAvailable(cfg Config) bool {
	tok, err := tokenstore.Get(BreakGlassKeyringService(cfg.CredentialHost()), BreakGlassKeyringUser)
	return err == nil && strings.TrimSpace(tok) != ""
}

// LoginJWTFor reads the context's login JWT from the keyring and strips
// the expiration suffix tokenstore stores alongside the raw JWT.
func LoginJWTFor(c *contexts.Context) (string, error) {
	encodedToken, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil {
		return "", fmt.Errorf("failed to get token from keyring: %w", err)
	}
	loginJWT, _ := tokenstore.DecodeTokenWithExpiration(encodedToken)
	return loginJWT, nil
}

// promptCredentialChoice asks the operator to pick between the resolved
// login context and the stashed break-glass token. Returns "break-glass"
// or "context".
func promptCredentialChoice(c *contexts.Context) (string, error) {
	fmt.Println("Available credentials:")
	fmt.Println("  0. !! break-glass !!")
	fmt.Printf("  1. %s (%s)\n", c.Name, c.Handle)
	fmt.Print("Select (0-1): ")

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read input: %w", err)
	}
	var selection int
	if _, err := fmt.Sscanf(strings.TrimSpace(input), "%d", &selection); err != nil {
		return "", fmt.Errorf("invalid selection: %w", err)
	}
	switch selection {
	case 0:
		return "break-glass", nil
	case 1:
		return "context", nil
	default:
		return "", errors.New("selection out of range")
	}
}

// sharedOpsAuth is a process-wide admincreds.Auth instance so repeated
// ExchangeOpsToken calls within one invocation (e.g. admin-wait polling
// loops) reuse the cached ops token instead of round-tripping core each
// tick. Keyed by (coreURL, loginJWT): if either changes mid-process the
// instance is rebuilt.
var (
	sharedOpsAuthMu  sync.Mutex
	sharedOpsAuth    *admincreds.Auth
	sharedOpsAuthKey string
)

// ExchangeOpsToken posts the login JWT to core's STS endpoint and
// returns a fresh ops-access JWT, reusing a process-wide cache so
// subsequent calls in the same invocation don't re-exchange. coreURL is
// the issuer the JWT was minted by — pulled from the resolved
// *contexts.Context, never from cfg.EntireCoreBaseURL, so a
// ENTIRE_CORE_AUTH_BASE_URL pointing at the wrong region can't redirect the
// exchange to a core that won't accept the JWT.
//
// clusterURL is the entiredb cluster's full URL (scheme+host). The
// exchange stamps aud=clusterURL+/admin on the issued JWT so a token
// minted for cluster A fails the audience check at cluster B.
func ExchangeOpsToken(ctx context.Context, cfg Config, coreURL, loginJWT, clusterURL string) (string, error) {
	coreURL = strings.TrimRight(coreURL, "/")
	if coreURL == "" {
		return "", errors.New("no entire-core URL configured for admin commands; log in with `entire-core auth login` or set ENTIRE_CORE_AUTH_BASE_URL")
	}
	if clusterURL == "" {
		return "", errors.New("ops-token exchange requires a target cluster URL (cfg.EntireBaseURL or --node-url)")
	}
	sharedOpsAuthMu.Lock()
	key := coreURL + "|" + clusterURL + "|" + loginJWT
	if sharedOpsAuth == nil || sharedOpsAuthKey != key {
		sharedOpsAuth = admincreds.New(coreURL, loginJWT, clusterURL, admincreds.TokenTypeOpsAccess, NewHTTPClient(cfg.SkipTLSVerify))
		sharedOpsAuthKey = key
	}
	auth := sharedOpsAuth
	sharedOpsAuthMu.Unlock()
	tok, err := auth.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("ops-token exchange: %w", err)
	}
	return tok, nil
}
