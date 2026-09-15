package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
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

// fallbackEligible reports whether a keyring error means "no usable keyring"
// rather than "no such credential" (ErrNotFound) or "the user interrupted us"
// (a Ctrl-C surfaces as context.Canceled from callKeyringWithTimeout). A
// timeout counts as unavailable: a Secret Service that never answers is no
// better than one that is absent. Everything else a Linux keyring call can
// return — no session bus, no provider on the bus, a collection that will not
// unlock, ErrUnsupportedPlatform on a cgo-less BSD — is an availability
// failure, so there is deliberately no string matching here.
func fallbackEligible(err error) bool {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// ErrFileStoreFailed marks a fallback that could not complete because the
// file store failed too. Login's headless hint checks for it: suggesting
// ENTIRE_TOKEN_STORE=file about a file store that just failed would send the
// user in a circle. Both underlying errors are wrapped alongside it.
var ErrFileStoreFailed = errors.New("file token store failed")

// bothFailed composes the error for a fallback that could not complete: the
// keyring failure, the ErrFileStoreFailed sentinel, and the file store's own
// error, each reachable through errors.Is.
func bothFailed(keyringErr, fileErr error) error {
	return fmt.Errorf("OS keyring (%s) unavailable: %w; %w: %w", keyringProviderName(), keyringErr, ErrFileStoreFailed, fileErr)
}

// setBackend makes s the process backend without re-resolving. Used when the
// fallback has proven the file store is where the credentials are.
func setBackend(s store) {
	backendMu.Lock()
	backend = s
	resolved = true
	backendMu.Unlock()
}

// fallbackStore fronts the OS keyring on Secret Service platforms. Each
// operation goes to the keyring first; when the keyring fails for an
// availability reason, the same operation is retried against the default-path
// file store. The file store is adopted as the process backend — and, unless
// ENTIRE_TOKEN_STORE_PATH is set, remembered for future processes — only once
// it has proven it holds (Get, Delete) or now holds (Set) the credential. A
// miss in both stores proves nothing about where future tokens should go and
// leaves no trace.
//
// The notice is printed once per process, on stderr, so that "your tokens
// are in a file now" is never silent. The invariant that makes this safe is
// in isSecretServicePlatform: the fallback is constructed only where an
// unavailable keyring means the machine has none.
type fallbackStore struct {
	primary store
	// newFile builds the file store; injectable for tests. file() memoizes
	// the result so every retry in this process and the eventual adoption
	// see one instance: otherwise concurrent misses could hand setBackend
	// different *fileStore values, and each fresh instance would re-print
	// the loose-permissions warning.
	newFile  func() store
	fileOnce sync.Once
	fs       store
	// adopt installs the file store as the process backend; injectable so
	// tests can observe it without touching package state.
	adopt  func(store)
	notice sync.Once
	// rememberWarn dedupes the could-not-remember warning; the marker write
	// itself is retried on every adoption (see switchTo).
	rememberWarn sync.Once
}

func newFallbackStore(primary store) *fallbackStore {
	return &fallbackStore{
		primary: primary,
		newFile: func() store { return defaultFileStore() },
		adopt:   setBackend,
	}
}

func (f *fallbackStore) file() store {
	f.fileOnce.Do(func() { f.fs = f.newFile() })
	return f.fs
}

func (f *fallbackStore) Get(service, user string) (string, error) {
	v, err := f.primary.Get(service, user)
	if !fallbackEligible(err) {
		//nolint:wrapcheck // thin wrapper, callers handle errors
		return v, err
	}
	fs := f.file()
	fv, ferr := fs.Get(service, user)
	if ferr != nil {
		if errors.Is(ferr, ErrNotFound) {
			// Neither store has it. Returning ErrNotFound would make every
			// caller say "not logged in" about a context whose credential
			// exists somewhere we cannot reach; the keyring error is the
			// honest answer, and auth status renders it as such.
			return "", fmt.Errorf("OS keyring (%s) unavailable: %w (and no credential in %s)", keyringProviderName(), err, FileBackendPath())
		}
		return "", bothFailed(err, ferr)
	}
	f.switchTo(fs, err)
	return fv, nil
}

func (f *fallbackStore) Set(service, user, password string) error {
	err := f.primary.Set(service, user, password)
	if !fallbackEligible(err) {
		//nolint:wrapcheck // thin wrapper, callers handle errors
		return err
	}
	fs := f.file()
	if ferr := fs.Set(service, user, password); ferr != nil {
		return bothFailed(err, ferr)
	}
	f.switchTo(fs, err)
	return nil
}

func (f *fallbackStore) Delete(service, user string) error {
	err := f.primary.Delete(service, user)
	if !fallbackEligible(err) {
		//nolint:wrapcheck // thin wrapper, callers handle errors
		return err
	}
	fs := f.file()
	ferr := fs.Delete(service, user)
	if ferr != nil {
		// A miss here is ErrNotFound, deliberately, unlike Get: Delete's
		// job is "make sure it is gone from wherever we can reach", and
		// logout must be able to remove a context on a machine whose
		// keyring has vanished. Any other file error is reported with both.
		if errors.Is(ferr, ErrNotFound) {
			return ErrNotFound
		}
		return bothFailed(err, ferr)
	}
	f.switchTo(fs, err)
	return nil
}

// switchTo adopts the file store after it proved itself. The notice prints once
// per process. The marker write is attempted on every adoption, not once:
// rememberBackend is idempotent and cheap once the marker exists, and a
// transient failure on the first attempt should not leave the machine
// unremembered for the rest of the process.
//
// Adopting on a successful Get is deliberate and has a known cost. It is what
// makes the customer's first command after losing the keyring work without a
// re-login. But tokens.json is never deleted when a later explicit keyring
// login clears the marker, and it survives the switch back, so a stale copy
// lingers and the cycle can recur on the next transient keyring failure until
// the file itself is removed; if the keyring then fails transiently (a
// timeout, a locked collection), a Get finds the stale token, adopts it, and
// re-writes the marker, pinning the machine to it until the user notices 401s
// and runs ENTIRE_TOKEN_STORE=keyring entire login. The notice is the only
// signal, which is why the remembered branch always prints the way back; the
// ENTIRE_TOKEN_STORE_PATH branch remembers nothing, so there is nothing to
// undo.
func (f *fallbackStore) switchTo(fs store, keyringErr error) {
	remembered := markerApplies()
	f.notice.Do(func() {
		fmt.Fprintf(fallbackNoticeW, "Note: OS keyring (%s) unavailable: %v\nStoring Entire tokens in %s instead. ",
			keyringProviderName(), keyringErr, FileBackendPath())
		if remembered {
			fmt.Fprintf(fallbackNoticeW, "This choice is remembered; run %s=%s entire login to switch back.\n", BackendEnvVar, backendKeyring)
		} else {
			fmt.Fprintf(fallbackNoticeW, "%s is set, so this choice is not remembered; keep it set for later commands, and set %s=%s as well to skip the keyring check.\n", PathEnvVar, BackendEnvVar, backendFile)
		}
	})
	if remembered {
		if err := rememberBackend(backendFile); err != nil {
			f.rememberWarn.Do(func() {
				fmt.Fprintf(fallbackNoticeW, "Warning: could not remember the token store choice: %v\n", err)
			})
		}
	}
	f.adopt(fs)
}
