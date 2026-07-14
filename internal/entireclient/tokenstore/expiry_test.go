package tokenstore

import (
	"testing"
	"time"
)

func TestEncodeDecodeTokenWithExpiration(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		expiresIn int64
	}{
		{
			name:      "normal token",
			token:     "entr_abc123xyz",
			expiresIn: 3600, // 1 hour
		},
		{
			name:      "token with special characters",
			token:     "entr_abc+123/xyz==",
			expiresIn: 7200,
		},
		{
			name:      "short expiration",
			token:     "entr_test",
			expiresIn: 60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := time.Now().Truncate(time.Second)
			encoded := EncodeTokenWithExpiration(tt.token, tt.expiresIn)
			after := time.Now().Truncate(time.Second).Add(time.Second)

			decodedToken, expiresAt := DecodeTokenWithExpiration(encoded)

			if decodedToken != tt.token {
				t.Errorf("token mismatch: got %q, want %q", decodedToken, tt.token)
			}

			expectedMin := before.Add(time.Duration(tt.expiresIn) * time.Second)
			expectedMax := after.Add(time.Duration(tt.expiresIn) * time.Second)

			if expiresAt.Before(expectedMin) || expiresAt.After(expectedMax) {
				t.Errorf("expiresAt %v not in expected range [%v, %v]", expiresAt, expectedMin, expectedMax)
			}
		})
	}
}

func TestDecodeTokenWithExpiration_LegacyFormat(t *testing.T) {
	// A token written before the |expiration suffix was introduced — no
	// pipe, no Unix timestamp. Decoder must round-trip it unchanged with
	// a zero expiresAt so the refresh layer treats it as already-expired.
	legacyToken := "entr_token_without_expiration"

	token, expiresAt := DecodeTokenWithExpiration(legacyToken)

	if token != legacyToken {
		t.Errorf("token mismatch: got %q, want %q", token, legacyToken)
	}

	if !expiresAt.IsZero() {
		t.Errorf("expiresAt should be zero for legacy tokens, got %v", expiresAt)
	}
}

func TestDecodeTokenWithExpiration_InvalidFormat(t *testing.T) {
	invalidToken := "entr_token|not_a_number"

	token, expiresAt := DecodeTokenWithExpiration(invalidToken)

	if token != invalidToken {
		t.Errorf("token mismatch: got %q, want %q", token, invalidToken)
	}

	if !expiresAt.IsZero() {
		t.Errorf("expiresAt should be zero for invalid format, got %v", expiresAt)
	}
}

func TestIsTokenExpiredOrExpiring(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{
			name:      "zero time (legacy token)",
			expiresAt: time.Time{},
			want:      true,
		},
		{
			name:      "already expired",
			expiresAt: time.Now().Add(-1 * time.Hour),
			want:      true,
		},
		{
			name:      "expiring within buffer (4 minutes left)",
			expiresAt: time.Now().Add(4 * time.Minute),
			want:      true,
		},
		{
			name:      "expiring at buffer edge (5 minutes left)",
			expiresAt: time.Now().Add(5 * time.Minute),
			want:      true,
		},
		{
			name:      "not expiring soon (6 minutes left)",
			expiresAt: time.Now().Add(6 * time.Minute),
			want:      false,
		},
		{
			name:      "plenty of time (1 hour left)",
			expiresAt: time.Now().Add(1 * time.Hour),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsTokenExpiredOrExpiring(tt.expiresAt)
			if got != tt.want {
				t.Errorf("IsTokenExpiredOrExpiring(%v) = %v, want %v", tt.expiresAt, got, tt.want)
			}
		})
	}
}
