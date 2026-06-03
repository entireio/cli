package auth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// defaultContextTokenTTL is the encoded keychain expiry used when a login
// token carries no usable exp claim (e.g. an opaque, non-JWT bearer). The
// server is the real authority on validity; this only governs when local
// readers consider the token stale, and we hold no refresh token to act on
// it, so a conservative non-zero value is enough to keep the entry usable.
const defaultContextTokenTTL = time.Hour

// RecordLoginContext records a freshly obtained login token in the
// shared contexts.json credential model: it derives the issuer (core
// URL), handle, and expiry from the token's own claims, stores the token
// in the OS keyring under the entire-core:<issuer> service scheme entiredb
// uses, and writes (or updates) the matching context.
//
// Contexts are keyed by identity (core URL + handle): re-logging into the
// same identity updates its context in place, while a second identity on
// the same core gets its own context (named handle@host) instead of
// clobbering the first.
//
// activate controls current_context: login passes true (the just-completed
// login becomes active, kubectl use-context style); read-time migration
// passes false so it never silently switches the user's active account —
// it still sets current_context when none exists yet.
//
// This is the contexts.json half of login's dual-write: the legacy
// entire-cli/<authBaseURL> keyring entry is still written by the caller so
// the control-plane readers keep working untouched during the transition.
// A login recorded here is visible to entiredb's CLIs (and the in-CLI git
// remote helper) because they share this file and keychain layout.
//
// Returns the context name on success. Errors are returned (not swallowed)
// so the caller can warn; login still succeeds on the legacy entry.
func RecordLoginContext(rawToken string, activate bool) (string, error) {
	claims, err := tokens.ParseClaims(rawToken)
	if err != nil {
		return "", fmt.Errorf("parse login token claims: %w", err)
	}
	coreURL := claims.Issuer
	if coreURL == "" {
		return "", errors.New("login token has no iss claim; cannot derive core URL for a context")
	}
	handle := claims.Handle
	if handle == "" {
		handle = claims.Subject
	}
	if handle == "" {
		return "", errors.New("login token has no handle/sub claim; cannot key the keychain slot")
	}

	keychainService := tokenstore.CoreKeyringService(coreURL)

	expiresIn := int64(defaultContextTokenTTL.Seconds())
	if !claims.ExpiresAt.IsZero() {
		if secs := int64(time.Until(claims.ExpiresAt).Seconds()); secs > 0 {
			expiresIn = secs
		}
	}

	encoded := tokenstore.EncodeTokenWithExpiration(rawToken, expiresIn)
	if err := tokenstore.Set(keychainService, handle, encoded); err != nil {
		return "", fmt.Errorf("store login token in keyring: %w", err)
	}

	var name string
	cfgDir := contexts.DefaultConfigDir()
	if modErr := contexts.Modify(cfgDir, func(f *contexts.File) (bool, error) {
		name = pickContextName(f, coreURL, handle)
		f.Upsert(&contexts.Context{
			Name:            name,
			CoreURL:         coreURL,
			Handle:          handle,
			KeychainService: keychainService,
		})
		if activate || f.CurrentContext == "" {
			f.CurrentContext = name
		}
		return true, nil
	}); modErr != nil {
		return "", fmt.Errorf("write context: %w", modErr)
	}

	return name, nil
}

// pickContextName chooses the contexts.json name for an (coreURL, handle)
// identity within f. An existing context for the same identity keeps its
// name (re-login updates in place). A fresh identity prefers the bare core
// host; if a *different* identity already holds that name, it's qualified
// with the handle (handle@host) so the two don't collide — and, in the
// pathological case that's taken too, a numeric suffix guarantees
// uniqueness.
func pickContextName(f *contexts.File, coreURL, handle string) string {
	for _, c := range f.Contexts {
		if sameIssuer(c.CoreURL, coreURL) && c.Handle == handle {
			return c.Name
		}
	}
	host := contextNameForCoreURL(coreURL)
	if f.Find(host) == nil {
		return host
	}
	qualified := handle + "@" + host
	if f.Find(qualified) == nil {
		return qualified
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", qualified, i)
		if f.Find(candidate) == nil {
			return candidate
		}
	}
}

// sameIssuer compares two core URLs ignoring a trailing slash.
func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// MigrateLegacyLoginContext bridges users who logged in before the
// contexts.json dual-write existed: if the legacy entire-cli/<authBaseURL>
// keyring entry holds a usable JWT and no context yet covers its issuer,
// it records an equivalent context (and keychain entry under the shared
// scheme) so the git remote helper can authenticate without a re-login.
//
// Returns (true, nil) when it created a context. No-ops — returning
// (false, nil) — when there's no legacy token, the token is opaque
// (no derivable issuer), or a context for that issuer already exists.
// Idempotent: safe to call on every helper invocation.
func MigrateLegacyLoginContext() (migrated bool, err error) {
	legacy, err := NewStore().GetToken(api.AuthBaseURL())
	if err != nil {
		return false, fmt.Errorf("read legacy login token: %w", err)
	}
	if legacy == "" {
		return false, nil
	}
	claims, parseErr := tokens.ParseClaims(legacy)
	if parseErr != nil || claims.Issuer == "" {
		// Opaque/unsigned legacy token — can't derive a context from it.
		return false, nil //nolint:nilerr // absence of a JWT issuer is not an error here
	}
	handle := claims.Handle
	if handle == "" {
		handle = claims.Subject
	}
	f, err := contexts.Load(contexts.DefaultConfigDir())
	if err != nil {
		return false, fmt.Errorf("load contexts: %w", err)
	}
	// Skip only when this exact identity is already represented. Keying on
	// issuer alone would skip a legacy bob@core just because alice@core (e.g.
	// from another CLI) already exists, leaving Bob without a context.
	for _, c := range f.Contexts {
		if sameIssuer(c.CoreURL, claims.Issuer) && c.Handle == handle {
			return false, nil
		}
	}
	// activate=false: migrating an old login (e.g. on first `git clone`) must
	// not silently switch the user's active context. RecordLoginContext still
	// sets current_context when none exists yet.
	if _, err := RecordLoginContext(legacy, false); err != nil {
		return false, err
	}
	return true, nil
}

// LoginTokenForContext returns the login JWT stored for c, read from the
// OS keyring slot the context points at. The encoded expiry is stripped;
// the server is the authority on validity and the device-flow login holds
// no refresh token, so an expired token surfaces as a 401 the caller can
// translate into a re-login hint.
func LoginTokenForContext(c *contexts.Context) (string, error) {
	if c == nil {
		return "", errors.New("nil context")
	}
	if c.KeychainService == "" || c.Handle == "" {
		return "", fmt.Errorf("context %q has no keychain slot", c.Name)
	}
	encoded, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil {
		return "", fmt.Errorf("read token for context %q: %w", c.Name, err)
	}
	if encoded == "" {
		return "", fmt.Errorf("no token stored for context %q (run `entire login`)", c.Name)
	}
	token, _ := tokenstore.DecodeTokenWithExpiration(encoded)
	return token, nil
}

// contextNameForCoreURL derives a stable, human-readable context name
// from the issuer URL — its host, matching entiredb's default of naming a
// context after the core it authenticates against. Falls back to the raw
// URL when it can't be parsed.
func contextNameForCoreURL(coreURL string) string {
	if u, err := url.Parse(coreURL); err == nil && u.Host != "" {
		return u.Host
	}
	return coreURL
}
