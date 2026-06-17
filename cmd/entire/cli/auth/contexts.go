package auth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// defaultContextTokenTTL is the encoded keychain expiry used when a login
// JWT carries no usable exp claim. The server is the real authority on
// validity; this only governs when local readers consider the token stale,
// so a conservative non-zero value is enough to keep the entry usable.
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
// activate controls current_context: true makes the just-completed login
// active (kubectl use-context style); false records it without switching
// the user's active account, though it still sets current_context when
// none exists yet.
//
// This is the CLI's only credential write: a login recorded here is what
// every consumer resolves against — the control plane, the data API, the
// in-CLI git remote helper, and entiredb's CLIs, which share this file and
// keychain layout.
//
// Returns the context name on success.
func RecordLoginContext(rawToken, refreshToken string, activate bool) (string, error) {
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

	// The refresh token lives in the paired "<service>:refresh" slot (raw,
	// no expiry suffix). Clear any prior one when this login carries none,
	// so a stale token from an earlier session can't later be replayed
	// against the server's single-use rotation and revoke the family.
	//
	// Write the refresh slot BEFORE the access token, matching
	// contextTokenStore.SaveTokens: a partial write must never leave a fresh
	// access token paired with a stale refresh token left over from an
	// earlier login. Refresh-first means a failed refresh write aborts before
	// the access token is touched (old pair preserved), rather than committing
	// a new access JWT against a dead refresh token.
	refreshSlot := tokenstore.RefreshService(keychainService)
	if refreshToken != "" {
		if err := tokenstore.Set(refreshSlot, handle, refreshToken); err != nil {
			return "", fmt.Errorf("store refresh token in keyring: %w", err)
		}
	} else {
		_ = tokenstore.Delete(refreshSlot, handle) //nolint:errcheck // best-effort cleanup of a stale refresh token
	}

	encoded := tokenstore.EncodeTokenWithExpiration(rawToken, expiresIn)
	if err := tokenstore.Set(keychainService, handle, encoded); err != nil {
		return "", fmt.Errorf("store login token in keyring: %w", err)
	}

	var name string
	cfgDir := userdirs.Config()
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
