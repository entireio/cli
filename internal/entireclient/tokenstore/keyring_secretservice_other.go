//go:build !((dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd)

package tokenstore

// classifyKeyringError passes errors through: only the Secret Service
// keyring (keyring_secretservice.go) has a "no provider" state to classify.
func classifyKeyringError(err error) error { return err }
