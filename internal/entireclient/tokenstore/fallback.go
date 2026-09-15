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
// or container usually lacks. On these platforms an unavailable keyring is
// treated as absent — including a present-but-locked collection, since without
// a prompter the CLI cannot tell the two apart; the notice is what makes that
// acceptable. On macOS and Windows the keyring is always present, so a failure
// is a denied prompt or a locked store and must not be worked around with a
// plaintext file. keyringProviderName reads the same set, so the two cannot
// drift.
var secretServicePlatforms = map[string]bool{
	"linux": true, "freebsd": true, "openbsd": true, "netbsd": true, "dragonfly": true,
}

func isSecretServicePlatform(goos string) bool { return secretServicePlatforms[goos] }

// fallbackEligible reports whether a keyring error means "no usable keyring"
// rather than "no such credential" (ErrNotFound) or "the user interrupted us"
// (a Ctrl-C surfaces as context.Canceled from callKeyringWithTimeout). A
// timeout counts as unavailable: a Secret Service that never answers is no
// better than one that is absent — but a timeout is never remembered, because
// the abandoned keyring call may still complete; see switchTo. Everything
// else a Linux keyring call can return — no session bus, no provider on the
// bus, a collection that will not unlock, ErrUnsupportedPlatform on a FreeBSD
// or DragonFly build without cgo (NetBSD and OpenBSD get D-Bus regardless) —
// is an availability failure, so there is deliberately no string matching
// here.
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
// fallback has proven the file store is where the credentials are. It also
// raises adoptedFile, so provenance (selectedBackend) follows the switch even
// when no marker records it — the ENTIRE_TOKEN_STORE_PATH case.
func setBackend(s store) {
	backendMu.Lock()
	backend = s
	resolved = true
	adoptedFile = true
	backendMu.Unlock()
}

// fallbackStore fronts the OS keyring on Secret Service platforms. Each
// operation goes to the keyring first; when the keyring fails for an
// availability reason, the same operation is retried against the file store
// at FileBackendPath (ENTIRE_TOKEN_STORE_PATH when set, else tokens.json in
// the config dir). The file store is adopted as the process backend — and,
// unless ENTIRE_TOKEN_STORE_PATH is set or the keyring merely timed out,
// remembered for future processes — only once it has proven it holds (Get,
// Delete) or now holds (Set) the credential. A miss in both stores proves
// nothing about where future tokens should go and leaves no trace beyond the
// warning Delete prints.
//
// The transition is announced once per process by the notice on stderr, and
// every login onto the file store prints where the tokens went (see
// persistLogin in the cli package), so "your tokens are in a file now" is
// never silent. The invariant that makes this safe is in
// isSecretServicePlatform: the fallback is constructed only where an
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
	// deleteMissWarn dedupes the warning for a Delete that missed in both
	// stores: logout clears several slots per context, and one warning per
	// process says everything the repeats would.
	deleteMissWarn sync.Once
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
		// keyring has vanished. But "cannot reach" is not "gone": the
		// keyring may be locked, unreachable from this session, or slow,
		// and still hold the credential — so the miss is warned about once,
		// even though it is not an error. Any other file error is reported
		// with both.
		if errors.Is(ferr, ErrNotFound) {
			f.deleteMissWarn.Do(func() {
				fmt.Fprintf(fallbackNoticeW, "Warning: OS keyring (%s) unavailable: %v\nCould not confirm this credential was removed from it. If this machine's keyring still holds Entire credentials, run entire logout again from a session with keyring access.\n",
					keyringProviderName(), err)
			})
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
// A keyring that TIMED OUT is adopted for this process only, never
// remembered. callKeyringWithTimeout abandons the goroutine rather than
// cancelling it, so the keyring may still complete the write once it answers;
// pinning the file store through the marker would then orphan that keyring
// copy for good. The notice says so and names the two variables that avoid
// the wait.
//
// Adopting on a successful Get is deliberate: it is what makes the customer's
// first command after losing the keyring work without a re-login. It has a
// known cost. A later explicit keyring login removes that credential's copy
// from the default-path file (removeSupersededFileCopy), so the re-adopt
// cycle — a transient keyring failure finds a stale token in the file, adopts
// it, and re-writes the marker — survives only for other contexts' entries
// still in the file and for a store named through ENTIRE_TOKEN_STORE_PATH,
// which is never touched. The notice is the only signal, which is why the
// remembered branch always prints the way back.
func (f *fallbackStore) switchTo(fs store, keyringErr error) {
	timedOut := errors.Is(keyringErr, context.DeadlineExceeded)
	remembered := markerApplies() && !timedOut
	f.notice.Do(func() {
		fmt.Fprintf(fallbackNoticeW, "Note: OS keyring (%s) unavailable: %v\nStoring Entire tokens in %s instead. ",
			keyringProviderName(), keyringErr, FileBackendPath())
		switch {
		case timedOut:
			fmt.Fprintf(fallbackNoticeW, "The keyring timed out rather than failing, so this choice is not remembered, and the keyring may also have received this credential once it answered. Set %s=%s to skip the keyring check, or %s to wait longer.\n", BackendEnvVar, backendFile, keyringTimeoutEnvVar)
		case remembered:
			fmt.Fprintf(fallbackNoticeW, "This choice is remembered; run %s=%s entire login to switch back.\n", BackendEnvVar, backendKeyring)
		default:
			fmt.Fprintf(fallbackNoticeW, "%s is set, so this choice is not remembered; keep it set for later commands, and set %s=%s as well to skip the keyring check.\n", PathEnvVar, BackendEnvVar, backendFile)
		}
	})
	if remembered {
		if err := rememberBackend(backendFile); err != nil {
			f.rememberWarn.Do(func() {
				fmt.Fprintf(fallbackNoticeW, "Warning: could not remember the token store choice: %v\nLater commands may not find this login without %s=%s; fix the Entire config directory and run entire login again to remember the choice.\n", err, BackendEnvVar, backendFile)
			})
		}
	}
	f.adopt(fs)
}
