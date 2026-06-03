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
// SECURITY: the returned URL becomes the host the env token is POSTed to as a
// subject_token during exchange. ParseClaims does NOT verify the signature, so
// the audience is attacker-controlled if a forged token is injected. This
// function only enforces the *shape* of a safe endpoint (https, bare origin);
// the caller MUST additionally verify the URL is a trusted core for the target
// cluster (see clusterdiscovery.ResolveClusterCores) before exchanging, or a
// forged aud could redirect the token to an arbitrary host.
//
// Structural rules, all required:
//   - the aud is a well-formed absolute URL,
//   - scheme is https (no cleartext token exchange),
//   - it carries a host and no userinfo, path, query, or fragment — entire
//     cores are bare origins (https://core.example.com), so anything richer is
//     either a misconfigured token or an attempt to smuggle a path/redirect.
//
// The aud claim may be a single string or an array (RFC 7519 §4.1.3);
// ParseClaims normalises both to a slice. Non-URL audiences (e.g. an OAuth
// client_id like "entire-cli") are skipped; the first URL-shaped audience is
// validated strictly. A token with no URL-shaped aud is rejected with a clear
// error rather than silently falling back to context resolution.
func CoreURLFromEnvToken(rawToken string) (string, error) {
	claims, err := tokens.ParseClaims(rawToken)
	if err != nil {
		return "", fmt.Errorf("parse %s claims: %w", EnvTokenVar, err)
	}
	for _, aud := range claims.Audience {
		u, perr := url.Parse(aud)
		if perr != nil || u.Scheme == "" {
			// Opaque (non-URL) audience such as an OAuth client_id — skip it.
			continue
		}
		// URL-shaped: enforce the strict origin rules. A URL-shaped-but-invalid
		// aud is a hard error (fail closed), never silently skipped.
		return validateCoreAudience(u)
	}
	return "", fmt.Errorf("%s must be a login or sa-session JWT whose aud is the home-region URL; found no URL-shaped audience claim", EnvTokenVar)
}

// validateCoreAudience enforces that u is a safe entire-core origin and
// returns its canonical form (scheme://host, no trailing slash).
func validateCoreAudience(u *url.URL) (string, error) {
	switch {
	case u.Scheme != "https":
		return "", fmt.Errorf("%s aud %q must use https; refusing to exchange the token over %s", EnvTokenVar, u.Redacted(), u.Scheme)
	case u.Host == "":
		return "", fmt.Errorf("%s aud %q has no host", EnvTokenVar, u.Redacted())
	case u.User != nil:
		return "", fmt.Errorf("%s aud %q must not contain userinfo", EnvTokenVar, u.Redacted())
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("%s aud %q must be a bare origin with no path", EnvTokenVar, u.Redacted())
	case u.RawQuery != "":
		return "", fmt.Errorf("%s aud %q must not contain query parameters", EnvTokenVar, u.Redacted())
	case u.Fragment != "":
		return "", fmt.Errorf("%s aud %q must not contain a fragment", EnvTokenVar, u.Redacted())
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/"), nil
}
