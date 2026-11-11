//go:build (dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd

// The build constraint matches go-keyring's keyring_unix.go: these are the
// platforms where the OS keyring is the Secret Service over D-Bus.

package tokenstore

import (
	"errors"
	"testing"

	dbus "github.com/godbus/dbus/v5"
)

// D-Bus error names the bus daemon replies with when nothing owns the name
// go-keyring calls (org.freedesktop.secrets): ServiceUnknown when no running
// process owns it and no activatable .service file provides it,
// NameHasNoOwner when the daemon reports the name as unowned without trying
// activation.
const (
	dbusServiceUnknown = "org.freedesktop.DBus.Error.ServiceUnknown"
	dbusNameHasNoOwner = "org.freedesktop.DBus.Error.NameHasNoOwner"
)

// classifyKeyringError wraps a Secret Service failure that means "no provider
// to talk to" in KeyringServiceMissingError; every other result passes
// through. keyringStore calls it inside the timed keyring call, so the
// session-bus probe is bounded by the same timeout.
func classifyKeyringError(err error) error {
	return classifySecretServiceError(err, sessionBusErr)
}

// classifySecretServiceError is classifyKeyringError with the session-bus
// probe injected, so tests never reach a real bus.
//
// go-keyring returns godbus errors unchanged. A reply from the bus is a typed
// dbus.Error and is classified by name alone. A failure to connect to the
// session bus is untyped (a missing bus address, a failed dbus-launch, a
// refused dial all come back as plain errors), so for any other error the bus
// is asked directly: if it cannot be reached either, that is the cause.
func classifySecretServiceError(err error, busErr func() error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	if name, ok := dbusErrorName(err); ok {
		if name == dbusServiceUnknown || name == dbusNameHasNoOwner {
			return &KeyringServiceMissingError{Cause: err}
		}
		return err
	}
	if busErr() != nil {
		return &KeyringServiceMissingError{Cause: err}
	}
	return err
}

// dbusErrorName returns the name of a D-Bus error reply in err's chain.
// godbus produces both value and pointer forms.
func dbusErrorName(err error) (string, bool) {
	var val dbus.Error
	if errors.As(err, &val) {
		return val.Name, true
	}
	var ptr *dbus.Error
	if errors.As(err, &ptr) && ptr != nil {
		return ptr.Name, true
	}
	return "", false
}

// sessionBusErr reports whether the D-Bus session bus can be reached, through
// the same shared connection go-keyring uses: connected already, it returns
// at once. Under `go test` it never touches the real bus and reports it
// reachable, like probeKeyringReachable.
func sessionBusErr() error {
	if testing.Testing() {
		return nil
	}
	_, err := dbus.SessionBus()
	return err //nolint:wrapcheck // only compared against nil
}
