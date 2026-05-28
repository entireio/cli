package cli

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/entireio/auth-go/tokens"

	"github.com/entireio/cli/internal/entiredb/client/contexts"
	"github.com/entireio/cli/internal/entiredb/tokenstore"
)

// fallbackTokenTTL is the access-token TTL stamped into the
// entiredb-format keyring entry when the JWT's exp claim is missing —
// matches entire-core's 1h JWTIssuer default. The token still verifies
// on its own merit either way; the TTL only governs when the
// vendored auth-interceptor refreshes proactively.
const fallbackTokenTTL = 3600

// writeEntiredbLoginContext persists the just-issued access token in
// the entiredb-format credential store so `entire repo` (and any other
// vendored entire-repo subcommand) finds it without a separate
// `entire-core auth login` call. Specifically:
//
//   - The token is written to the OS keyring under the entire-core
//     issuer-keyed service ("entire-core:<iss>") so the vendored
//     cliauth/auth-interceptor reads it from the same slot
//     `entire-core auth login` would have written.
//   - A kubectl-style context entry is upserted into
//     ~/.config/entire/contexts.json (the file the vendored cliauth
//     reads) carrying the issuer URL, principal handle, and
//     keychain-service name. current_context is flipped to this entry,
//     matching `entire-core auth login`'s "I just authenticated, this
//     is now my default session" UX.
//
// The token's iss and handle are pulled from the JWT's own claims
// (already verified at runLogin's validateReceivedToken step). If the
// token is opaque (not a JWT) or the claims are incomplete, the
// bridge skips silently with a warning — the cli's own keyring
// already has the bearer, so `entire login` is still functional for
// non-entiredb surfaces.
//
// Refresh tokens are not bridged: the device-flow this CLI uses
// returns only a bearer. The vendored auth-interceptor's refresh path
// logs a warning and falls back to the existing bearer on expiry,
// which matches the cli's pre-existing auth UX.
func writeEntiredbLoginContext(errW io.Writer, accessToken string) error {
	claims, err := tokens.ParseClaims(accessToken)
	if err != nil {
		// Opaque token — nothing to bridge. The cli's own keyring still
		// has the bearer for surfaces that don't go through contexts.json.
		fmt.Fprintf(errW, "Note: skipping entiredb context bridge (token is not a JWT: %v)\n", err)
		return nil
	}
	issuer := strings.TrimRight(claims.Issuer, "/")
	if issuer == "" || claims.Handle == "" {
		fmt.Fprintf(errW, "Note: skipping entiredb context bridge (token missing iss or handle claim — `entire repo` will need `entire-core auth login`)\n")
		return nil
	}

	keychain := tokenstore.CoreKeyringService(issuer)
	encoded := tokenstore.EncodeTokenWithExpiration(accessToken, fallbackTokenTTL)
	if err := tokenstore.Set(keychain, claims.Handle, encoded); err != nil {
		return fmt.Errorf("store access token in entiredb keyring slot: %w", err)
	}

	configDir := contexts.DefaultConfigDir()
	contextName, err := defaultEntiredbContextName(issuer, claims.Handle)
	if err != nil {
		return fmt.Errorf("derive context name: %w", err)
	}
	if err := contexts.Modify(configDir, func(f *contexts.File) (bool, error) {
		f.Upsert(&contexts.Context{
			Name:            contextName,
			CoreURL:         issuer,
			Handle:          claims.Handle,
			KeychainService: keychain,
		})
		f.CurrentContext = contextName
		return true, nil
	}); err != nil {
		return fmt.Errorf("update entiredb contexts.json: %w", err)
	}
	fmt.Fprintf(errW, "Activated entiredb context %q (covers `entire repo`).\n", contextName)
	return nil
}

// defaultEntiredbContextName derives the context entry name from the
// JWT's iss host plus the principal handle. Matches the convention
// `entire-core auth login` uses for its default --name (sans the
// OAuth provider element, which the device-flow JWT doesn't carry).
func defaultEntiredbContextName(issuer, handle string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil {
		return "", fmt.Errorf("parse issuer %q: %w", issuer, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("issuer %q has no host", issuer)
	}
	if handle == "" {
		return u.Host, nil
	}
	return handle + "@" + u.Host, nil
}
