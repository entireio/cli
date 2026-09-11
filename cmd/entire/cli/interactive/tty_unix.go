//go:build unix

package interactive

import (
	"fmt"
	"os"
)

// OpenPromptTTY opens the controlling terminal for interactive prompts.
func OpenPromptTTY() (*PromptTTY, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/tty: %w", err)
	}
	return &PromptTTY{in: tty, out: tty}, nil
}
