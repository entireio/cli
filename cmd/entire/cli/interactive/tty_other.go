//go:build !unix && !windows

package interactive

import (
	"errors"
)

// OpenPromptTTY opens the platform prompt terminal for interactive prompts.
func OpenPromptTTY() (*PromptTTY, error) {
	return nil, errors.New("prompt terminal is unsupported on this platform")
}
