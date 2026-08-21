package cli

import "github.com/entireio/cli/internal/coreapi"

// RenderUserFacingError cleans an error for display: it drops the
// `security "BearerAuth": security source "BearerAuth":` segments ogen's
// generated control-plane client wraps every auth failure in, which name the
// OpenAPI security scheme that failed to resolve and so say nothing a user can
// act on (see coreapi.StripSecurityWrapping).
//
// It exists as a named function because there is more than one place the CLI
// turns an error into text, and they can't share one: main.go prints the error
// a command returns, renderCoreError feeds the mirror-create wizard (which
// prints its own errors and returns a SilentError, so main.go never sees them),
// and the search TUI captures err.Error() into its model before the command
// returns nil. Call this at any new site that renders an error for a user
// rather than adding another strip.
func RenderUserFacingError(err error) error {
	return coreapi.StripSecurityWrapping(err)
}

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
