package auth

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/entireio/auth-go/tokens"
)

// EnvTokenVar is the environment variable that, when set, bypasses
// contexts.json and the keyring entirely: its value is used verbatim as the
// bearer for control-plane and git data-plane requests. This is the CI /
// workload-identity path — a runner injects a short-lived login or sa-session
// JWT and clones without an interactive `entire login`. The explicit
// `entire auth token --jurisdiction` command remains a separate path and uses
// the value as the subject of its requested jurisdiction-token exchange.
const EnvTokenVar = "ENTIRE_TOKEN"

// ParseEnvToken is the single owner of the ENTIRE_TOKEN validation sequence
// shared by coreapi.New's bypass and `entire auth status`: it trims the raw
// value, enforces fail-closed that it is non-blank, and derives the control-
// plane core origin from its aud via CoreURLFromEnvToken. Callers pass the raw
// env value (presence is the caller's LookupEnv decision) and send the returned
// token verbatim as the bearer to coreURL. A blank or aud-less value is an
// error, never a silent fall-back to context resolution.
func ParseEnvToken(raw string) (coreURL, token string, err error) {
	token = strings.TrimSpace(raw)
	if token == "" {
		return "", "", fmt.Errorf("%s is set but blank", EnvTokenVar)
	}
	coreURL, err = CoreURLFromEnvToken(token)
	if err != nil {
		return "", "", err
	}
	return coreURL, token, nil
}

// CoreURLFromEnvToken derives the home-region core URL from an ENTIRE_TOKEN
// JWT's audience claim. Login and sa-session JWTs carry aud=<home-region URL>,
// so we read aud, not iss (iss may be a different regional core).
//
// SECURITY: ParseClaims does NOT verify the signature, so the audience is
// attacker-controlled if a forged token is injected. This function only
// enforces the *shape* of a safe core origin (https, bare origin). The git
// helper uses the result only after checking it against the target cluster's
// advertised CoreURLs, then sends the env token directly to the data plane.
// Control-plane clients use the result as their bearer target, while the
// explicit `entire auth token --jurisdiction` path uses it as the STS host for
// that command's requested exchange.
//
// Structural rules, all required:
//   - the aud is a well-formed absolute URL,
//   - scheme is https (no cleartext token transmission),
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
