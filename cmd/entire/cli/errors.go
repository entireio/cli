package cli

import (
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/internal/coreapi"
)

// RenderUserFacingError cleans an error for display: it drops the
// `security "BearerAuth": security source "BearerAuth":` segments ogen's
// generated control-plane client wraps every auth failure in, which name the
// OpenAPI security scheme that failed to resolve and so say nothing a user can
// act on (see coreapi.StripSecurityWrapping).
//
// It also strips terminal controls from the message. Errors name stored
// session IDs, branches and paths, and carry server messages, any of which can
// hold escape sequences; tuiutil.SanitizeTerminalText keeps the line breaks
// and tabs of multi-line errors. The result still unwraps to err, so
// errors.Is/As keep matching.
//
// It exists as a named function because there is more than one place the CLI
// turns an error into text, and they can't share one: main.go prints the error
// a command returns, renderCoreError feeds the mirror-add wizard (which
// prints its own errors and returns a SilentError, so main.go never sees them),
// and the search TUI captures err.Error() into its model before the command
// returns nil. Call this at any new site that renders an error for a user
// rather than adding another strip.
func RenderUserFacingError(err error) error {
	err = coreapi.StripSecurityWrapping(err)
	if err == nil {
		return nil
	}
	msg := err.Error()
	if clean := tuiutil.SanitizeTerminalText(msg); clean != msg {
		return &renderedError{msg: clean, err: err}
	}
	return err
}

// renderedError is an error whose message was sanitized for the terminal. It
// unwraps to the original so callers can still match the chain.
type renderedError struct {
	msg string
	err error
}

func (e *renderedError) Error() string { return e.msg }

func (e *renderedError) Unwrap() error { return e.err }

// SilentError wraps an error to signal that the error message has already been
// printed to the user. main.go checks for this type to avoid duplicate output.
type SilentError struct {
	Err error
}

func (e *SilentError) Error() string {
	return e.Err.Error()
}

func (e *SilentError) Unwrap() error {
	return e.Err
}

// AlreadyPrinted reports that the user-facing message has already been written.
func (e *SilentError) AlreadyPrinted() bool {
	return true
}

// NewSilentError creates a SilentError wrapping the given error.
// Use this when you've already printed a user-friendly error message
// and don't want main.go to print the error again.
func NewSilentError(err error) *SilentError {
	return &SilentError{Err: err}
}
