package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/auth-go/sts"
	"github.com/entireio/cli/cmd/entire/cli/api"
)

// repoExchangeTransportForTest, when non-nil, is used as the sts.Client
// transport by RepoScopedToken instead of the default. Test-only seam so
// the wire form (audience / scope / client_id) can be asserted without a
// live core. Production leaves it nil.
var repoExchangeTransportForTest http.RoundTripper

// SetRepoExchangeTransportForTest installs rt as the transport used by
// RepoScopedToken and returns a cleanup function. Test-only.
func SetRepoExchangeTransportForTest(rt http.RoundTripper) func() {
	prev := repoExchangeTransportForTest
	repoExchangeTransportForTest = rt
	return func() { repoExchangeTransportForTest = prev }
}

// RepoScopedToken exchanges the logged-in user's token for a short-lived,
// repo-scoped access token usable against a data-plane cluster's git
// endpoints (clone / fetch / info-refs).
//
// The data plane's git gate rejects the raw login bearer (HTTP 403): it
// only accepts a token whose RFC 8693 audience is <clusterBaseURL><repoSlug>
// and whose scope is "repo:<action>". This is the same exchange
// git-remote-entire performs internally for the entire:// transport — the
// CLI does it in-process when it needs to read the data plane directly
// (e.g. probing a mirror's clone readiness).
//
//   - clusterBaseURL is the data-plane cluster origin (scheme+host, e.g.
//     https://aws-us-east-2.entire.io); a trailing slash is trimmed.
//   - repoSlug is the full surface-prefixed path (e.g. /gh/octocat/hello
//     or /et/<project>/<repo>), joined to the cluster URL verbatim to form
//     the audience.
//   - action is "pull" for reads or "push" for writes.
//
// The exchange targets the same core endpoint and client identity the CLI
// logged in against (AuthBaseURL + the provider's STS path, client_id
// entire-cli), so a successful login implies a usable exchange. Errors
// surface verbatim from the STS endpoint (e.g. invalid_target when no
// mirror matches the slug+cluster).
//
// The subject token is the stored login access token read directly,
// rather than routed through the refresh-aware tokenmanager. That's
// deliberate for two reasons: (1) `entire login` (device flow) stores only
// a bare access token — no refresh token — so there is nothing the manager
// could refresh that this path can't equally use; an expired login token
// fails both ways. (2) The manager's exchange also emits an RFC 8693
// `resource` parameter alongside `audience`, whereas the data-plane gate
// keys solely on `audience`; going direct keeps the wire form byte-for-byte
// what git-remote-entire (and the standalone entiredb CLI) already send.
// Each call performs a fresh exchange and does not cache — callers that
// poll (e.g. the mirror clone wait) re-invoke on token expiry. If the CLI
// gains refresh tokens, route this through the tokenmanager instead.
func RepoScopedToken(ctx context.Context, clusterBaseURL, repoSlug, action string) (string, error) {
	provider := CurrentProvider()
	if strings.TrimSpace(provider.STSPath) == "" {
		return "", errors.New("repo-scoped token exchange requires a v2 auth host (set ENTIRE_AUTH_BASE_URL to a core that exposes /oauth/token)")
	}

	loginJWT, err := LookupCurrentToken()
	if err != nil {
		return "", fmt.Errorf("read login token: %w", err)
	}
	if loginJWT == "" {
		return "", ErrNotLoggedIn
	}

	clusterBaseURL = strings.TrimRight(clusterBaseURL, "/")
	if clusterBaseURL == "" {
		return "", errors.New("repo-scoped token exchange requires a target cluster URL")
	}
	issuer := api.AuthBaseURL()

	client := &sts.Client{
		Transport:         repoExchangeTransportForTest,
		BaseURL:           issuer,
		Path:              provider.STSPath,
		UserAgent:         provider.ClientID,
		AllowInsecureHTTP: isLoopbackHTTP(issuer) || insecureHTTPOverride.Load(),
	}
	set, err := client.Exchange(ctx, sts.ExchangeRequest{
		SubjectToken:     loginJWT,
		SubjectTokenType: sts.SubjectTokenTypeAccessToken,
		// sts.Client (unlike the tokenmanager) applies no default, so set
		// requested_token_type explicitly to the access-token URI.
		RequestedTokenType: sts.SubjectTokenTypeAccessToken,
		// Audience-only (no Resource): the data plane's git gate keys its
		// repo check on the audience host+slug. Sending a separate resource
		// param risks the server validating a different value than it grants.
		Audience: clusterBaseURL + repoSlug,
		Scope:    "repo:" + action,
		// Public-client identification per RFC 6749 §2.3.1, carried via
		// Extra because the sts package is provider-agnostic.
		Extra: url.Values{"client_id": {provider.ClientID}},
	})
	if err != nil {
		return "", fmt.Errorf("repo-scoped token exchange: %w", err)
	}
	return set.AccessToken, nil
}
