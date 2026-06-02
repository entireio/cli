package auth

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/entireio/auth-go/tokens"
)

// EnvTokenVar is the environment variable that, when set, bypasses
// contexts.json and the keyring entirely: its value is used verbatim as the
// login JWT for repo-scoped token exchange. This is the CI / workload-identity
// path — a runner injects a short-lived login or sa-session JWT and clones
// without an interactive `entire login`.
const EnvTokenVar = "ENTIRE_TOKEN"

// CoreURLFromEnvToken derives the home-region core URL from an ENTIRE_TOKEN
// JWT's audience claim. Login and sa-session JWTs carry aud=<home-region URL>,
// which is what STS routing keys on — so we read aud, not iss (iss may be a
// regional core that can't mint the cross-region exchange).
//
// The aud claim is URL-shaped (scheme://host[/path]). It may be a single
// string or an array (RFC 7519 §4.1.3); ParseClaims normalises both to a
// slice. The first URL-shaped audience wins. A token with no URL-shaped aud
// is rejected with a clear error rather than silently falling back to context
// resolution, so a misconfigured CI token fails loudly.
func CoreURLFromEnvToken(rawToken string) (string, error) {
	claims, err := tokens.ParseClaims(rawToken)
	if err != nil {
		return "", fmt.Errorf("parse %s claims: %w", EnvTokenVar, err)
	}
	for _, aud := range claims.Audience {
		if u, err := url.Parse(aud); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return strings.TrimRight(aud, "/"), nil
		}
	}
	return "", fmt.Errorf("%s must be a login or sa-session JWT whose aud is the home-region URL; found no URL-shaped audience claim", EnvTokenVar)
}
