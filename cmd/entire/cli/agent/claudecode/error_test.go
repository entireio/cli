package claudecode

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClaudeError_ErrorIncludesKindAndMessage(t *testing.T) {
	t.Parallel()
	e := &ClaudeError{Kind: ClaudeErrorAuth, Message: "Invalid API key"}
	s := e.Error()
	if !strings.Contains(s, "auth") {
		t.Errorf("Error() = %q; want to contain kind 'auth'", s)
	}
	if !strings.Contains(s, "Invalid API key") {
		t.Errorf("Error() = %q; want to contain message", s)
	}
}

func TestClaudeError_UnwrapReturnsCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying")
	e := &ClaudeError{Kind: ClaudeErrorUnknown, Cause: cause}
	if got := errors.Unwrap(e); !errors.Is(got, cause) {
		t.Errorf("Unwrap() = %v; want %v", got, cause)
	}
}

func TestClaudeError_UnwrapNilCause(t *testing.T) {
	t.Parallel()
	e := &ClaudeError{Kind: ClaudeErrorAuth}
	if got := errors.Unwrap(e); got != nil {
		t.Errorf("Unwrap() = %v; want nil", got)
	}
}

func TestClaudeError_ErrorEmptyMessageFallback(t *testing.T) {
	t.Parallel()
	e := &ClaudeError{Kind: ClaudeErrorRateLimit}
	s := e.Error()
	want := "claude CLI error (kind=rate_limit)"
	if s != want {
		t.Errorf("Error() = %q; want %q", s, want)
	}
}

func TestClaudeError_ErrorEmptyMessageIncludesExitCode(t *testing.T) {
	t.Parallel()
	e := &ClaudeError{Kind: ClaudeErrorUnknown, ExitCode: 137}
	s := e.Error()
	want := "claude CLI error (kind=unknown, exit=137)"
	if s != want {
		t.Errorf("Error() = %q; want %q", s, want)
	}
}

func TestClaudeError_ErrorsAsIntegration(t *testing.T) {
	t.Parallel()
	cause := errors.New("timeout")
	wrapped := fmt.Errorf("operation failed: %w", &ClaudeError{
		Kind:    ClaudeErrorCLIMissing,
		Message: "claude not found",
		Cause:   cause,
	})

	var ce *ClaudeError
	if !errors.As(wrapped, &ce) {
		t.Fatal("errors.As did not find *ClaudeError in wrapped chain")
	}
	if ce.Kind != ClaudeErrorCLIMissing {
		t.Errorf("Kind = %q; want %q", ce.Kind, ClaudeErrorCLIMissing)
	}
	if !errors.Is(ce, cause) {
		t.Error("errors.Is did not find cause through ClaudeError.Unwrap()")
	}
}

func TestClassifyEnvelopeError(t *testing.T) {
	t.Parallel()
	intPtr := func(n int) *int { return &n }
	tests := []struct {
		name     string
		result   string
		status   *int
		exitCode int
		wantKind ClaudeErrorKind
		wantAPI  int
		wantExit int
	}{
		{"Auth401", "Authentication required", intPtr(401), 0, ClaudeErrorAuth, 401, 0},
		{"Auth403", "forbidden", intPtr(403), 0, ClaudeErrorAuth, 403, 0},
		{"RateLimit429", "Too many requests", intPtr(429), 0, ClaudeErrorRateLimit, 429, 0},
		{"Config404", "model not found", intPtr(404), 0, ClaudeErrorConfig, 404, 0},
		{"Config400", "invalid_request_error", intPtr(400), 0, ClaudeErrorConfig, 400, 0},
		{"UnknownNoStatus", "something blew up", nil, 0, ClaudeErrorUnknown, 0, 0},
		{"Unknown5xx", "upstream error", intPtr(503), 0, ClaudeErrorUnknown, 503, 0},
		{"ExitCodePropagated", "internal error", intPtr(500), 2, ClaudeErrorUnknown, 500, 2},
		{"AuthFromResultWhenStatusNil", "Invalid API key provided", nil, 0, ClaudeErrorAuth, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyEnvelopeError(tc.result, tc.status, tc.exitCode)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v; want %v", got.Kind, tc.wantKind)
			}
			if got.Message != tc.result {
				t.Errorf("Message = %q; want %q", got.Message, tc.result)
			}
			if got.APIStatus != tc.wantAPI {
				t.Errorf("APIStatus = %d; want %d", got.APIStatus, tc.wantAPI)
			}
			if got.ExitCode != tc.wantExit {
				t.Errorf("ExitCode = %d; want %d", got.ExitCode, tc.wantExit)
			}
		})
	}
}

func TestClassifyStderrError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stderr    string
		exitCode  int
		wantKind  ClaudeErrorKind
		wantExit  int
		maxMsgLen int // 0 means no length check
	}{
		{"AuthFromInvalidKey", "error: Invalid API key", 1, ClaudeErrorAuth, 1, 0},
		{"AuthFromNotLoggedIn", "Please run claude login first; you are not logged in", 1, ClaudeErrorAuth, 1, 0},
		{"AuthCaseInsensitive", "INVALID API KEY", 1, ClaudeErrorAuth, 1, 0},
		{"UnknownPreservesMessage", "segfault", 134, ClaudeErrorUnknown, 134, 0},
		{"TruncatesLongStderr", strings.Repeat("x", 800), 1, ClaudeErrorUnknown, 1, 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyStderrError(tc.stderr, tc.exitCode)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v; want %v", got.Kind, tc.wantKind)
			}
			if got.ExitCode != tc.wantExit {
				t.Errorf("ExitCode = %d; want %d", got.ExitCode, tc.wantExit)
			}
			if tc.maxMsgLen > 0 && len(got.Message) > tc.maxMsgLen {
				t.Errorf("len(Message) = %d; want <= %d", len(got.Message), tc.maxMsgLen)
			}
		})
	}
}
