package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"unicode/utf8"
)

// The helper trims, not the runner: RunIsolatedTextGeneratorCLIRaw must return
// raw bytes for Claude's envelope parser. Whitespace-only counts as empty
// rather than being passed downstream as a "summary".
func TestHandleTextGenResult_TrimsStdout(t *testing.T) {
	t.Parallel()
	out, err := HandleTextGenResult(ExecResult{Stdout: []byte("  hello world\n\n")}, nil, AgentNameCodex, "empty", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello world" {
		t.Errorf("out = %q; want %q", out, "hello world")
	}

	_, err = HandleTextGenResult(ExecResult{Stdout: []byte("   \n\t\n")}, nil, AgentNameCodex, "codex CLI returned empty output", nil)
	var tge *TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("whitespace-only: err = %v; want *TextGenError", err)
	}
	if tge.Message != "codex CLI returned empty output" {
		t.Errorf("whitespace-only: Message = %q; want the emptyMsg", tge.Message)
	}
}

// Classification runs on the whole stderr, not the truncated Message: a banner
// or stack preamble puts the real status line past the 500-byte cap.
func TestHandleTextGenResult_ClassifiesFullStderr(t *testing.T) {
	t.Parallel()
	noise := strings.Repeat("at someFrame (/a/b/c.js)\n", 30) // ~750 bytes
	res := ExecResult{
		Stderr:   []byte(noise + "ERROR: 401 Unauthorized"),
		ExitCode: 1,
	}
	_, err := HandleTextGenResult(res, errors.New("exit status 1"), AgentNameCodex, "empty", nil)
	var tge *TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *TextGenError", err)
	}
	if tge.Kind != TextGenErrorAuth {
		t.Errorf("Kind = %q; want auth (status sits past the Message cap)", tge.Kind)
	}
	if len(tge.Message) > stderrMessageMaxLen {
		t.Errorf("Message len = %d; want <= %d (display is still capped)", len(tge.Message), stderrMessageMaxLen)
	}
}

// Three-tier fallback: stderr, stdout, run error. Without it a stdout-primary
// CLI or a launch failure renders "(no diagnostic detail available)" while the
// real text sits unused in the ExecResult.
func TestHandleTextGenResult_MessageNeverEmptyOnFailure(t *testing.T) {
	t.Parallel()
	// stderr-preferred and the stdout tier are covered by TestGenerateText_Matrix
	// (AuthFrom401, DiagnosticOnStdout). The runErr tier is only reachable when
	// the subprocess produced nothing at all — a launch failure.
	runErr := errors.New("fork/exec: permission denied")
	_, err := HandleTextGenResult(ExecResult{}, runErr, AgentNameCodex, "empty", nil)
	var tge *TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *TextGenError", err)
	}
	if tge.Message != "fork/exec: permission denied" {
		t.Errorf("Message = %q; want the run error", tge.Message)
	}
}

// One table for Error()/Unwrap. CLIMissing sets no Message, so without the
// Cause fallback the consumers that print Error() directly (dispatch, review,
// runner setup) show only "kind=cli_missing" — jargon naming no binary.
func TestTextGenError_ErrorAndUnwrap(t *testing.T) {
	t.Parallel()
	cause := &exec.Error{Name: "codex", Err: exec.ErrNotFound}
	tests := []struct {
		name string
		err  *TextGenError
		want string
	}{
		{"message wins",
			&TextGenError{Kind: TextGenErrorAuth, Provider: AgentNameCodex, Message: "Invalid API key", Cause: cause},
			"codex CLI error (kind=auth): Invalid API key"},
		{"falls back to cause when no message",
			&TextGenError{Kind: TextGenErrorCLIMissing, Provider: AgentNameCodex, Cause: cause},
			`codex CLI error (kind=cli_missing): exec: "codex": executable file not found in $PATH`},
		{"exit code when neither",
			&TextGenError{Kind: TextGenErrorUnknown, Provider: AgentNameClaudeCode, ExitCode: 137},
			"claude-code CLI error (kind=unknown, exit=137)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q; want %q", got, tc.want)
			}
		})
	}

	// Unwrap must expose Cause so errors.Is/As reach it through a wrap.
	wrapped := fmt.Errorf("operation failed: %w",
		&TextGenError{Kind: TextGenErrorCLIMissing, Provider: AgentNameCodex, Cause: cause})
	var tge *TextGenError
	if !errors.As(wrapped, &tge) {
		t.Fatal("errors.As did not find *TextGenError through the wrap")
	}
	if !errors.Is(tge, exec.ErrNotFound) {
		t.Error("errors.Is did not reach Cause through Unwrap")
	}
}

func TestClassifyStderrHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   TextGenErrorKind
	}{
		{"401 maps to auth", "ERROR: 401 Unauthorized", TextGenErrorAuth},
		{"403 maps to auth", "ERROR: 403 Forbidden", TextGenErrorAuth},
		{"429 maps to rate_limit", "ERROR: 429 Too Many Requests", TextGenErrorRateLimit},
		{"400 maps to config", "ERROR: 400 Bad Request", TextGenErrorConfig},
		{"404 maps to config", "ERROR: 404 Not Found", TextGenErrorConfig},
		{"no status maps to unknown", "something weird and unclassifiable", TextGenErrorUnknown},
		{"empty maps to unknown", "", TextGenErrorUnknown},

		// Regression guards for the PR #1005 review finding: bare substring
		// match on short digit sequences produced false positives. Word-
		// boundary matching must reject digits embedded in larger numbers,
		// unit suffixes, or adjacent word characters.
		{"port number containing 401 is NOT auth", "could not bind to port 14010", TextGenErrorUnknown},
		{"millisecond suffix 429ms is NOT rate_limit", "request took 429ms before failing", TextGenErrorUnknown},
		{"byte count containing 400 is NOT config", "wrote 14000 bytes then stalled", TextGenErrorUnknown},
		{"id containing 404 is NOT config", "trace-id=404a9f", TextGenErrorUnknown},
		{"timestamp minute containing 401 is NOT auth", "2026-04-21T14:01:23Z connection reset", TextGenErrorUnknown},
		{"most specific kind wins", "HTTP 401 Unauthorized; retry window 429", TextGenErrorAuth},

		// Word boundaries alone are not sufficient: ':' and '.' are non-word
		// characters, so `\b4\d{2}\b` matched stack frames, decimals and bare
		// counts. Node CLIs emit "file.js:LINE:COL" constantly.
		{"node stack frame line:col is NOT auth", "at Socket.emit (node:events:401:20)", TextGenErrorUnknown},
		{"js bundle frame is NOT rate_limit", "at Object.<anonymous> (/x/dist/index.js:429:15)", TextGenErrorUnknown},
		{"go panic source line is NOT config", "panic: index out of range\n\tmain.go:404 +0x1f", TextGenErrorUnknown},
		{"decimal fraction is NOT auth", "request took 2.403 seconds", TextGenErrorUnknown},
		{"decimal fraction with unit is NOT rate_limit", "completed in 0.429 s", TextGenErrorUnknown},
		{"bare delimited count is NOT config", "v1.2 build 404 finished", TextGenErrorUnknown},
		{"log line number is NOT rate_limit", "parse failed at line 429", TextGenErrorUnknown},

		// Precedence is by specificity, not position: a leading Config-class
		// status must not mask a later auth signal.
		{"leading 413 does not mask a later 401", "HTTP 413 Payload Too Large\nHTTP 401 Unauthorized", TextGenErrorAuth},
		{"leading 402 does not mask a later 401", "status 402 then HTTP 401", TextGenErrorAuth},
		{"413 alone is config, not unknown", "HTTP 413 Payload Too Large", TextGenErrorConfig},
		{"422 alone is config", "status: 422", TextGenErrorConfig},
		{"402 is rate_limit, not config", "HTTP 402 Payment Required", TextGenErrorRateLimit},
		{"parenthesized status classifies", "Unauthorized (401)", TextGenErrorAuth},
		{"parenthesized rate limit", "rate limit exceeded (429)", TextGenErrorRateLimit},

		// Keyword-prefixed and reason-phrase forms must still classify.
		{"status= form", "status=403 returned by upstream", TextGenErrorAuth},
		{"status colon form", "status: 429", TextGenErrorRateLimit},
		{"HTTP/1.1 form", "HTTP/1.1 400 Bad Request", TextGenErrorConfig},
		{"reason phrase without keyword", "429 Too Many Requests", TextGenErrorRateLimit},
		{"json snake_case status_code", `{"status_code": 401, "detail": "bad token"}`, TextGenErrorAuth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyStderrHTTPStatus(tc.stderr); got != tc.want {
				t.Errorf("ClassifyStderrHTTPStatus(%q) = %q; want %q", tc.stderr, got, tc.want)
			}
		})
	}
}

