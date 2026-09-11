//go:build windows

package interactive

import (
	"fmt"
	"os"
)

// OpenPromptTTY opens the Windows controlling-console devices for interactive
// prompts. Windows exposes console input and output through separate handles.
func OpenPromptTTY() (*PromptTTY, error) {
	// Bubble Tea switches the input handle into raw mode with SetConsoleMode,
	// which requires GENERIC_WRITE as well as GENERIC_READ.
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
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
