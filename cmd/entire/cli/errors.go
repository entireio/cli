package cli

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

// NewSilentError creates a SilentError wrapping the given error.
// Use this when you've already printed a user-friendly error message
// and don't want main.go to print the error again.
func NewSilentError(err error) *SilentError {
	return &SilentError{Err: err}
}

// ExitCodeError wraps an error with a specific process exit code.
// Use this when a subcommand needs to exit with a code other than 1.
// main.go checks for this type and uses ExitCode instead of the default 1.
type ExitCodeError struct {
	Err      error
	ExitCode int
}

func (e *ExitCodeError) Error() string {
	return e.Err.Error()
}

func (e *ExitCodeError) Unwrap() error {
	return e.Err
}

// NewExitCodeError creates an ExitCodeError wrapping the given error
// with the specified exit code.
func NewExitCodeError(err error, code int) *ExitCodeError {
	return &ExitCodeError{Err: err, ExitCode: code}
}
