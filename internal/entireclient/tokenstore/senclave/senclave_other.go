//go:build !darwin

package senclave

// Key is unavailable off macOS.
type Key struct{}

// Generate reports ErrUnsupported.
func Generate() ([]byte, error) { return nil, ErrUnsupported }

// Load reports ErrUnsupported.
func Load([]byte) (*Key, error) { return nil, ErrUnsupported }

// PublicKey reports ErrUnsupported.
func (*Key) PublicKey() ([]byte, error) { return nil, ErrUnsupported }

// Seal reports ErrUnsupported.
func (*Key) Seal([]byte) ([]byte, error) { return nil, ErrUnsupported }

// Unseal reports ErrUnsupported.
func (*Key) Unseal([]byte, string) ([]byte, error) { return nil, ErrUnsupported }

// Close is a no-op.
func (*Key) Close() {}
