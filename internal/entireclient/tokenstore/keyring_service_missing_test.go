package tokenstore

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestKeyringServiceMissingError(t *testing.T) {
	t.Parallel()

	cause := errors.New("The name org.freedesktop.secrets was not provided by any .service files")
	err := fmt.Errorf("read access token: %w", &KeyringServiceMissingError{Cause: cause})

	if !errors.Is(err, ErrKeyringServiceMissing) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want both ErrKeyringServiceMissing and the cause", err)
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Errorf("err = %q, want the provider's text kept for diagnosis", err)
	}
}

func TestIsKeyringUnreadable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "not found", err: ErrNotFound},
		{name: "macOS Keychain unreachable", err: fmt.Errorf("x: %w", ErrKeyringUnavailable), want: true},
		{name: "no Secret Service provider", err: fmt.Errorf("x: %w", &KeyringServiceMissingError{Cause: errors.New("dbus")}), want: true},
		{name: "other", err: errors.New("boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsKeyringUnreadable(tt.err); got != tt.want {
				t.Errorf("IsKeyringUnreadable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
