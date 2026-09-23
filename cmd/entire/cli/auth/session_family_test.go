package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/entireio/auth-go/tokens"
)

// signedShapeJWT builds a token with the given payload claims and a real
// algorithm. The signature itself is never verified here — the claim readers
// only pick a row out of a listing the server already authorized — but the
// header must name an algorithm, which is what separates a token worth reading
// from an alg:none one anybody can mint.
func signedShapeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

func TestSessionFamilyIDFromLoginJWT(t *testing.T) {
	t.Parallel()

	t.Run("reads the fid claim", func(t *testing.T) {
		t.Parallel()
		got, err := SessionFamilyIDFromLoginJWT(signedShapeJWT(t, map[string]any{"fid": "fam-123", "sub": "u1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "fam-123" {
			t.Errorf("fid = %q, want %q", got, "fam-123")
		}
	})

	// A core too old to mint fid must degrade to "no session identified", not
	// to an error that would take the whole status command down with it.
	t.Run("absent claim is empty, not an error", func(t *testing.T) {
		t.Parallel()
		got, err := SessionFamilyIDFromLoginJWT(signedShapeJWT(t, map[string]any{"sub": "u1"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("fid = %q, want empty", got)
		}
	})

	for name, tok := range map[string]string{
		"not a JWT":           "opaque-token",
		"undecodable payload": "aGVhZGVy.!!!not-base64!!!.sig",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := SessionFamilyIDFromLoginJWT(tok); err == nil {
				t.Errorf("SessionFamilyIDFromLoginJWT(%q) = nil error, want one", tok)
			}
		})
	}
}

// Every claim reader in this package refuses an unsigned token, so a caller
// cannot mint one to name itself a session or a home region. The check belongs
// to decodeLoginJWTClaims, so both readers are asserted against it.
func TestDecodeLoginJWTClaims_RejectsAlgNone(t *testing.T) {
	t.Parallel()

	enc := base64.RawURLEncoding.EncodeToString
	algNone := enc([]byte(`{"alg":"none"}`)) + "." +
		enc([]byte(`{"fid":"fam-123","home_jurisdiction":"us"}`)) + "."

	if _, err := SessionFamilyIDFromLoginJWT(algNone); !errors.Is(err, tokens.ErrUnsignedJWT) {
		t.Errorf("SessionFamilyIDFromLoginJWT err = %v, want ErrUnsignedJWT", err)
	}
	if _, err := HomeJurisdictionFromLoginJWT(algNone); !errors.Is(err, tokens.ErrUnsignedJWT) {
		t.Errorf("HomeJurisdictionFromLoginJWT err = %v, want ErrUnsignedJWT", err)
	}
}
