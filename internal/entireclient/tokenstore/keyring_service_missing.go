package tokenstore

import (
	"errors"
	"fmt"
)

// ErrKeyringServiceMissing marks an OS keyring call that failed because no
// Secret Service provider (gnome-keyring, KeePassXC, ...) answers on the D-Bus
// session bus, or the session bus itself cannot be reached. Unlike
// ErrKeyringUnavailable, logging in again with the file store does fix it, so
// callers attach that advice; the text here only names the cause.
//
// Only Linux and the BSDs (go-keyring's Secret Service provider) return it.
var ErrKeyringServiceMissing = errors.New("no Secret Service provider is reachable on the D-Bus session bus, so the OS keyring cannot be used")

// KeyringServiceMissingError carries the provider's own error next to
// ErrKeyringServiceMissing, so both stay reachable through errors.Is/As and
// the D-Bus text is still shown for diagnosis.
type KeyringServiceMissingError struct{ Cause error }

func (e *KeyringServiceMissingError) Error() string {
	return fmt.Sprintf("%s (%v)", ErrKeyringServiceMissing, e.Cause)
}

func (e *KeyringServiceMissingError) Unwrap() []error {
	return []error{ErrKeyringServiceMissing, e.Cause}
}

// IsKeyringUnreadable reports whether err says the OS keyring could not be
// read at all, as opposed to holding no login. Callers that treat "not logged
// in" as a soft outcome use it so an unreadable keyring is not reported as a
// missing login.
func IsKeyringUnreadable(err error) bool {
	return errors.Is(err, ErrKeyringUnavailable) || errors.Is(err, ErrKeyringServiceMissing)
}
