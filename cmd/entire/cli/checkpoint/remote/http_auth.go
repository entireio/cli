package remote

import (
	"errors"
	"fmt"
	"strings"
)

// ErrHTTPAuthUnavailable means Git needed HTTPS credentials but could not prompt.
var ErrHTTPAuthUnavailable = errors.New("HTTPS credentials unavailable without terminal prompts")

// ErrHTTPAuthRejected means the server rejected HTTP authentication. It does not
// imply that caching credentials would help, or identify why they were rejected.
var ErrHTTPAuthRejected = errors.New("HTTP authentication rejected")

// withHTTPAuthFailure preserves an actionable, typed cause without embedding
// Git's output: that output can contain credentials in a remote URL. Keep the
// original exec error too. Generic 403/404, exit 128, and repository-not-found
// errors aren't proof of an authentication failure and must not match.
func withHTTPAuthFailure(err error, output string) error {
	lower := strings.ToLower(output)
	var authErr error
	switch {
	case strings.Contains(lower, "terminal prompts disabled") &&
		(strings.Contains(lower, "could not read username for 'http") ||
			strings.Contains(lower, "could not read password for 'http")):
		authErr = ErrHTTPAuthUnavailable
	case strings.Contains(lower, "authentication failed for 'http"),
		strings.Contains(lower, "http basic: access denied"):
		authErr = ErrHTTPAuthRejected
	default:
		return err
	}
	return fmt.Errorf("%w: %w", err, authErr)
}
