package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/httputil"
	"github.com/entireio/cli/internal/entireclient/repocreds"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// ErrRepoTargetUnknown reports that the cluster's STS refused the exchange
// with RFC 8693 `invalid_target`: it has no servable mirror at the
// requested audience. The placement row may well exist but be suspended —
// the data plane's auth gate deliberately hides suspended mirrors behind
// invalid_target rather than disclosing their state (an enumeration guard;
// see entiredb's validateMirrorRepoExchange). Callers that already know the
// mirror exists (e.g. the create flow's clone probe) use this to render an
// actionable message instead of the raw OAuth error.
var ErrRepoTargetUnknown = errors.New("cluster has no servable mirror at this audience")

// repoExchangeTimeout bounds each HTTP call on the mint path: the
// /.well-known cluster discovery at construction (disk-cached after the
// first call) and each /oauth/token exchange.
const repoExchangeTimeout = 30 * time.Second

// resolveContextForCluster is the discovery seam, swapped in tests so they
// don't reach the network. Mirrors clusterdiscovery.ResolveContextForCluster.
var resolveContextForCluster resolveContextFunc = clusterdiscovery.ResolveContextForCluster

// repoExchangeTransportForTest, when non-nil, is the HTTP transport used by
// RepoScopedToken's exchange (and login refresh), so the wire form can be
// asserted without a live core. Production leaves it nil.
var repoExchangeTransportForTest http.RoundTripper

// SetRepoExchangeTransportForTest installs rt as the transport used by
// RepoScopedToken and returns a cleanup function. Test-only.
func SetRepoExchangeTransportForTest(rt http.RoundTripper) func() {
	prev := repoExchangeTransportForTest
	repoExchangeTransportForTest = rt
	return func() { repoExchangeTransportForTest = prev }
}

// RepoTokenSource mints short-lived, repo-scoped access tokens usable
// against one data-plane cluster's git endpoints (clone / fetch /
// info-refs). The data plane's git gate rejects the raw login bearer: it
// only accepts a token whose RFC 8693 audience is
// https://<clusterHost><repoSlug> and whose scope is "repo:<action>".
//
// The login context is resolved once, at construction, the way
// git-remote-entire resolves it: the cluster's
// /.well-known/entire-cluster.json names the core(s) it trusts, and the
// matching local context (active if eligible, else the sole eligible one,
// else an explicit-choice error) supplies the subject token — exchanged at
// that context's core, never at the active context's. Token calls then only
// exchange (through repocreds, the same code path and wire form
// git-remote-entire uses), re-minting an expired login JWT from the stored
// refresh token as needed — so a poller's re-mints don't depend on
// discovery staying reachable.
type RepoTokenSource struct {
	creds *repocreds.Cache
}

// NewRepoTokenSource resolves clusterHost's trusted core and login context
// and returns a source minting tokens for that cluster.
func NewRepoTokenSource(ctx context.Context, clusterHost string) (*RepoTokenSource, error) {
	if clusterHost == "" {
		return nil, errors.New("repo-scoped token exchange requires a target cluster host")
	}

	// Bridge any pre-contexts.json login so the resolver can match it.
	// Best-effort: a migration failure must not block resolution.
	_, _ = MigrateLegacyLoginContext() //nolint:errcheck // best-effort bridge; resolution proceeds regardless

	httpClient := &http.Client{Timeout: repoExchangeTimeout, Transport: repoExchangeTransportForTest}
	clusterCtx, err := resolveContextForCluster(ctx, userdirs.Config(), userdirs.Cache(), clusterHost, httpClient, nil)
	if err != nil {
		return nil, err
	}

	allowInsecure := insecureHTTPEnabled() || isLoopbackHTTP(clusterCtx.CoreURL)
	loginProvider, err := NewRefreshingLoginProvider(clusterCtx, repoExchangeTransportForTest, allowInsecure)
	if err != nil {
		return nil, err
	}

	return &RepoTokenSource{creds: repocreds.New(clusterCtx.CoreURL, "https://"+clusterHost, loginProvider, httpClient)}, nil
}

// Token returns a repo-scoped token for repoSlug (the surface-prefixed repo
// path, e.g. /gh/octocat/hello, joined verbatim to the cluster URL to form
// the audience) and action ("pull" or "push"). Tokens are cached per
// (repoSlug, action) until near expiry.
func (s *RepoTokenSource) Token(ctx context.Context, repoSlug, action string) (string, error) {
	token, err := s.creds.Token(ctx, repoSlug, action)
	if err != nil {
		// invalid_target means the cluster has no servable mirror at this
		// audience (commonly a suspended placement). Surface the sentinel for
		// callers that branch on it, preserving the verbatim OAuth body
		// (second %w) for those that don't.
		var oe *httputil.OAuthError
		if errors.As(err, &oe) && oe.Code == "invalid_target" {
			return "", fmt.Errorf("repo-scoped token exchange: %w: %w", ErrRepoTargetUnknown, err)
		}
		return "", fmt.Errorf("repo-scoped token exchange: %w", err)
	}
	return token, nil
}

// Invalidate drops the cached (repoSlug, action) token so the next Token
// call re-exchanges — for when the data plane rejected it (401) ahead of
// its recorded expiry.
func (s *RepoTokenSource) Invalidate(repoSlug, action string) {
	s.creds.Invalidate(repoSlug, action)
}

// RepoScopedToken is the one-shot form of RepoTokenSource: resolve, mint
// once, discard. Callers that re-mint (e.g. a polling wait) should hold a
// RepoTokenSource instead, so re-mints skip cluster discovery.
func RepoScopedToken(ctx context.Context, clusterHost, repoSlug, action string) (string, error) {
	src, err := NewRepoTokenSource(ctx, clusterHost)
	if err != nil {
		return "", err
	}
	return src.Token(ctx, repoSlug, action)
}
