//go:build (dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd

package tokenstore

import (
	"errors"
	"fmt"
	"testing"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
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
			if tt.err != nil && !sameError(got, tt.err) {
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

// keyringStore classifies on every operation, not just the classifier in
// isolation: go-keyring's mock returns the provider error from Get, Set and
// Delete, as the Secret Service provider does.
func TestKeyringStore_ClassifiesEveryOperation(t *testing.T) {
	// Not parallel: go-keyring's mock provider is process-global.
	t.Cleanup(keyring.MockInit)

	ops := map[string]func(keyringStore) error{
		"get":    func(s keyringStore) error { _, err := s.Get("svc", "user"); return err },
		"set":    func(s keyringStore) error { return s.Set("svc", "user", "secret") },
		"delete": func(s keyringStore) error { return s.Delete("svc", "user") },
	}
	for _, tt := range []struct {
		name        string
		providerErr error
		wantMissing bool
	}{
		{name: "no provider", providerErr: dbus.Error{Name: dbusServiceUnknown, Body: []any{"not provided"}}, wantMissing: true},
		{name: "unrelated D-Bus error", providerErr: dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}},
	} {
		keyring.MockInitWithError(tt.providerErr)
		for op, call := range ops {
			err := call(keyringStore{})
			if !sameError(err, tt.providerErr) {
				t.Errorf("%s/%s: err = %v, lost the provider error", tt.name, op, err)
			}
			if errors.Is(err, ErrKeyringServiceMissing) != tt.wantMissing {
				t.Errorf("%s/%s: err = %v, want ErrKeyringServiceMissing = %v", tt.name, op, err, tt.wantMissing)
			}
		}
	}
}

// sameError reports whether want is reachable in got's chain. A dbus.Error
// value holds a slice, so it is not comparable and errors.Is never matches
// it; match those by name instead.
func sameError(got, want error) bool {
	if name, ok := dbusErrorName(want); ok {
		gotName, gotOK := dbusErrorName(got)
		return gotOK && gotName == name
	}
	return errors.Is(got, want)
}
