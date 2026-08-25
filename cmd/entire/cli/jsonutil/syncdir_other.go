//go:build !unix && !windows

package jsonutil

import (
	"fmt"
	"os"
)

// SyncDir flushes directory entries on supported non-POSIX platforms.
func SyncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // caller selects the directory whose entry it just published
	if err != nil {
		return fmt.Errorf("opening directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("syncing directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("closing synced directory: %w", err)
	}
	return nil
}
