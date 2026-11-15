package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// RenderUserFacingError cleans an error for display: it drops the
// `security "BearerAuth": security source "BearerAuth":` segments ogen's
// generated control-plane client wraps every auth failure in, which name the
// OpenAPI security scheme that failed to resolve and so say nothing a user can
// act on (see coreapi.StripSecurityWrapping).
//
// It also augments headless/container keyring failures (e.g. missing DBus
// Secret Service) with actionable guidance pointing the user to the file
// token store (#3058).
//
// It exists as a named function because there is more than one place the CLI
// turns an error into text, and they can't share one: main.go prints the error
// a command returns, renderCoreError feeds the mirror-add wizard (which
// prints its own errors and returns a SilentError, so main.go never sees them),
// and the search TUI captures err.Error() into its model before the command
// returns nil. Call this at any new site that renders an error for a user
// rather than adding another strip.
func RenderUserFacingError(err error) error {
	if err == nil {
		return nil
	}
	err = coreapi.StripSecurityWrapping(err)
	return withKeyringUnavailableHint(err)
}

// isKeyringUnavailableError reports whether err stems from an absent or
// unreachable OS keyring (such as on headless Linux without DBus/SecretService,
// minimal containers, or sandbox restrictions).
func isKeyringUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, tokenstore.ErrKeyringUnavailable) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "org.freedesktop.secrets") ||
		strings.Contains(msg, "Cannot autolaunch D-Bus") ||
		strings.Contains(msg, "no such secret service") ||
		strings.Contains(msg, "the macOS Keychain is not accessible")
}

func withKeyringUnavailableHint(err error) error {
	if err == nil || !isKeyringUnavailableError(err) || errors.Is(err, tokenstore.ErrBadConfig) || tokenstore.FileBackendSelected() {
		return err
	}
	if strings.Contains(err.Error(), "ENTIRE_TOKEN_STORE=file") {
		return err
	}

	return fmt.Errorf("%w\n\nIf this machine has no usable OS keyring (headless server, container, CI), store tokens in a file instead:\n\n  export %s=file\n\nor, to make it stick, put {\"token_store\": \"file\"} in %s.\n\nTokens are then written with 0600 permissions to %s (override the location with %s)",
		err, tokenstore.BackendEnvVar, tokenstore.ConfigFilePath(), tokenstore.FileBackendPath(), tokenstore.PathEnvVar)
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
