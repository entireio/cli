//go:build unix

package strategy

import "os"

func removeAcquiredCleanupLockFile(path string, release func()) error {
	err := os.Remove(path)
	release()
	return err
}
