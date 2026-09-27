package controlplane

import (
	"testing"
	"time"
)

// Vectors from RFC 6238 Appendix B (SHA-1), truncated to the six digits an
// authenticator app shows. The secret is the RFC's ASCII "12345678901234567890"
// in base32.
func TestTOTPCode(t *testing.T) {
	t.Parallel()
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := []struct {
		secret string
		at     int64
		want   string
	}{
		{secret, 59, "287082"},
		{secret, 1111111109, "081804"},
		{secret, 1234567890, "005924"},
		// GitHub shows the setup key lowercase in spaced groups of four.
		{"gezd gnbv gy3t qojq gezd gnbv gy3t qojq", 59, "287082"},
		// A key copied from elsewhere may carry base32 padding.
		{"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ====", 59, "287082"},
	}
	for _, tc := range cases {
		got, err := totpCode(tc.secret, time.Unix(tc.at, 0))
		if err != nil {
			t.Fatalf("totpCode(%q, %d): %v", tc.secret, tc.at, err)
		}
		if got != tc.want {
			t.Errorf("totpCode(%q, %d) = %q, want %q", tc.secret, tc.at, got, tc.want)
		}
	}
}

func TestTOTPCode_RejectsMalformedSecret(t *testing.T) {
	t.Parallel()
	if _, err := totpCode("not base32!", time.Unix(59, 0)); err == nil {
		t.Fatal("expected an error for a secret that is not base32")
	}
}
