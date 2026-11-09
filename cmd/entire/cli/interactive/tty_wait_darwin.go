//go:build darwin

package interactive

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// WaitForInput waits up to timeout for prompt input to read, and reports
// whether it arrived. On macOS /dev/tty takes no read deadline (kqueue rejects
// it, so Go leaves it unpolled), but select works on it, and waiting before the
// read leaves nothing reading the terminal when the prompt gives up.
func (t *PromptTTY) WaitForInput(timeout time.Duration) (bool, error) {
	raw, err := t.in.SyscallConn()
	if err != nil {
		return false, fmt.Errorf("prompt terminal descriptor: %w", err)
	}
	var ready bool
	var waitErr error
	if err := raw.Control(func(fd uintptr) {
		ready, waitErr = selectReadable(int(fd), timeout)
	}); err != nil {
		return false, fmt.Errorf("prompt terminal descriptor: %w", err)
	}
	return ready, waitErr
}

func selectReadable(fd int, timeout time.Duration) (bool, error) {
	if fd >= unix.FD_SETSIZE {
		return false, fmt.Errorf("descriptor %d is beyond select's limit", fd)
	}
	deadline := time.Now().Add(timeout)
	for {
		var set unix.FdSet
		set.Set(fd)
		tv := unix.NsecToTimeval(max(time.Until(deadline), 0).Nanoseconds())
		_, err := unix.Select(fd+1, &set, nil, nil, &tv)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("wait for prompt input: %w", err)
		}
		return set.IsSet(fd), nil
	}
}
