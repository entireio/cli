package tokenstore

import (
	"fmt"
	"strings"
	"time"
)

// TokenExpirationSeparator separates the token from its expiration timestamp.
// Format: "token|expires_at_unix"
const TokenExpirationSeparator = "|"

// TokenExpirationBuffer is how long before expiration we should refresh.
const TokenExpirationBuffer = 5 * time.Minute

// EncodeTokenWithExpiration encodes a token with its expiration time as a
// "token|expires_at_unix" suffix. Callers must pass a positive expiresIn —
// the encoded suffix is the contract that tells future readers when to
// refresh, and a zero/negative TTL would record an immediately-expired
// token.
func EncodeTokenWithExpiration(token string, expiresIn int64) string {
	expiresAt := time.Now().Unix() + expiresIn
	return fmt.Sprintf("%s%s%d", token, TokenExpirationSeparator, expiresAt)
}

// DecodeTokenWithExpiration decodes a token that may have an expiration timestamp.
// Returns the token and expiration time. If no expiration is encoded, returns
// the token as-is and a zero time.
func DecodeTokenWithExpiration(encoded string) (token string, expiresAt time.Time) {
	idx := strings.LastIndex(encoded, TokenExpirationSeparator)
	if idx == -1 {
		return encoded, time.Time{}
	}

	token = encoded[:idx]
	var expiresAtUnix int64
	if _, err := fmt.Sscanf(encoded[idx+1:], "%d", &expiresAtUnix); err != nil {
		return encoded, time.Time{}
	}
	return token, time.Unix(expiresAtUnix, 0)
}

// IsTokenExpiredOrExpiring checks if a token is expired or will expire soon.
// A zero expiresAt is treated as "unknown, assume expired".
func IsTokenExpiredOrExpiring(expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return time.Now().Add(TokenExpirationBuffer).After(expiresAt)
}
