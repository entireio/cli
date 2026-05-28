package cliauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/cli/internal/entiredb/httputil"
)

// ExchangeRepoScopedToken swaps a login JWT for a short-lived
// repo-scoped JWT at core's canonical /oauth/token endpoint (RFC 8693
// token exchange). Used by CLI git operations (clone, mirror) that
// talk HTTPS directly — git-remote-entire has its own copy for the
// entire:// transport. coreURL is the JWT's issuer (resolved from the
// active context); never read from cfg.EntireCoreBaseURL directly, so
// cross-region setups don't post EU-issued JWTs at a US core.
//
// clusterURL is the data-plane base URL (scheme+host, no trailing
// slash). The audience IS the resource check at the receiving
// entiredb, so a token issued for cluster A fails the audience check
// at cluster B. repoSlug is the full URL slug including the /et/
// prefix (e.g. "/et/widgets/alice-app" with project name 'widgets',
// or legacy "/git/alice/alice-app" for pre-rename rows that used the
// owner segment); it's joined to clusterURL verbatim to form the
// audience.
func ExchangeRepoScopedToken(ctx context.Context, coreURL, loginJWT, repoSlug, action, clusterURL string, httpClient *http.Client) (string, error) {
	coreURL = strings.TrimRight(coreURL, "/")
	clusterURL = strings.TrimRight(clusterURL, "/")
	if coreURL == "" {
		return "", errors.New("no entire-core URL configured for scoped-token exchange; log in with `entire-core auth login` or set ENTIRE_CORE_AUTH_BASE_URL")
	}
	if clusterURL == "" {
		return "", errors.New("repo-token exchange requires a target cluster URL")
	}

	audience := clusterURL + repoSlug
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("subject_token", loginJWT)
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	form.Set("audience", audience)
	form.Set("scope", "repo:"+action)
	form.Set("client_id", "entire-cli")

	slog.Default().LogAttrs(ctx, slog.LevelDebug, "repo-token exchange",
		slog.String("core_url", coreURL),
		slog.String("audience", audience),
		slog.String("action", action))
	token, _, err := httputil.PostOAuthToken(ctx, httpClient, coreURL, form)
	if err != nil {
		return "", fmt.Errorf("repo-token exchange: %w", err)
	}
	return token, nil
}
