package controlplane

import (
	"crypto/hmac"
	"crypto/sha1" // RFC 6238 specifies HMAC-SHA1; GitHub's authenticator setup uses it.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// totpCode returns the RFC 6238 one-time code (SHA-1, six digits, 30-second
// step) for a base32 secret as GitHub shows it during authenticator setup:
// lowercase, spaced groups, and trailing padding are all accepted.
func totpCode(secret string, now time.Time) (string, error) {
	normalized := strings.TrimRight(strings.ToUpper(strings.ReplaceAll(secret, " ", "")), "=")
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized)
	if err != nil {
		return "", fmt.Errorf("decode TOTP secret: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1_000_000), nil
}
