package flock

import (
	"errors"
	"io/fs"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// openLockFileIn opens the lock file, creating it if needed, without ever
// asking for O_CREATE *without* O_EXCL.
//
// That combination is the one operation macOS gets wrong: when several callers
// race the first creation of a single name through openat(2), most of them get
// a spurious ENOENT instead of a descriptor, measured at 25-75% under
// contention, on macOS 26.6 (25G72) and 26.6.2 (25G83), reproducible in plain C
// with no Go involved. Nothing else races: O_CREATE|O_EXCL, a plain open of an
// existing file, mkdirat/symlinkat/linkat/renameat, and full-path open(2) are
// all correct, and Linux is unaffected entirely. Only openat's create-or-open
// fallback is broken.
//
// This matters here more than anywhere else in the tree, because a lock nobody
// can take is a lock that silently stops serializing: callers saw
// "acquire state lock: ... no such file or directory" and concurrent session
// state merges were lost. The pre-os.Root code used a full path, so this is a
// regression the root migration would otherwise introduce on the platform we
// develop on.
//
// Splitting create-or-open into an exclusive create plus a plain open avoids
// the broken path entirely, and is deterministic rather than a retry: measured
// 0 failures in 6000 racing opens where the single-call form failed 4806 times.
// The loop covers only the vanishingly rare case of the file being removed
// between the two steps. Lock files are never unlinked (see ClearSessionState),
// so in practice it runs once.
//
// Both steps go through osroot.OpenFileNoFollow rather than root.OpenFile. An
// os.Root refuses a symlink that escapes it but follows one pointing elsewhere
// inside it, and the plain-open fallback is reached precisely when something
// already exists at the name, which is where a planted link would sit. No
// caller can be reached that way today: every AcquireIn name resolves through a
// git common dir root, and git will not check a path out into .git. The refusal
// is here so that stays true if a worktree-anchored caller is ever added, since
// the worktree is the one tree a checkout does control. It keeps the two-step
// split intact, so the macOS behaviour above is unaffected.
func openLockFileIn(root *os.Root, name string) (*os.File, error) {
	var err error
	for range 3 {
		var f *os.File
		f, err = osroot.OpenFileNoFollow(root, name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err //nolint:wrapcheck // AcquireContextIn wraps this; wrapping twice would bury the cause
		}
		// It exists now, so open it without O_CREATE.
		f, err = osroot.OpenFileNoFollow(root, name, os.O_RDWR, 0)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // AcquireContextIn wraps this; wrapping twice would bury the cause
		}
		// Removed between the two steps; start over.
	}
	return nil, err //nolint:wrapcheck // AcquireContextIn wraps this; wrapping twice would bury the cause
}
