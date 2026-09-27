//go:build !unix && !windows

package interactive

import "errors"

// OpenPromptTTY reports that this platform has no supported controlling-terminal device.
func OpenPromptTTY() (*PromptTTY, error) {
	return nil, errors.New("prompt terminal is unsupported on this platform")
}
