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
// raw bytes for Claude's envelope parser.
func TestHandleTextGenResult_TrimsStdout(t *testing.T) {
	t.Parallel()
	res := ExecResult{Stdout: []byte("  hello world\n\n")}
	out, err := HandleTextGenResult(res, nil, AgentNameCodex, "empty", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello world" {
		t.Errorf("out = %q; want %q", out, "hello world")
	}
}

// TestHandleTextGenResult_WhitespaceOnlyStdoutIsEmpty pins that whitespace-only
// stdout counts as empty rather than being passed downstream as a "summary".
func TestHandleTextGenResult_WhitespaceOnlyStdoutIsEmpty(t *testing.T) {
	t.Parallel()
	res := ExecResult{Stdout: []byte("   \n\t\n")}
	_, err := HandleTextGenResult(res, nil, AgentNameCodex, "codex CLI returned empty output", nil)
	var tge *TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *TextGenError", err)
	}
	if tge.Message != "codex CLI returned empty output" {
		t.Errorf("Message = %q; want the emptyMsg", tge.Message)
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
	tests := []struct {
		name string
		res  ExecResult
		want string
	}{
		{"stderr preferred", ExecResult{Stderr: []byte("from stderr"), Stdout: []byte("from stdout")}, "from stderr"},
		{"falls back to stdout", ExecResult{Stdout: []byte("quota exhausted")}, "quota exhausted"},
		{"falls back to run error", ExecResult{}, "fork/exec: permission denied"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runErr := errors.New("fork/exec: permission denied")
			_, err := HandleTextGenResult(tc.res, runErr, AgentNameCodex, "empty", nil)
			var tge *TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("err = %v; want *TextGenError", err)
			}
			if tge.Message != tc.want {
				t.Errorf("Message = %q; want %q", tge.Message, tc.want)
			}
		})
	}
}

func TestTextGenError_ErrorIncludesKindAndMessage(t *testing.T) {
	t.Parallel()
	e := &TextGenError{Kind: TextGenErrorAuth, Provider: AgentNameClaudeCode, Message: "Invalid API key"}
	s := e.Error()
	if !strings.Contains(s, "auth") {
		t.Errorf("Error() = %q; want to contain kind 'auth'", s)
	}
	if !strings.Contains(s, "Invalid API key") {
		t.Errorf("Error() = %q; want to contain message", s)
	}
}

// CLIMissing sets no Message, so without the Cause fallback the consumers that
// print Error() directly (dispatch, review, runner setup) show only
// "codex CLI error (kind=cli_missing)" — jargon naming no binary.
func TestTextGenError_ErrorFallsBackToCause(t *testing.T) {
	t.Parallel()
	cause := &exec.Error{Name: "codex", Err: exec.ErrNotFound}
	e := &TextGenError{Kind: TextGenErrorCLIMissing, Provider: AgentNameCodex, Cause: cause}
	got := e.Error()
	if !strings.Contains(got, "executable file not found") {
		t.Errorf("Error() = %q; want the cause's actionable text", got)
	}
	if !strings.Contains(got, "codex") {
		t.Errorf("Error() = %q; want the binary name to appear", got)
	}
	// Message still wins when present — the cause must not be appended twice.
	withMsg := &TextGenError{Kind: TextGenErrorAuth, Provider: AgentNameCodex, Message: "401 Unauthorized", Cause: cause}
	if strings.Contains(withMsg.Error(), "executable file not found") {
		t.Errorf("Error() = %q; Message must take precedence over Cause", withMsg.Error())
	}
}

func TestTextGenError_UnwrapReturnsCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying")
	e := &TextGenError{Kind: TextGenErrorUnknown, Cause: cause}
	if got := errors.Unwrap(e); !errors.Is(got, cause) {
		t.Errorf("Unwrap() = %v; want %v", got, cause)
	}
}

func TestTextGenError_ErrorEmptyMessageIncludesExitCode(t *testing.T) {
	t.Parallel()
	e := &TextGenError{Kind: TextGenErrorUnknown, Provider: AgentNameClaudeCode, ExitCode: 137}
	want := "claude-code CLI error (kind=unknown, exit=137)"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q; want %q", got, want)
	}
}

func TestTextGenError_ErrorsAsIntegration(t *testing.T) {
	t.Parallel()
	cause := errors.New("timeout")
	wrapped := fmt.Errorf("operation failed: %w", &TextGenError{
		Kind:     TextGenErrorCLIMissing,
		Provider: AgentNameCodex,
		Message:  "codex not found",
		Cause:    cause,
	})

	var tge *TextGenError
	if !errors.As(wrapped, &tge) {
		t.Fatal("errors.As did not find *TextGenError in wrapped chain")
	}
	if tge.Kind != TextGenErrorCLIMissing {
		t.Errorf("Kind = %q; want %q", tge.Kind, TextGenErrorCLIMissing)
	}
	if !errors.Is(tge, cause) {
		t.Error("errors.Is did not find cause through TextGenError.Unwrap()")
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
		{"json camelCase", `{"statusCode":403}`, TextGenErrorAuth},
		{"error keyword", "error: 403 returned by upstream", TextGenErrorAuth},
		{"status colon", "status: 404", TextGenErrorConfig},

		// Prose about a status: do NOT classify.
		{"prose names a 401", "The user was debugging a 401 Unauthorized from the payments API.", TextGenErrorUnknown},
		{"prose names a 429", "Fixed the 429 Too Many Requests retry loop in client.go", TextGenErrorUnknown},
		{"prose names a 404", "Summary: resolved a 404 Not Found on /v1/users", TextGenErrorUnknown},
		{"bare reason phrase", "401 Unauthorized", TextGenErrorUnknown},
		{"empty", "", TextGenErrorUnknown},
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
