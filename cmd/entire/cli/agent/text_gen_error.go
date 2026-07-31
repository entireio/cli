package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// TextGenErrorKind classifies a typed text-generation CLI error so callers can
// produce actionable user-facing messages without parsing strings.
type TextGenErrorKind string

const (
	// TextGenErrorAuth indicates an authentication or authorization failure
	// (HTTP 401/403, or provider-specific stderr phrase).
	TextGenErrorAuth TextGenErrorKind = "auth"
	// TextGenErrorRateLimit indicates the request was rejected for rate-limit
	// or quota reasons (HTTP 429).
	TextGenErrorRateLimit TextGenErrorKind = "rate_limit"
	// TextGenErrorConfig indicates a client-side request error other than
	// auth or rate-limit (e.g., HTTP 4xx for invalid model or malformed args).
	TextGenErrorConfig TextGenErrorKind = "config"
	// TextGenErrorCLIMissing indicates the provider's binary was not found on PATH.
	TextGenErrorCLIMissing TextGenErrorKind = "cli_missing"
	// TextGenErrorUnknown is the catch-all for failures we cannot classify.
	TextGenErrorUnknown TextGenErrorKind = "unknown"
)

// TextGenError is the shared typed error every summary provider's GenerateText
// returns on failure, except ctx cancellation/deadline which pass through as
// sentinels.
//
// APIStatus is 0 when no HTTP status was seen. ExitCode is 0 both for a genuine
// exit 0 (Claude's primary failure mode is exit 0 with is_error:true) and for
// "not captured"; no consumer distinguishes them.
//
// Message is rendered verbatim to the user and comes from third-party stderr,
// so it must stay trimmed, capped and valid UTF-8 — see TruncateStderr.
type TextGenError struct {
	Kind      TextGenErrorKind
	Provider  types.AgentName
	Message   string
	APIStatus int
	ExitCode  int
	Cause     error
}

// Error is what `entire dispatch`, `entire review` and runner setup print —
// they cannot reach renderTextGenError (unexported, package cli).
//
// Cause fills in when Message is empty (the CLIMissing shape sets no Message),
// so the user gets `exec: "codex": executable file not found in $PATH` rather
// than bare "kind=cli_missing". For Cursor that is the only place the real
// binary name (`agent`) appears.
func (e *TextGenError) Error() string {
	detail := e.Message
	if detail == "" && e.Cause != nil {
		detail = e.Cause.Error()
	}
	if detail == "" {
		if e.ExitCode != 0 {
			return fmt.Sprintf("%s CLI error (kind=%s, exit=%d)", e.Provider, e.Kind, e.ExitCode)
		}
		return fmt.Sprintf("%s CLI error (kind=%s)", e.Provider, e.Kind)
	}
	return fmt.Sprintf("%s CLI error (kind=%s): %s", e.Provider, e.Kind, detail)
}

func (e *TextGenError) Unwrap() error { return e.Cause }

// ExecResult is what RunIsolatedTextGeneratorCLIRaw returns: the raw pieces
// a caller needs to classify a subprocess outcome.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// stderrMessageMaxLen caps the Message field size when derived from stderr.
const stderrMessageMaxLen = 500

// TruncateStderr trims whitespace and caps stderr for use as a TextGenError
// Message. Shared across providers so the user-facing Message is predictable.
//
// UTF-8 safe: a naive byte-slice at stderrMessageMaxLen can land in the middle
// of a multi-byte rune, producing invalid UTF-8 in the rendered error message.
// strings.ToValidUTF8 replaces any broken trailing sequence with "".
func TruncateStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= stderrMessageMaxLen {
		return s
	}
	truncated := s[:stderrMessageMaxLen]
	if !utf8.ValidString(truncated) {
		truncated = strings.ToValidUTF8(truncated, "")
	}
	return truncated
}

