package cli

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/entireio/auth-go/tokens"
)

// makeJWT builds a three-segment JWT-shaped string from the given header and
// payload JSON, with a junk signature segment. ParseClaims doesn't verify
// signatures, so this is enough to exercise validateReceivedToken's checks.
func makeJWT(t *testing.T, headerJSON, payloadJSON string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	return strings.Join([]string{
		enc.EncodeToString([]byte(headerJSON)),
		enc.EncodeToString([]byte(payloadJSON)),
		enc.EncodeToString([]byte("sig")),
	}, ".")
}

func TestValidateReceivedToken_OpaqueTokenAccepted(t *testing.T) {
	t.Parallel()

	if err := validateReceivedToken("opaque-token-string", "https://example.test", time.Now()); err != nil {
		t.Fatalf("validateReceivedToken(opaque) = %v, want nil", err)
	}
}

func TestValidateReceivedToken_DotBearingOpaqueTokenAccepted(t *testing.T) {
	t.Parallel()

	// 3-segment opaque token whose segments aren't valid base64url. Previously
	// rejected because ParseClaims falls through ErrMalformedJWT and surfaces
	// a generic decode error; should be accepted now as just-another-opaque-
	// token so an AS issuing dot-bearing non-JWT bearers can still log in.
	if err := validateReceivedToken("aaa.bbb.ccc", "https://example.test", time.Now()); err != nil {
		t.Fatalf("validateReceivedToken(3-seg opaque) = %v, want nil", err)
	}
}

func TestValidateReceivedToken_BadBase64PayloadAccepted(t *testing.T) {
	t.Parallel()

	// 3-segment token with a JWT-shaped header but a payload that isn't valid
	// base64url. Same principle: any parse failure other than ErrUnsignedJWT
	// is treated as opaque.
	jwt := strings.Join([]string{
		"eyJhbGciOiJSUzI1NiJ9", // {"alg":"RS256"}
		"!!!not-base64!!!",
		"sig",
	}, ".")
	if err := validateReceivedToken(jwt, "https://example.test", time.Now()); err != nil {
		t.Fatalf("validateReceivedToken(bad base64 payload) = %v, want nil", err)
	}
}

func TestValidateReceivedToken_RejectsUnsignedJWT(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"none"}`, `{"iss":"https://example.test"}`)
	err := validateReceivedToken(jwt, "https://example.test", time.Now())
	if !errors.Is(err, tokens.ErrUnsignedJWT) {
		t.Fatalf("validateReceivedToken(alg:none) = %v, want ErrUnsignedJWT", err)
	}
}

func TestValidateReceivedToken_RejectsIssuerMismatch(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://impostor.test"}`)
	err := validateReceivedToken(jwt, "https://example.test", time.Now())
	if err == nil || !strings.Contains(err.Error(), "iss mismatch") {
		t.Fatalf("validateReceivedToken(iss mismatch) = %v, want iss-mismatch error", err)
	}
}

func TestValidateReceivedToken_AllowsIssuerTrailingSlashDiff(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test/"}`)
	if err := validateReceivedToken(jwt, "https://example.test", time.Now()); err != nil {
		t.Fatalf("validateReceivedToken(trailing slash) = %v, want nil", err)
	}
}

func TestValidateReceivedToken_RejectsAlreadyExpired(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test","exp":1700000000}`)
	err := validateReceivedToken(jwt, "https://example.test", now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "already expired") {
		t.Fatalf("validateReceivedToken(expired) = %v, want already-expired error", err)
	}
}

func TestValidateReceivedToken_AllowsFutureExp(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test","exp":1700009000}`)
	if err := validateReceivedToken(jwt, "https://example.test", now); err != nil {
		t.Fatalf("validateReceivedToken(future exp) = %v, want nil", err)
	}
}
