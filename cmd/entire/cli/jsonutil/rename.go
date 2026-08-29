package jsonutil

import (
	"os"
	"time"
)

// retryRename's bounds. With the delay doubling from renameInitialDelay the
// total wait is ~155ms: long enough to outlast an antivirus scan or another
// hook's read of the file being replaced, short enough that a hook running
// under its host's deadline is not held hostage by a stuck handle.
const (
	renameAttempts     = 6
	renameInitialDelay = 5 * time.Millisecond
)

// renameFile renames oldpath onto newpath, retrying transient platform
// errors.
//
// On POSIX, rename(2) over a file another process has open always succeeds —
// the old inode lives on for that reader — so this is a plain os.Rename. On
// Windows the destination is replaced only while no other handle is open on
// it: a concurrent reader (a second hook, an editor, Defender's on-access
// scan) fails the rename with ERROR_SHARING_VIOLATION or ERROR_ACCESS_DENIED
// for as long as it holds the handle, which is the failure behind #455. Those
// are retried with backoff; anything else surfaces at once.
func renameFile(oldpath, newpath string) error {
	return retryRename(func() error { return os.Rename(oldpath, newpath) }, renameIsTransient, time.Sleep)
}

// retryRename runs rename until it succeeds, returns a non-transient error,
// or renameAttempts is exhausted; the last error is returned. sleep is
// injected so the backoff schedule is testable without waiting it out.
func retryRename(rename func() error, transient func(error) bool, sleep func(time.Duration)) error {
	delay := renameInitialDelay
	for attempt := 1; ; attempt++ {
		err := rename()
		if err == nil || !transient(err) || attempt == renameAttempts {
			return err
		}
		sleep(delay)
		delay *= 2
	}
}
