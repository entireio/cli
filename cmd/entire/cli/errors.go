package cli

import (
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

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

// cliUpgradeRequiredCode is the OAuth error code the auth server returns
// when this CLI build is older than the minimum version it accepts.
const cliUpgradeRequiredCode = "cli_upgrade_required"

// WithCLIUpgradeHint appends update guidance when the server rejected a
// request because this CLI build is too old. The server signals this with
// the cli_upgrade_required OAuth error code, which auth-go surfaces as an
// unrecognised-code error whose message carries the code verbatim — there is
// no sentinel to errors.Is against, so match on the code string. Without the
// hint the bare code gives the user no way forward; the appended command is
// the same one the periodic version check advertises.
//
// The code can surface from any auth-server OAuth flow — login (device and
// browser), the refresh grant under every authenticated command, and the
// RFC 8693 exchanges — so main.go applies this at its central error-print
// arm rather than any single command wrapping its own errors.
func WithCLIUpgradeHint(err error) error {
	if err == nil || !strings.Contains(err.Error(), cliUpgradeRequiredCode) {
		return err
	}
	return fmt.Errorf("%w\n\nThis version of the Entire CLI is too old for this server. Update it, then rerun the command:\n\n  %s",
		err, versioncheck.UpdateCommandForCurrentBinary(versioninfo.Version))
}
