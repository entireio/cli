package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrFileFallbackFailed marks the case where the OS keyring refused a write
// *and* the file store could not take it either. Callers use it to suppress
// "try the file store instead" guidance that has already been tried.
var ErrFileFallbackFailed = errors.New("keyring unavailable and file token store write failed")

// fallbackWarnW receives the one-time file-fallback warning. Package-level so
// tests can capture it; production writes to stderr, matching the
// loose-permissions warning in file.go.
var fallbackWarnW io.Writer = os.Stderr

var (
	fallbackMu   sync.Mutex
	fallbackPath string // set once credentials were written to the file store because the keyring refused them
)

// FellBackToFileStore reports whether this process stored credentials in the
// file backend after the OS keyring refused them, and where. User-facing
// provenance (e.g. `entire auth status`) uses it so the reported location is
// where the tokens actually are.
func FellBackToFileStore() (path string, ok bool) {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	return fallbackPath, fallbackPath != ""
}

// resetFileFallbackForTesting clears the process-wide fallback marker so a
// test asserting on the one-time warning starts from a known state.
func resetFileFallbackForTesting() {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	fallbackPath = ""
}

// noteFileFallback records that a credential landed in the file store and
// warns — once per process — that it is on disk rather than in the keyring.
// The user chose neither, so they get told: the tokens are bearer credentials,
// and where they live is the user's call to review.
func noteFileFallback(path string, keyringErr error) {
	fallbackMu.Lock()
	first := fallbackPath == ""
	fallbackPath = path
	fallbackMu.Unlock()

	if !first {
		return
	}
	fmt.Fprintf(fallbackWarnW,
		"Warning: OS keyring (%s) is unavailable: %v\nCredentials saved to %s instead (mode 0600). Set %s=file to skip the keyring, or %s=keyring to fail instead of writing a file.\n",
		keyringProviderName(), keyringErr, path, BackendEnvVar, BackendEnvVar)
}

// fallbackStore is the default backend: the OS keyring, with the file store
// standing in whenever the keyring is unusable.
//
// The fallback exists because the alternative is throwing away a login the
// user already completed. `entire login` finishes the whole device/browser
// flow — code entered, browser approved — and only then writes tokens; on a
// machine with no Secret Service (headless server, container, plain SSH box)
// that write failed with `exec: "dbus-launch": executable file not found`,
// the command exited 1, and the user was told to re-run the entire flow with
// ENTIRE_TOKEN_STORE=file. Storing the tokens they just earned, in the
// documented 0600 file, is strictly better than discarding them — announced
// on stderr (noteFileFallback), never silent.
//
// The keyring stays first, so a working keyring is always preferred and a
// machine that grows one later goes back to using it. ENTIRE_TOKEN_STORE
// overrides the whole arrangement in either direction: "file" skips the
// keyring attempt, "keyring" requires it and fails rather than writing a file.
type fallbackStore struct {
	keyring store
	file    *fileStore

	// mu guards unusable, a process-local latch: once the keyring has failed
	// there is no point paying for it again (a hung provider costs up to
	// keyringTimeout per call, and a CLI run makes several).
	mu       sync.Mutex
	unusable bool
}

func (s *fallbackStore) keyringUsable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.unusable
}

func (s *fallbackStore) markKeyringUnusable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unusable = true
}

// interruptedKeyringCall reports whether err is the wrapped context.Canceled
// that callKeyringWithTimeout returns when the user pressed Ctrl-C. That is a
// user aborting the command, not a broken keyring: it must propagate
// untouched (main turns it into the quiet SIGINT exit) and must never be
// answered by writing a token to disk. A timeout wraps DeadlineExceeded
// instead, which does mean "unusable".
func interruptedKeyringCall(err error) bool {
	return errors.Is(err, context.Canceled)
}

// Get prefers the keyring and consults the file store when the keyring has no
// answer. A keyring miss (ErrNotFound) still checks the file, because that is
// exactly where an earlier fallback write would have put the entry. A keyring
// *failure* with nothing in the file surfaces the keyring error rather than
// ErrNotFound, so a broken keyring never masquerades as "not logged in".
func (s *fallbackStore) Get(service, user string) (string, error) {
	var keyringErr error
	if s.keyringUsable() {
		value, err := s.keyring.Get(service, user)
		switch {
		case err == nil:
			return value, nil
		case interruptedKeyringCall(err):
			return "", err //nolint:wrapcheck // user abort propagates verbatim
		case errors.Is(err, ErrNotFound):
			// Keyring is healthy and simply holds no entry.
		default:
			s.markKeyringUnusable()
			keyringErr = err
		}
	}

	value, fileErr := s.file.Get(service, user)
	if fileErr == nil {
		return value, nil
	}
	if keyringErr != nil {
		return "", keyringErr
	}

	return "", fileErr
}

// Set writes to the keyring, falling back to the file store when it refuses.
func (s *fallbackStore) Set(service, user, password string) error {
	if s.keyringUsable() {
		err := s.keyring.Set(service, user, password)
		if err == nil {
			return nil
		}
		if interruptedKeyringCall(err) {
			return err //nolint:wrapcheck // user abort propagates verbatim
		}
		s.markKeyringUnusable()
		if fileErr := s.file.Set(service, user, password); fileErr != nil {
			return fmt.Errorf("%w: keyring: %w; file %s: %w", ErrFileFallbackFailed, err, s.file.path, fileErr)
		}
		noteFileFallback(s.file.path, err)
		return nil
	}

	// The keyring already failed earlier in this process; the warning has
	// been printed and the file store is where this belongs.
	if err := s.file.Set(service, user, password); err != nil {
		return err
	}
	return nil
}

// Delete clears the credential from both backends: a fallback write may have
// left a copy in either, and logout has to remove all of them. It succeeds if
// any backend had the entry and removed it, reports ErrNotFound only when
// every backend agreed there was nothing to remove, and otherwise surfaces
// the first real failure.
func (s *fallbackStore) Delete(service, user string) error {
	deleted := false
	var firstErr error

	note := func(err error) {
		switch {
		case err == nil:
			deleted = true
		case errors.Is(err, ErrNotFound):
			// Nothing here to remove.
		case firstErr == nil:
			firstErr = err
		}
	}

	if s.keyringUsable() {
		err := s.keyring.Delete(service, user)
		if interruptedKeyringCall(err) {
			return err //nolint:wrapcheck // user abort propagates verbatim
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.markKeyringUnusable()
		}
		note(err)
	}
	note(s.file.Delete(service, user))

	switch {
	case deleted:
		return nil
	case firstErr != nil:

		return firstErr
	default:
		return ErrNotFound
	}
}