// IsExecNotFoundErr reports whether err indicates the CLI binary was not found
// on PATH. Intentionally excludes other *exec.Error causes (permission denied,
// invalid executable format), which should surface as a generic failure so
// operators aren't misdirected to a reinstall when the real problem is a
// broken/inaccessible binary.
func IsExecNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return true
	}
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// A 4xx status must appear in an HTTP-status context, matched by one of three
// alternatives: (a) keyword-prefixed, (b) followed by its reason phrase, or
// (c) parenthesized after a reason phrase. The status is in whichever group
// matched.
//
// Lexical context is required because word boundaries alone are not enough:
// ':' and '.' are non-word characters, so `\b4\d{2}\b` also matches stack
// frames ("foo.js:401:12"), decimals ("2.403 seconds") and bare counts.
//
// (a) and (c) are machine-shaped; (b) is how prose names a status. They stay
// separable because stdout is classified with (a)+(c) only — see
// ClassifyDiagnosticHTTPStatus.
const (
	// `status[_\s-]*code` matches the JSON form `{"status_code": 401}`: '_' is
	// a word character, so a bare `\bstatus\b` fails on "status_code".
	httpStatusKeywordAlt = `\b(?:https?(?:/[\d.]+)?|status(?:[_\s-]*code)?|code|err(?:or)?)\b\W{0,3}(4\d{2})\b`
	httpStatusReasonAlt  = `\b(4\d{2})\s+(?:unauthorized|forbidden|too\s+many\s+requests|bad\s+request` +
		`|not\s+found|payment\s+required|request\s+timeout|conflict|gone` +
		`|payload\s+too\s+large|unprocessable|rate\s*limit)`
	// (c) "Unauthorized (401)", "rate limit exceeded (429)".
	httpStatusParenAlt = `\b(?:unauthorized|forbidden|too\s+many\s+requests|bad\s+request` +
		`|not\s+found|payment\s+required|rate\s*limit(?:\s+exceeded)?|quota\s+exceeded)\b\W{0,4}\((4\d{2})\)`
)

var (
	http4xxPattern = regexp.MustCompile(`(?i)` + httpStatusKeywordAlt + `|` + httpStatusReasonAlt + `|` + httpStatusParenAlt)
	// Machine-shaped alternatives only, for text that may be model output.
	httpStatusKeywordPattern = regexp.MustCompile(`(?i)` + httpStatusKeywordAlt + `|` + httpStatusParenAlt)
)

// KindForHTTPStatus is the single authoritative status -> Kind mapping.
//
// Shared by the stderr/stdout classifier and Claude's envelope classifier.
// Claude reports the same failure through either channel, and separate
// switches drifted twice (413/422, then 402), giving different remediation for
// an identical failure. Pinned by TestEnvelopeAndStderrClassifiersAgree.
func KindForHTTPStatus(status int) TextGenErrorKind {
	switch {
	case status == 401, status == 403:
		return TextGenErrorAuth
	// 402 is quota exhaustion: same user action as 429, not a misconfiguration.
	case status == 429, status == 402:
		return TextGenErrorRateLimit
	case status >= 400 && status < 500:
		return TextGenErrorConfig
	default:
		return TextGenErrorUnknown
	}
}

// classifyWith returns the most specific Kind found anywhere in s.
//
// Precedence is Auth > RateLimit > Config, not document order: when a retrying
// CLI logs several statuses, the one with a specific remediation should win
// regardless of position. Config is the catch-all for other 4xx.
func classifyWith(re *regexp.Regexp, s string) TextGenErrorKind {
	best := TextGenErrorUnknown
	rank := map[TextGenErrorKind]int{
		TextGenErrorUnknown:   0,
		TextGenErrorConfig:    1,
		TextGenErrorRateLimit: 2,
		TextGenErrorAuth:      3,
	}
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		// At most one capture group is populated per match.
		status := ""
		for _, g := range m[1:] {
			if g != "" {
				status = g
				break
			}
		}
		code, convErr := strconv.Atoi(status)
		if convErr != nil {
			continue
		}
		kind := KindForHTTPStatus(code)
		if kind == TextGenErrorUnknown {
			continue
		}
		if rank[kind] > rank[best] {
			best = kind
			if best == TextGenErrorAuth {
				return best // nothing outranks auth
			}
		}
	}
	return best
}

// ClassifyDiagnosticHTTPStatus classifies text that is only probably
// diagnostic — a stdout-primary CLI's output on a non-zero exit with empty
// stderr.
//
// Accepts only the machine-shaped forms ("HTTP 401", `{"status_code": 401}`),
// never the prose form ("401 Unauthorized"), because stdout on a summary call
// is usually the model's summary of the user's transcript.
func ClassifyDiagnosticHTTPStatus(stdout string) TextGenErrorKind {
	return classifyWith(httpStatusKeywordPattern, stdout)
}

