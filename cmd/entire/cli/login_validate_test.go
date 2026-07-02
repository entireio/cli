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

// Opaque and otherwise claim-free tokens are rejected up front with an
// error naming the requirement: RecordLoginContext (the sole persistence
// path) keys the context on iss/handle claims, so they could never
// complete a login anyway.
func TestValidateReceivedToken_RejectsClaimFreeTokens(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		token string
		want  string // substring of the rejection
	}{
		"opaque":             {"opaque-token-string", "parseable JWT claims"},
		"3-seg opaque":       {"aaa.bbb.ccc", "parseable JWT claims"},
		"bad base64 payload": {strings.Join([]string{"eyJhbGciOiJSUzI1NiJ9" /* {"alg":"RS256"} */, "!!!not-base64!!!", "sig"}, "."), "parseable JWT claims"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateReceivedToken(tc.token, "https://example.test", time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateReceivedToken(%s) = %v, want error containing %q", name, err, tc.want)
			}
		})
	}
}

func TestValidateReceivedToken_RejectsMissingIss(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"handle":"alice"}`)
	err := validateReceivedToken(jwt, "https://example.test", time.Now())
	if err == nil || !strings.Contains(err.Error(), "no iss claim") {
		t.Fatalf("validateReceivedToken(no iss) = %v, want no-iss error", err)
	}
}

func TestValidateReceivedToken_RejectsMissingHandleAndSub(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test"}`)
	err := validateReceivedToken(jwt, "https://example.test", time.Now())
	if err == nil || !strings.Contains(err.Error(), "no handle or sub claim") {
		t.Fatalf("validateReceivedToken(no handle/sub) = %v, want no-handle error", err)
	}
}

func TestValidateReceivedToken_SubAloneSatisfiesIdentityClaim(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test","sub":"user-123"}`)
	if err := validateReceivedToken(jwt, "https://example.test", time.Now()); err != nil {
		t.Fatalf("validateReceivedToken(sub only) = %v, want nil", err)
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

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://impostor.test","handle":"alice"}`)
	err := validateReceivedToken(jwt, "https://example.test", time.Now())
	if err == nil || !strings.Contains(err.Error(), "iss mismatch") {
		t.Fatalf("validateReceivedToken(iss mismatch) = %v, want iss-mismatch error", err)
	}
}

func TestValidateReceivedToken_AllowsIssuerTrailingSlashDiff(t *testing.T) {
	t.Parallel()

	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test/","handle":"alice"}`)
	if err := validateReceivedToken(jwt, "https://example.test", time.Now()); err != nil {
		t.Fatalf("validateReceivedToken(trailing slash) = %v, want nil", err)
	}
}

func TestValidateReceivedToken_RejectsAlreadyExpired(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test","handle":"alice","exp":1700000000}`)
	err := validateReceivedToken(jwt, "https://example.test", now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "already expired") {
		t.Fatalf("validateReceivedToken(expired) = %v, want already-expired error", err)
	}
}

func TestValidateReceivedToken_AllowsFutureExp(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	jwt := makeJWT(t, `{"alg":"RS256"}`, `{"iss":"https://example.test","handle":"alice","exp":1700009000}`)
	if err := validateReceivedToken(jwt, "https://example.test", now); err != nil {
		t.Fatalf("validateReceivedToken(future exp) = %v, want nil", err)
	}
}

func TestParseLoginServer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string // want=="" means error expected
	}{
		{"default form", "https://us.auth.entire.io", "https://us.auth.entire.io"},
		{"trailing slash normalised", "https://eu.auth.entire.io/", "https://eu.auth.entire.io"},
		{"case and default port normalised", "HTTPS://US.AUTH.ENTIRE.IO:443", "https://us.auth.entire.io"},
		{"loopback http kept", "http://127.0.0.1:8787", "http://127.0.0.1:8787"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"no scheme", "us.auth.entire.io", ""},
		{"bad scheme", "ftp://x.example", ""},
		{"userinfo rejected", "https://tok@evil.example", ""},
		{"path rejected", "https://x.example/oauth", ""},
		{"query rejected", "https://x.example?a=1", ""},
		{"fragment rejected", "https://x.example#frag", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLoginServer(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("parseLoginServer(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLoginServer(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseLoginServer(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
