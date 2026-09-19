package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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

// SessionFamilyIDFromLoginJWT reads the fid (refresh-token family id) claim
// without verifying the signature — the caller only uses it to recognise which
// row of a session listing is its own, exactly as HomeJurisdictionFromLoginJWT
// only routes. A login session IS a refresh-token family (see
// api.AuthSession), so fid is what identifies the caller's session.
//
// Returns "" (no error) when the claim is absent, so a core too old to mint it
// degrades to "no session identified" rather than to an error.
func SessionFamilyIDFromLoginJWT(loginJWT string) (string, error) {
	parts := strings.Split(loginJWT, ".")
	if len(parts) < 2 {
		return "", errors.New("login token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode login token payload: %w", err)
	}
	var claims struct {
		FID string `json:"fid"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse login token payload: %w", err)
	}
	return claims.FID, nil
}
