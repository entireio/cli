package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// unsignedJWT builds a token with the given payload claims. The signature is
// never verified here — SessionFamilyIDFromLoginJWT only reads a claim to pick
// a row out of a listing the server itself authorized.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func TestSessionFamilyIDFromLoginJWT(t *testing.T) {
	t.Parallel()

	t.Run("reads the fid claim", func(t *testing.T) {
		t.Parallel()
		got, err := SessionFamilyIDFromLoginJWT(unsignedJWT(t, map[string]any{"fid": "fam-123", "sub": "u1"}))
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
		got, err := SessionFamilyIDFromLoginJWT(unsignedJWT(t, map[string]any{"sub": "u1"}))
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
