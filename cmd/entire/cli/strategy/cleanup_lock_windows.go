//go:build windows

package strategy

import "os"

func removeAcquiredCleanupLockFile(path string, release func()) error {
	release()
	return os.Remove(path)
}
