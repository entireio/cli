package tokenstore

import (
	"io"
	"os"
)

// fallbackNoticeW receives the short notice printed when the keyring is found
// unavailable and tokens move to the file store, and the warnings about the
// remembered preference. Package-level so tests can capture it; production
// writes to stderr like the other store warnings.
var fallbackNoticeW io.Writer = os.Stderr

// secretServicePlatforms are the GOOS values where the OS keyring is a
// separately installed daemon (Secret Service over D-Bus) that a bare server
// or container usually lacks. Only there is an unavailable keyring evidence
// that the machine has none; on macOS and Windows the keyring is always
// present, so a failure is a denied prompt or a locked store and must not be
// worked around with a plaintext file. keyringProviderName reads the same
// set, so the two cannot drift.
var secretServicePlatforms = map[string]bool{
	"linux": true, "freebsd": true, "openbsd": true, "netbsd": true, "dragonfly": true,
}

func isSecretServicePlatform(goos string) bool { return secretServicePlatforms[goos] }

// fallbackStore fronts the OS keyring on Secret Service platforms. Task 3
// gives it its behaviour; until then it passes every call through.
type fallbackStore struct {
	primary store
}

func newFallbackStore(primary store) *fallbackStore { return &fallbackStore{primary: primary} }

func (f *fallbackStore) Get(service, user string) (string, error) {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return f.primary.Get(service, user)
}

func (f *fallbackStore) Set(service, user, password string) error {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return f.primary.Set(service, user, password)
}

func (f *fallbackStore) Delete(service, user string) error {
	//nolint:wrapcheck // thin wrapper, callers handle errors
	return f.primary.Delete(service, user)
}
