//go:build windows

package interactive

import (
	"fmt"
	"os"
)

// OpenPromptTTY opens the Windows console devices for interactive prompts.
func OpenPromptTTY() (*PromptTTY, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open CONIN$: %w", err)
	}
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("open CONOUT$: %w", err)
	}
	return &PromptTTY{in: in, out: out}, nil
}
