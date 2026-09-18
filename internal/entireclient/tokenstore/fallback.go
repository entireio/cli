package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// fallbackNoticeW receives every store warning: the notice printed when the
// keyring is found unavailable and tokens move to the file store, the
// could-not-remember warning, the superseded-copy warning, and the
// unusable-marker warning. Package-level so
// tests can capture it; production writes to stderr, as loosePermsWarnW does
// for the loose-permissions warning.
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
// file store failed too. Login's withHeadlessStoreHint and auth status's
// storeReadError both check for it and point at ENTIRE_TOKEN_STORE_PATH
// instead of recommending the file store that just failed, which would send
// the user in a circle. Both underlying errors are wrapped alongside it.
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
// unless ENTIRE_TOKEN_STORE_PATH is set or a write merely timed out,
// remembered for future processes — only once it has proven it holds (Get,
// Delete) or now holds (Set) the credential. A miss in both stores proves
// nothing about where future tokens should go and leaves no trace.
//
// The transition is announced once per process by the notice on stderr, and
// every login onto the file store prints where the tokens went (see
// persistLogin in the cli package), so "your tokens are in a file now" is
// never silent. The fallback is constructed only on the platforms where an
// unavailable keyring is treated as absent (see secretServicePlatforms for
// why that is acceptable).
//
// Once the keyring has answered for an account in this process — a success,
// or an ErrNotFound, which also proves it is reachable — no later call for
// that account falls back: a keyring that worked a moment ago is present, so
// the failure is transient, and reporting it beats moving half of a login
// elsewhere. Login writes the refresh and access slots as two calls under one
// user, and falling back on only the second would leave the refresh token in
// the keyring and the access token in the file, which the next process cannot
// reassemble. The latch is per user rather than per process because logout
// --all-contexts walks every saved account in one process, and an account
// whose credential lives only in the file must still be reachable there.
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
	// answered holds the users for which the keyring has answered; see
	// mayFallBack.
	answered sync.Map
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

// mayFallBack reports whether a keyring result allows the retry against the
// file store, and records a keyring that answered for user so that no later
// call for that user in this process falls back (see the type comment).
func (f *fallbackStore) mayFallBack(user string, err error) bool {
	if err == nil || errors.Is(err, ErrNotFound) {
		f.answered.Store(user, struct{}{})
		return false
	}
	if !fallbackEligible(err) {
		return false
	}
	_, answered := f.answered.Load(user)
	return !answered
}

func (f *fallbackStore) Get(service, user string) (string, error) {
	v, err := f.primary.Get(service, user)
	if !f.mayFallBack(user, err) {
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
	f.switchTo(fs, err, false)
	return fv, nil
}

func (f *fallbackStore) Set(service, user, password string) error {
	err := f.primary.Set(service, user, password)
	if !f.mayFallBack(user, err) {
		//nolint:wrapcheck // thin wrapper, callers handle errors
		return err
	}
	fs := f.file()
	if ferr := fs.Set(service, user, password); ferr != nil {
		return bothFailed(err, ferr)
	}
	f.switchTo(fs, err, true)
	return nil
}

func (f *fallbackStore) Delete(service, user string) error {
	err := f.primary.Delete(service, user)
	if !f.mayFallBack(user, err) {
		//nolint:wrapcheck // thin wrapper, callers handle errors
		return err
	}
	fs := f.file()
	ferr := fs.Delete(service, user)
	if ferr != nil {
		// A miss here is ErrNotFound, deliberately, unlike Get: Delete's
		// job is "make sure it is gone from wherever we can reach", and
		// logout must be able to remove a context on a machine whose
		// keyring has vanished. "Cannot reach" is not "gone", but this
		// store cannot tell a logout from login's best-effort clear of a
		// stale slot (the first store call of a device-flow login on a
		// keyring-less machine), so it says nothing here; logout reports an
		// unreachable store itself, from statusTarget.storeErr. Any other
		// file error is reported with both.
		if errors.Is(ferr, ErrNotFound) {
			return ErrNotFound
		}
		return bothFailed(err, ferr)
	}
	f.switchTo(fs, err, false)
	return nil
}

// switchTo adopts the file store after it proved itself. The notice prints once
// per process. The marker write is attempted on every adoption, not once:
// rememberBackend is idempotent and cheap once the marker exists, and a
// transient failure on the first attempt should not leave the machine
// unremembered for the rest of the process.
//
// A keyring that TIMED OUT on a WRITE is adopted for this process only,
// never remembered. callKeyringWithTimeout abandons the goroutine rather than
// cancelling it, so the keyring may still complete the write once it answers;
// pinning the file store through the marker would then orphan that keyring
// copy for good. A timed-out Get or Delete orphans nothing (a discarded value;
// a removal that can only make the keyring copy more gone), so it is
// remembered like any other availability failure. Otherwise a keyring that
// hangs rather than fails would cost the full timeout and reprint the notice
// on every command, with no way to heal. The notice says so and names the two variables that
// avoid the wait; when ENTIRE_TOKEN_STORE_PATH is set as well it adds that the
// variable has to stay set, so the timeout does not swallow the path advice.
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
func (f *fallbackStore) switchTo(fs store, keyringErr error, write bool) {
	timedOut := write && errors.Is(keyringErr, context.DeadlineExceeded)
	pathSet := !markerApplies()
	remembered := !pathSet && !timedOut
	f.notice.Do(func() {
		fmt.Fprintf(fallbackNoticeW, "Note: OS keyring (%s) unavailable: %v\nStoring Entire tokens in %s instead. ",
			keyringProviderName(), keyringErr, FileBackendPath())
		switch {
		case timedOut:
			fmt.Fprintf(fallbackNoticeW, "The keyring timed out rather than failing, so this choice is not remembered: the abandoned keyring call may still complete. Set %s=%s to skip the keyring check, or %s to wait longer.", BackendEnvVar, backendFile, keyringTimeoutEnvVar)
			if pathSet {
				fmt.Fprintf(fallbackNoticeW, " %s is set; keep it set for later commands.", PathEnvVar)
			}
			fmt.Fprintln(fallbackNoticeW)
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
