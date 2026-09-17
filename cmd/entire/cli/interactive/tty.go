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

// Output returns the terminal output handle. Callers that hand terminal output
// to a library such as Bubble Tea need the concrete file so it can enable the
// console's VT processing.
func (t *PromptTTY) Output() *os.File {
	return t.out
}

// Close closes the prompt terminal handles. A read still pending on the input
// is released first: os.File.Close waits for it, and on Windows a console read
// only completes on a keypress, so a prompt whose reader loop had already
// issued its next read (Bubble Tea's, after the answer) would otherwise make
// the user press a key a second time. See releasePendingReads. A release
// failure is reported alongside the close, never instead of it: the handles
// are closed regardless.
func (t *PromptTTY) Close() error {
	var errs []error
	if err := releasePendingReads(t.in); err != nil {
		errs = append(errs, fmt.Errorf("release prompt terminal input: %w", err))
	}
	if t.in == t.out {
		if err := t.in.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close prompt terminal: %w", err))
		}
		return errors.Join(errs...)
	}
	if err := t.in.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close prompt terminal input: %w", err))
	}
	if err := t.out.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close prompt terminal output: %w", err))
	}
	return errors.Join(errs...)
}
