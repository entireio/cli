//go:build (dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd

package tokenstore

import (
	"errors"
	"fmt"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

func TestClassifySecretServiceError(t *testing.T) {
	t.Parallel()

	serviceUnknown := dbus.Error{Name: dbusServiceUnknown, Body: []any{"The name org.freedesktop.secrets was not provided by any .service files"}}
	noOwner := &dbus.Error{Name: dbusNameHasNoOwner, Body: []any{"Could not get owner of name 'org.freedesktop.secrets': no such name"}}
	accessDenied := dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"denied"}}
	connectFailed := errors.New("dbus: couldn't determine address of session bus")
	busDown := errors.New("bus down")

	tests := []struct {
		name        string
		err         error
		busErr      error
		wantMissing bool
		wantProbe   bool
	}{
		{name: "nil", err: nil},
		{name: "not found is not classified", err: ErrNotFound},
		{name: "ServiceUnknown reply", err: serviceUnknown, wantMissing: true},
		{name: "NameHasNoOwner reply (pointer form)", err: noOwner, wantMissing: true},
		{name: "wrapped ServiceUnknown reply", err: fmt.Errorf("open session: %w", serviceUnknown), wantMissing: true},
		{name: "unrelated D-Bus reply", err: accessDenied},
		{name: "session bus unreachable", err: connectFailed, busErr: busDown, wantMissing: true, wantProbe: true},
		{name: "other error with the bus reachable", err: errors.New("failed to unlock correct collection"), wantProbe: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probed := false
			got := classifySecretServiceError(tt.err, func() error {
				probed = true
				return tt.busErr
			})
			if probed != tt.wantProbe {
				t.Errorf("probed = %v, want %v", probed, tt.wantProbe)
			}
			if errors.Is(got, ErrKeyringServiceMissing) != tt.wantMissing {
				t.Fatalf("classify(%v) = %v, want ErrKeyringServiceMissing = %v", tt.err, got, tt.wantMissing)
			}
			// The provider's own error stays reachable either way.
			if tt.err != nil && !errors.Is(got, tt.err) {
				t.Errorf("classify(%v) = %v, lost the original error", tt.err, got)
			}
			if tt.wantMissing && errors.Is(got, ErrNotFound) {
				t.Errorf("classify(%v) = %v must not also match ErrNotFound", tt.err, got)
			}
		})
	}

	t.Run("D-Bus reply stays reachable through errors.As", func(t *testing.T) {
		t.Parallel()

		got := classifySecretServiceError(serviceUnknown, func() error { return nil })
		var dbusErr dbus.Error
		if !errors.As(got, &dbusErr) || dbusErr.Name != dbusServiceUnknown {
			t.Errorf("errors.As(%v, dbus.Error) = %v (%q), want the ServiceUnknown reply", got, dbusErr, dbusErr.Name)
		}
	})
}
