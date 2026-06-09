package interactive

import (
	"fmt"
	"os"
)

// PromptTTY is a platform prompt terminal opened for both input and output.
type PromptTTY struct {
	in  *os.File
	out *os.File
}

// Read reads prompt input.
func (t *PromptTTY) Read(p []byte) (int, error) {
	n, err := t.in.Read(p)
	if err != nil {
		return n, fmt.Errorf("read prompt terminal: %w", err)
	}
	return n, nil
}

// Write writes prompt output.
func (t *PromptTTY) Write(p []byte) (int, error) {
	n, err := t.out.Write(p)
	if err != nil {
		return n, fmt.Errorf("write prompt terminal: %w", err)
	}
	return n, nil
}

// Close closes the prompt terminal handles.
func (t *PromptTTY) Close() error {
	err := t.in.Close()
	if t.out != t.in {
		if outErr := t.out.Close(); err == nil {
			err = outErr
		}
	}
	return err
}
