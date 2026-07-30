package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
// returns on failure, except context cancellation/deadline, which pass through
// as bare sentinels (wrapped for evidence — see HandleTextGenResult).
//
// APIStatus is 0 when no HTTP status was observed (there is no HTTP 0).
// ExitCode is 0 both when the subprocess genuinely exited 0 — Claude's primary
// failure mode is exit 0 with is_error:true — and when no exit code was
// captured; the two are not distinguished because no consumer needs to.
//
// Message is user-facing: it is rendered verbatim by the explain layer, and is
// populated from third-party CLI stderr, so it must stay whitespace-trimmed,
// length-capped and valid UTF-8 (see TruncateStderr).
type TextGenError struct {
	Kind      TextGenErrorKind
	Provider  types.AgentName
	Message   string
	APIStatus int
	ExitCode  int
	Cause     error
}

// Error renders the error for consumers that print it directly rather than
// going through the explain layer's renderTextGenError — `entire dispatch`,
// `entire review`'s synthesis sink, and runner setup. Those surfaces have no
// access to the (unexported, package cli) renderer, so this string is what
// their users actually see.
//
// Cause is included when Message is empty, which is the CLIMissing shape:
// those constructions deliberately set no Message, so without this the user
// would get only "codex CLI error (kind=cli_missing)" — internal jargon that
// names no binary. Falling back to Cause restores the actionable text
// (`exec: "codex": executable file not found in $PATH`), and for Cursor it is
// the only place the real binary name (`agent`, not `cursor`) appears.
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

// http4xxPattern matches a 4xx HTTP status code that appears in an actual
// HTTP-status context, via one of two alternatives:
//
//	(a) preceded by a status-ish keyword within a few non-word characters —
//	    "HTTP 401", "HTTP/1.1 401", "status: 401", "status=401", "code 401",
//	    "ERROR: 401".
//	(b) followed by its canonical reason phrase — "401 Unauthorized",
//	    "429 Too Many Requests".
//
// Requiring lexical context (rather than just digit isolation) is what makes
// this safe. Word boundaries alone are NOT sufficient, because the digits in
// real CLI stderr are usually adjacent to ':' and '.', which are themselves
// non-word characters — so `\b4\d{2}\b` happily matches a stack frame
// ("at foo.js:401:12"), a decimal ("took 2.403 seconds"), a source line
// ("main.go:404 +0x1f") and a bare count ("build 404 finished"), and reports
// them as authentication or config failures. Gemini, Copilot and Cursor are
// Node CLIs whose crash output is exactly "file.js:LINE:COL" frames, so that
// false-positive class was the common case, not an edge case.
//
// The status is captured in whichever of the two groups matched.
var http4xxPattern = regexp.MustCompile(`(?i)` +
	`\b(?:https?(?:/[\d.]+)?|status(?:\s*code)?|code|err(?:or)?)\b\W{0,3}(4\d{2})\b` +
	`|` +
	`\b(4\d{2})\s+(?:unauthorized|forbidden|too\s+many\s+requests|bad\s+request` +
	`|not\s+found|payment\s+required|request\s+timeout|conflict|gone` +
	`|payload\s+too\s+large|unprocessable|rate\s*limit)`)

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
	for _, m := range http4xxPattern.FindAllStringSubmatch(stderr, -1) {
		// Exactly one of the two capture groups is populated per match.
		status := m[1]
		if status == "" {
			status = m[2]
		}
		switch status {
		case "401", "403":
			return TextGenErrorAuth
		case "429":
			return TextGenErrorRateLimit
		case "400", "404":
			return TextGenErrorConfig
		}
	}
	return TextGenErrorUnknown
}

// HandleTextGenResult converts the outcome of a RunIsolatedTextGeneratorCLIRaw
// call into (trimmed stdout, err). On success returns (output, nil).
//
// On failure it returns a *TextGenerationError whose Err is either a ctx
// sentinel or a *TextGenError. The two error types are complementary, not
// alternatives, and both must survive to the explain layer:
//
//   - *TextGenError answers "what kind of failure was this?" and drives the
//     user-facing label and remediation (renderTextGenError).
//   - *TextGenerationError answers "what did the subprocess emit before it
//     died?" and drives the timeout diagnostic ("provider produced no output"
//     vs "was generating output when killed"). That evidence is the only
//     signal available on the ctx path, where classification is meaningless.
//
// Wrapping preserves both: errors.As finds *TextGenError through the wrapper,
// errors.Is finds the ctx sentinel, and errors.As finds *TextGenerationError
// for the evidence. Returning a bare sentinel here would silently regress the
// timeout diagnostic, and returning a bare *TextGenError would drop the
// stdout-byte count.
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
	// withEvidence attaches the captured subprocess output that the explain
	// layer's timeout diagnostic reads. Applied to every failure return.
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
		// Prefer stderr, fall back to stdout, then to the run error itself, so
		// Message is never empty. codex/cursor/copilot are stdout-primary
		// tools that print diagnostics there and exit non-zero; reading only
		// stderr renders "(no diagnostic detail available from X CLI)" while
		// the real text sits unused in res.Stdout. The run error is the last
		// resort — it is the only thing that describes a launch failure
		// (permission denied, exec format error), which produces no output at
		// all and is not a "binary missing" case.
		stderr := strings.TrimSpace(string(res.Stderr))
		raw := stderr
		if raw == "" {
			raw = strings.TrimSpace(string(res.Stdout))
		}
		if raw == "" {
			raw = runErr.Error()
		}
		// Classify against stderr ONLY, and against the FULL stderr.
		//
		// Only-stderr: stdout for a summary call is the model's prose summary of
		// the user's transcript, not diagnostic output. Classifying it means a
		// summary that happens to discuss a 401 gets reported as "authentication
		// failed" with a confident, wrong "check your CLI authentication" row —
		// strictly worse than Unknown, which at least shows the raw text
		// honestly. Stdout is still good enough to *show* (below); it is not
		// evidence about why the process failed.
		//
		// Full-stderr: truncating first would drop a status line (or a provider
		// phrase) sitting past the 500-byte cap, which is exactly where it lands
		// when the CLI prints a banner or stack preamble ahead of the real error.
		kind := ClassifyStderrHTTPStatus(stderr)
		if kind == TextGenErrorUnknown && extraClassify != nil {
			if k := extraClassify(stderr); k != TextGenErrorUnknown {
				kind = k
			}
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
