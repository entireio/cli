// Package senclave wraps secrets with a Secure Enclave key on macOS.
//
// A single P-256 key lives in the Secure Enclave with a user-presence access
// control. Sealing uses the public key and never prompts; unsealing uses the
// private key and makes macOS show its Touch ID (or password) dialog. The
// key never leaves the enclave, so ciphertext at rest is useless to any
// process that cannot pass that dialog.
//
// Everything here is bound at runtime through purego, so release builds stay
// CGO_ENABLED=0. Non-darwin builds report ErrUnsupported.
package senclave

import "errors"

// DefaultTag names the token key shared by every Entire binary.
const DefaultTag = "io.entire.cli.token-key"

// Errors callers can branch on.
var (
	// ErrUnsupported: not macOS, or Security.framework failed to load.
	ErrUnsupported = errors.New("secure enclave: unsupported on this platform")
	// ErrNoKey: no key with the requested tag exists yet.
	ErrNoKey = errors.New("secure enclave: no token key found")
	// ErrCanceled: the user dismissed the authentication dialog.
	ErrCanceled = errors.New("secure enclave: authentication canceled")
	// ErrNoInteraction: no GUI session can show the dialog (SSH, headless).
	ErrNoInteraction = errors.New("secure enclave: no interactive session for Touch ID")
	// ErrMissingEntitlement: the binary's signature lacks keychain access.
	ErrMissingEntitlement = errors.New("secure enclave: binary lacks keychain entitlement (unsigned dev build?)")
)

// Sealer seals and unseals byte strings. The darwin Key implements it; tests
// substitute a fake.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Unseal(ciphertext []byte, reason string) ([]byte, error)
}
