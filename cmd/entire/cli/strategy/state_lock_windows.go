//go:build windows

package strategy

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// acquireStateFileLock takes an exclusive lock on path via Windows
// LockFileEx. The returned release unlocks and closes the file. Callers
// must call release exactly once.
func acquireStateFileLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path built from validated session ID
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock state lock: %w", err)
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		_ = f.Close()
	}, nil
}
