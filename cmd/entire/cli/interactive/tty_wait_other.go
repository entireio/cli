//go:build !darwin

package interactive

import (
	"errors"
	"time"
)

// WaitForInput is needed only on macOS, where /dev/tty takes no read deadline.
// Elsewhere it is unsupported and callers rely on SetReadDeadline.
func (t *PromptTTY) WaitForInput(time.Duration) (bool, error) {
	return false, errors.ErrUnsupported
}
