package interactive

import (
	"errors"
	"fmt"
	"os"
)

// PromptTTY is the platform's controlling terminal, opened for prompt input and
// output independently where the platform requires separate handles.
type PromptTTY struct {
	in  *os.File
	out *os.File
}

// Input returns the terminal input handle. Callers that hand terminal input to
// a library such as Bubble Tea need the concrete file so it can manage raw mode.
func (t *PromptTTY) Input() *os.File {
	return t.in
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
	if t.in == t.out {
		if err := t.in.Close(); err != nil {
			return fmt.Errorf("close prompt terminal: %w", err)
		}
		return nil
	}
	var errs []error
	if err := t.in.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close prompt terminal input: %w", err))
	}
	if err := t.out.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close prompt terminal output: %w", err))
	}
	return errors.Join(errs...)
}
