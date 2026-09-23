package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/auth-go/tokens"
)

// LoginTokenExpiry reports when a login JWT's access half stops being accepted,
// without verifying the signature — the server re-verifies, and a caller only
// uses this to say how long the bearer in hand has left.
//
// This is NOT a session's lifetime. A session is a refresh-token family lasting
// weeks; the access token it mints lasts about an hour and is normally renewed
// long before it lapses. The two coincide in exactly one case worth reporting:
// a family revoked while its last access token is still inside that hour, where
// the token's expiry IS when the user gets logged out.
//
// Returns a zero time (no error) when the token carries no exp, so a caller can
// stay quiet rather than invent a deadline.
func LoginTokenExpiry(loginJWT string) (time.Time, error) {
	claims, err := tokens.ParseClaims(loginJWT)
	if err != nil {
		return time.Time{}, err //nolint:wrapcheck // ParseClaims already names the token
	}
	return claims.ExpiresAt, nil
}

// decodeLoginJWTClaims reads custom claims out of a login JWT's payload into
// out, without verifying the signature — the server re-verifies, and the
// readers here only use what they find to recognise a row or route a request.
//
// Unverified is not unchecked. tokens.ParseClaims runs first, so the token must
// be a well-formed three-segment JWT naming a real algorithm; an alg:none token
// is refused here exactly as CoreURLFromEnvToken refuses one, keeping every
// reader in this package on a single policy. Having passed, the payload segment
// is known present, decodable and valid JSON, so the second pass only has to
// reach the claim ParseClaims has no field for; its own error paths are there
// because the compiler requires them, not because they are expected.
func decodeLoginJWTClaims(loginJWT string, out any) error {
	if _, err := tokens.ParseClaims(loginJWT); err != nil {
		return fmt.Errorf("login token: %w", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(loginJWT, ".")[1])
	if err != nil {
		return fmt.Errorf("decode login token payload: %w", err)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("parse login token payload: %w", err)
	}
	return nil
}

// SessionFamilyIDFromLoginJWT reads the fid (refresh-token family id) claim
// without verifying the signature — the caller only uses it to recognise which
// row of a session listing is its own, exactly as HomeJurisdictionFromLoginJWT
// only routes. A login session IS a refresh-token family (see
// api.AuthSession), so fid is what identifies the caller's session.
//
// Returns "" (no error) when the claim is absent, so a core too old to mint it
// degrades to "no session identified" rather than to an error.
func SessionFamilyIDFromLoginJWT(loginJWT string) (string, error) {
	var claims struct {
		FID string `json:"fid"`
	}
	if err := decodeLoginJWTClaims(loginJWT, &claims); err != nil {
		return "", err
	}
	return claims.FID, nil
}
