//go:build windows

package jsonutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// SyncDir flushes a directory when Windows supports it and treats documented
// unsupported-directory-sync errors as a successful best-effort operation.
func SyncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // caller selects the directory whose entry it just published
	if err != nil {
		return fmt.Errorf("opening directory for sync: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil &&
		!errors.Is(syncErr, windows.ERROR_INVALID_FUNCTION) &&
		!errors.Is(syncErr, windows.ERROR_ACCESS_DENIED) &&
		!errors.Is(syncErr, windows.ERROR_NOT_SUPPORTED) {
		return fmt.Errorf("syncing directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing synced directory: %w", closeErr)
	}
	return nil
}