// The strict scan used on stdout, which may be the model's summary rather than
// a diagnostic: accepts the machine-shaped form, rejects the prose form.
// Skipping stdout entirely is the opposite error — it leaves the
// stdout-primary providers permanently Unknown.
func TestClassifyDiagnosticHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want TextGenErrorKind
	}{
		// Machine-shaped: classify.
		{"HTTP keyword", "HTTP 429: quota exhausted", TextGenErrorRateLimit},
		{"json snake_case", `{"status_code": 401}`, TextGenErrorAuth},

		// Prose naming a status: do NOT classify.
		{"prose names a 401", "The user was debugging a 401 Unauthorized from the payments API.", TextGenErrorUnknown},
		{"bare reason phrase", "401 Unauthorized", TextGenErrorUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyDiagnosticHTTPStatus(tc.in); got != tc.want {
				t.Errorf("ClassifyDiagnosticHTTPStatus(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncateStderr(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 800)
	got := TruncateStderr(long)
	if len(got) > stderrMessageMaxLen {
		t.Errorf("len = %d; want <= %d", len(got), stderrMessageMaxLen)
	}
	if got := TruncateStderr("  hello  "); got != "hello" {
		t.Errorf("TruncateStderr trims whitespace = %q; want 'hello'", got)
	}
}

// TestTruncateStderr_UTF8Safe pins the PR #1005 review finding: a naive
// byte-slice at stderrMessageMaxLen could land mid-rune and produce invalid
// UTF-8 in user-facing output. The truncator must return a valid-UTF-8
// string in every case.
func TestTruncateStderr_UTF8Safe(t *testing.T) {
	t.Parallel()
	// Build a string whose byte 499 is a continuation byte of a 3-byte rune
	// (U+4E2D "中" → 0xE4 0xB8 0xAD). Each 中 occupies 3 bytes; pad with
	// single-byte ASCII so the 500-byte cut lands inside a rune.
	padding := strings.Repeat("a", 498)
	s := padding + "中" + "xx" // total = 498 + 3 + 2 = 503 bytes
	got := TruncateStderr(s)
	if !utf8.ValidString(got) {
		t.Fatalf("TruncateStderr returned invalid UTF-8: bytes=%v", []byte(got))
	}
	if len(got) > stderrMessageMaxLen {
		t.Errorf("len = %d; want <= %d", len(got), stderrMessageMaxLen)
	}
}

func TestIsExecNotFoundErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"exec.Error wrapping ErrNotFound", &exec.Error{Name: "codex", Err: exec.ErrNotFound}, true},
		{"top-level exec.ErrNotFound", exec.ErrNotFound, true},
		{"os.ErrNotExist", os.ErrNotExist, true},
		{"wrapped exec.ErrNotFound via fmt.Errorf", fmt.Errorf("spawn failed: %w", exec.ErrNotFound), true},
		{"permission denied is NOT CLI-missing", &exec.Error{Name: "x", Err: os.ErrPermission}, false},
		{"nil is NOT CLI-missing", nil, false},
		{"arbitrary error is NOT CLI-missing", errors.New("some other failure"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsExecNotFoundErr(tc.err); got != tc.want {
				t.Errorf("IsExecNotFoundErr(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}
