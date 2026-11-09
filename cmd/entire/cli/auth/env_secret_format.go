package auth

import (
	"encoding/base32"
	"fmt"
	"hash/crc32"
	"regexp"
	"strings"

	"github.com/oklog/ulid/v2"
)

// A v1 service account secret is
//
//	ent_svc_<juri>_<account ULID>_<secret ULID>_<random><check>
//
// <juri> is the jurisdiction of the account's project, which also names the
// Core that holds the secret. <random> is 32 bytes in lowercase unpadded
// base32 (52 characters). <check> is the CRC-32 (IEEE) of everything before
// it, in base62, left-padded to 6 characters. Core owns the format and
// refuses every non-canonical spelling (lowercase ULIDs, a last base32
// character with unused bits set), so each secret has exactly one spelling.
const (
	secretRandomLen   = 52
	secretCheckLen    = 6 // 62^6 > 2^32, so every CRC-32 fits
	secretRandomChars = "abcdefghijklmnopqrstuvwxyz234567"
	secretCheckChars  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

var (
	secretJurisdictionPattern = regexp.MustCompile(`^[a-z]{2,8}$`)
	secretRandomEncoding      = base32.NewEncoding(secretRandomChars).WithPadding(base32.NoPadding)
)

// serviceAccountSecret is what a secret names, without the secret itself.
type serviceAccountSecret struct {
	jurisdiction string
	accountID    string // svc_<ULID>
}

// parseServiceAccountSecret validates a v1 secret offline, so a mistyped or
// truncated secret fails before it is sent anywhere. Errors never echo the
// secret.
func parseServiceAccountSecret(secret string) (serviceAccountSecret, error) {
	malformed := func(reason string) error {
		return fmt.Errorf("%s holds a malformed service account secret: %s", EnvTokenVar, reason)
	}
	rest, ok := strings.CutPrefix(secret, ServiceAccountSecretPrefix)
	if !ok {
		return serviceAccountSecret{}, malformed("missing the " + ServiceAccountSecretPrefix + " prefix")
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 4 {
		return serviceAccountSecret{}, malformed("want " + ServiceAccountSecretPrefix + "<jurisdiction>_<account>_<secret>_<random>")
	}
	juri, account, secretID, tail := parts[0], parts[1], parts[2], parts[3]
	if !secretJurisdictionPattern.MatchString(juri) {
		return serviceAccountSecret{}, malformed("the jurisdiction is not 2 to 8 lowercase letters")
	}
	if !isCanonicalULID(account) {
		return serviceAccountSecret{}, malformed("the service account ID is not a ULID")
	}
	if !isCanonicalULID(secretID) {
		return serviceAccountSecret{}, malformed("the secret ID is not a ULID")
	}
	if len(tail) != secretRandomLen+secretCheckLen {
		return serviceAccountSecret{}, malformed("the random part has the wrong length")
	}
	random, check := tail[:secretRandomLen], tail[secretRandomLen:]
	if strings.Trim(random, secretRandomChars) != "" {
		return serviceAccountSecret{}, malformed("the random part is not lowercase base32")
	}
	if !isCanonicalRandom(random) {
		return serviceAccountSecret{}, malformed("the random part sets unused bits in its last character")
	}
	if check != secretChecksum(secret[:len(secret)-secretCheckLen]) {
		return serviceAccountSecret{}, malformed("checksum mismatch; the secret is probably mistyped or truncated")
	}
	return serviceAccountSecret{jurisdiction: juri, accountID: "svc_" + account}, nil
}

// isCanonicalULID reports whether s is a ULID as ulid.String prints it.
func isCanonicalULID(s string) bool {
	id, err := ulid.ParseStrict(s)
	return err == nil && id.String() == s
}

// isCanonicalRandom reports whether random re-encodes to itself. 52 base32
// characters carry 260 bits for 256, and a decoder ignores the last 4.
func isCanonicalRandom(random string) bool {
	raw, err := secretRandomEncoding.DecodeString(random)
	return err == nil && secretRandomEncoding.EncodeToString(raw) == random
}

// secretChecksum is the <check> part for body, the secret up to and
// including the last random character.
func secretChecksum(body string) string {
	sum := crc32.ChecksumIEEE([]byte(body))
	var out [secretCheckLen]byte
	for i := secretCheckLen - 1; i >= 0; i-- {
		out[i] = secretCheckChars[sum%62]
		sum /= 62
	}
	return string(out[:])
}
