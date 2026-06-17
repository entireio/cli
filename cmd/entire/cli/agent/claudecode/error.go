package claudecode

import (
	"fmt"
	"strings"
)

// ClaudeErrorKind classifies a typed Claude CLI error so callers can
// produce actionable user-facing messages without parsing strings.
type ClaudeErrorKind string

const (
	// ClaudeErrorAuth indicates an authentication or authorization failure
	// (HTTP 401/403 in the CLI envelope, or recognized stderr substring).
	ClaudeErrorAuth ClaudeErrorKind = "auth"
	// ClaudeErrorRateLimit indicates the request was rejected for rate-limit
	// or quota reasons (HTTP 429).
	ClaudeErrorRateLimit ClaudeErrorKind = "rate_limit"
	// ClaudeErrorConfig indicates a client-side request error other than
	// auth or rate-limit (e.g., HTTP 4xx for invalid model or malformed args).
	ClaudeErrorConfig ClaudeErrorKind = "config"
	// ClaudeErrorCLIMissing indicates the claude binary was not found on PATH.
	ClaudeErrorCLIMissing ClaudeErrorKind = "cli_missing"
	// ClaudeErrorUnknown is the catch-all for failures we cannot classify.
	ClaudeErrorUnknown ClaudeErrorKind = "unknown"
)

// ClaudeError is a typed error returned by ClaudeCodeAgent's text generation
// methods. APIStatus and ExitCode use zero to mean "not applicable."
type ClaudeError struct {
	Kind      ClaudeErrorKind
	Message   string // user-safe text extracted from the CLI envelope or stderr
	APIStatus int
	ExitCode  int
	Cause     error
}

func (e *ClaudeError) Error() string {
	if e.Message == "" {
		if e.ExitCode != 0 {
			return fmt.Sprintf("claude CLI error (kind=%s, exit=%d)", e.Kind, e.ExitCode)
		}
		return fmt.Sprintf("claude CLI error (kind=%s)", e.Kind)
	}
	return fmt.Sprintf("claude CLI error (kind=%s): %s", e.Kind, e.Message)
}

func (e *ClaudeError) Unwrap() error { return e.Cause }

const stderrMessageMaxLen = 500

// authStderrPhrases is intentionally small. The primary auth-detection path
// is the structured envelope (classifyEnvelopeError); these phrases are a
// best-effort fallback for crashes that exit non-zero before the envelope
// is produced.
var authStderrPhrases = []string{
	"invalid api key",
	"not logged in",
}

// classifyEnvelopeError converts a Claude CLI is_error:true envelope into a
// typed ClaudeError. The result text is treated as user-safe (the CLI
// produces it for human consumption).
func classifyEnvelopeError(resultText string, apiStatus *int, exitCode int) *ClaudeError {
	e := &ClaudeError{
		Message:  resultText,
		ExitCode: exitCode,
	}
	if apiStatus != nil {
		e.APIStatus = *apiStatus
	}
	switch {
	case e.APIStatus == 401, e.APIStatus == 403:
		e.Kind = ClaudeErrorAuth
	case e.APIStatus == 429:
		e.Kind = ClaudeErrorRateLimit
	case e.APIStatus >= 400 && e.APIStatus < 500:
		e.Kind = ClaudeErrorConfig
	case e.APIStatus == 0 && hasAuthPhrase(resultText):
		// No structured status (older CLI builds / internal errors) — fall
		// back to the same phrase heuristic the stderr path uses so users
		// still get auth-specific guidance.
		e.Kind = ClaudeErrorAuth
	default:
		e.Kind = ClaudeErrorUnknown
	}
	return e
}

// classifyStderrError is a fallback classifier used when the subprocess exited
// non-zero without producing a parseable envelope. It only attempts to
// recognize a small, stable set of auth phrases; everything else becomes
// ClaudeErrorUnknown with the (truncated) stderr as the message.
func classifyStderrError(stderr string, exitCode int) *ClaudeError {
	msg := strings.TrimSpace(stderr)
	if len(msg) > stderrMessageMaxLen {
		msg = msg[:stderrMessageMaxLen]
	}
	e := &ClaudeError{Message: msg, ExitCode: exitCode}
	if hasAuthPhrase(msg) {
		e.Kind = ClaudeErrorAuth
		return e
	}
	e.Kind = ClaudeErrorUnknown
	return e
}

func hasAuthPhrase(s string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range authStderrPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
