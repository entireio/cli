//go:build unix

package lockfile

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockExclusive(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) //nolint:gosec // G115: uintptr->int is safe for fd
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrLocked
	}
	return fmt.Errorf("flock LOCK_EX|LOCK_NB: %w", err)
}

func unlock(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil { //nolint:gosec // G115: uintptr->int is safe for fd
		return fmt.Errorf("flock LOCK_UN: %w", err)
	}
	return nil
}
