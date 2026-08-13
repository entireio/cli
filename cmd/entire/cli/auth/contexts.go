package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// defaultLoginTokenTTL is the encoded keychain expiry used when a login
// JWT carries no usable exp claim. The server is the real authority on
// validity; this only governs when local readers consider the token stale,
// so a conservative non-zero value is enough to keep the entry usable.
const defaultLoginTokenTTL = time.Hour

// ErrCredentialStoreWrite marks a failure writing tokens to the configured
// credential backend (OS keyring or file store), as opposed to claim
// validation or login-metadata failures. Login UX branches on it via
// errors.Is to decide whether pointing the user at the file token store
// would actually help.
var ErrCredentialStoreWrite = errors.New("credential store write failed")

// credStoreWriteError tags an underlying store error with
// ErrCredentialStoreWrite without changing its message.
type credStoreWriteError struct{ inner error }

func (e *credStoreWriteError) Error() string   { return e.inner.Error() }
func (e *credStoreWriteError) Unwrap() []error { return []error{e.inner, ErrCredentialStoreWrite} }

// RecordLogin stores a freshly obtained login as the machine's one current
// Entire login. A new login replaces the previous one; the issuer, account
// metadata, access token, and refresh token all live in the configured
// credential backend (the OS keychain by default).
func RecordLogin(rawToken, refreshToken string) error {
	claims, err := tokens.ParseClaims(rawToken)
	if err != nil {
		return fmt.Errorf("parse login token claims: %w", err)
	}
	coreURL := claims.Issuer
	if coreURL == "" {
		return errors.New("login token has no iss claim; cannot derive login server URL")
	}
	handle := claims.Handle
	if handle == "" {
		handle = claims.Subject
	}
	if handle == "" {
		return errors.New("login token has no handle/sub claim; cannot key the credential slot")
	}

	keychainService := tokenstore.CoreKeyringService(coreURL)

	expiresIn := int64(defaultLoginTokenTTL.Seconds())
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
	// loginTokenStore.SaveTokens: a partial write must never leave a fresh
	// access token paired with a stale refresh token left over from an
	// earlier login. Refresh-first means a failed refresh write aborts before
	// the access token is touched (old pair preserved), rather than committing
	// a new access JWT against a dead refresh token.
	refreshSlot := tokenstore.RefreshService(keychainService)
	if refreshToken != "" {
		if err := tokenstore.Set(refreshSlot, handle, refreshToken); err != nil {
			return fmt.Errorf("store refresh token in credential store: %w", &credStoreWriteError{err})
		}
	} else {
		_ = tokenstore.Delete(refreshSlot, handle) //nolint:errcheck // best-effort cleanup of a stale refresh token
	}

	encoded := tokenstore.EncodeTokenWithExpiration(rawToken, expiresIn)
	if err := tokenstore.Set(keychainService, handle, encoded); err != nil {
		return fmt.Errorf("store login token in credential store: %w", &credStoreWriteError{err})
	}

	const name = "current"
	cfgDir := userdirs.Config()
	if modErr := contexts.Modify(cfgDir, func(f *contexts.File) (bool, error) {
		next := &contexts.Context{
			Name:            name,
			CoreURL:         coreURL,
			Handle:          handle,
			KeychainService: keychainService,
		}
		for _, previous := range f.Contexts {
			sameLogin := sameIssuer(previous.CoreURL, coreURL) && previous.Handle == handle
			if sameLogin {
				next.JurisdictionAudiences = previous.JurisdictionAudiences
				continue
			}
			if err := deleteContextKeychain(previous); err != nil {
				return false, fmt.Errorf("replace previous login: %w", err)
			}
		}
		f.CurrentContext = name
		f.Contexts = []*contexts.Context{next}
		return true, nil
	}); modErr != nil {
		return fmt.Errorf("store current login: %w", modErr)
	}

	return nil
}

// sameIssuer compares two login server URLs ignoring a trailing slash.
func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// LocalIdentityCacheKey returns a non-secret local auth identity key.
func LocalIdentityCacheKey() (string, error) {
	if raw := strings.TrimSpace(os.Getenv(EnvTokenVar)); raw != "" {
		claims, err := tokens.ParseClaims(raw)
		if err != nil {
			return "", fmt.Errorf("parse %s claims: %w", EnvTokenVar, err)
		}
		return strings.Join([]string{
			"env",
			strings.TrimRight(claims.Issuer, "/"),
			claims.Subject,
			claims.Handle,
			strings.Join(claims.Audience, ","),
		}, "|"), nil
	}

	c, ok, err := usableCurrentLogin()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return strings.Join([]string{
		"login",
		strings.TrimRight(c.CoreURL, "/"),
		c.Handle,
		c.KeychainService,
	}, "|"), nil
}

// StoredLoginToken returns the current login JWT from its credential slot.
// The encoded expiry is stripped;
// the server is the authority on validity and the device-flow login holds
// no refresh token, so an expired token surfaces as a 401 the caller can
// translate into a re-login hint.
func StoredLoginToken(c *contexts.Context) (string, error) {
	if c == nil {
		return "", errors.New("nil login")
	}
	if c.KeychainService == "" || c.Handle == "" {
		return "", errors.New("current login has no credential slot")
	}
	encoded, err := tokenstore.Get(c.KeychainService, c.Handle)
	if err != nil {
		return "", fmt.Errorf("read current login token: %w", err)
	}
	if encoded == "" {
		return "", errors.New("no login token stored (run `entire login`)")
	}
	token, _ := tokenstore.DecodeTokenWithExpiration(encoded)
	return token, nil
}