// ClassifyStderrHTTPStatus scans stderr for an HTTP status code and returns
// the matching error Kind. Most CLIs pass through their upstream API's HTTP
// status on failure, so this is the load-bearing classification signal.
// Returns TextGenErrorUnknown when no recognized status is present.
//
// Pass the FULL stderr, not a truncated copy: truncation is a display concern
// and applying it first would hide a status line that appears after the cap
// (CLIs commonly emit a banner or stack preamble before the real error).
//
// When several statuses appear, the first RECOGNIZED one wins — not merely the
// leftmost match. A leading status this function does not map (e.g. 413) must
// not consume the scan and mask a later 401.
func ClassifyStderrHTTPStatus(stderr string) TextGenErrorKind {
	return classifyWith(http4xxPattern, stderr)
}

// HandleTextGenResult converts a RunIsolatedTextGeneratorCLIRaw outcome into
// (trimmed stdout, err). On success returns (output, nil).
//
// On failure it returns a *TextGenerationError wrapping either a ctx sentinel
// or a *TextGenError. Both must survive: *TextGenError drives the user-facing
// label, *TextGenerationError carries the evidence the timeout diagnostic
// needs and is the only signal on the ctx path. Wrapping keeps all three
// lookups working (errors.As for either type, errors.Is for the sentinel);
// returning either one bare silently degrades the other.
//
// On a failed run, Message is the first non-empty of stderr, stdout, and
// runErr.Error() — so it is never empty — and both classification and
// extraClassify see that full text before it is truncated for display.
//
// extraClassify is an optional per-agent hook invoked only when the shared
// HTTP-status baseline returned Unknown — used by agents whose stderr carries
// auth/rate-limit signals without an HTTP status (e.g. gemini). Pass nil to
// skip.
//
// emptyMsg populates TextGenError.Message when the subprocess exits 0 with no
// stdout (whitespace-only stdout counts as empty).
//
// Claude does not use this helper — its envelope-first classification order
// differs and is inlined in claudecode.GenerateText.
func HandleTextGenResult(res ExecResult, runErr error, provider types.AgentName, emptyMsg string, extraClassify func(stderr string) TextGenErrorKind) (string, error) {
	// Attached to every failure return; the timeout diagnostic reads it.
	withEvidence := func(err error) error {
		return &TextGenerationError{
			Err:         err,
			Stderr:      TruncateStderr(string(res.Stderr)),
			StdoutBytes: len(res.Stdout),
		}
	}
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			return "", withEvidence(context.Canceled)
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			return "", withEvidence(context.DeadlineExceeded)
		}
		if IsExecNotFoundErr(runErr) {
			return "", withEvidence(&TextGenError{Kind: TextGenErrorCLIMissing, Provider: provider, Cause: runErr})
		}
		// Message: stderr, else stdout, else the run error — never empty.
		// codex/cursor/copilot are stdout-primary and print diagnostics there;
		// a launch failure (permission denied, exec format error) produces no
		// output at all and only runErr describes it.
		stderr := strings.TrimSpace(string(res.Stderr))
		raw := stderr
		if raw == "" {
			raw = strings.TrimSpace(string(res.Stdout))
		}
		if raw == "" {
			raw = runErr.Error()
		}
		// Classify the FULL stderr: truncating first would drop a status line
		// sitting past the 500-byte display cap, which is where it lands when a
		// CLI prints a banner or stack preamble ahead of the real error.
		kind := ClassifyStderrHTTPStatus(stderr)
		if kind == TextGenErrorUnknown && extraClassify != nil {
			if k := extraClassify(stderr); k != TextGenErrorUnknown {
				kind = k
			}
		}
		// Only when stderr said nothing, fall back to stdout with the strict
		// scan. Skipping stdout leaves the stdout-primary providers permanently
		// Unknown; scanning it permissively misreads a summary that merely
		// discusses a 401 as a 401.
		if kind == TextGenErrorUnknown && stderr == "" {
			kind = ClassifyDiagnosticHTTPStatus(strings.TrimSpace(string(res.Stdout)))
		}
		return "", withEvidence(&TextGenError{
			Kind:     kind,
			Provider: provider,
			Message:  TruncateStderr(raw),
			ExitCode: res.ExitCode,
			Cause:    runErr,
		})
	}
	out := strings.TrimSpace(string(res.Stdout))
	if out == "" {
		return "", withEvidence(&TextGenError{
			Kind:     TextGenErrorUnknown,
			Provider: provider,
			Message:  emptyMsg,
		})
	}
	return out, nil
}
