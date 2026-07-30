package claudecode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestClassifyClaudeEnvelope_NonErrorReturnsNil(t *testing.T) {
	t.Parallel()
	stdout := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`)
	if env := classifyClaudeEnvelope(stdout, nil); env != nil {
		t.Errorf("want nil on success envelope; got %#v", env)
	}
}

func TestClassifyClaudeEnvelope_EmptyStdoutReturnsNil(t *testing.T) {
	t.Parallel()
	if env := classifyClaudeEnvelope(nil, nil); env != nil {
		t.Errorf("want nil on empty stdout (caller handles via CLIMissing/stderr path); got %#v", env)
	}
}

func TestClassifyClaudeEnvelope_IsErrorWithHTTP401MapsToAuth(t *testing.T) {
	t.Parallel()
	stdout := []byte(`{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Auth required"}`)
	env := classifyClaudeEnvelope(stdout, nil)
	if env == nil {
		t.Fatal("want typed error")
	}
	if env.Kind != agent.TextGenErrorAuth {
		t.Errorf("Kind = %q; want auth", env.Kind)
	}
	if env.Message != "Auth required" {
		t.Errorf("Message = %q; want envelope result text", env.Message)
	}
	if env.APIStatus != 401 {
		t.Errorf("APIStatus = %d; want 401", env.APIStatus)
	}
	if env.Provider != agent.AgentNameClaudeCode {
		t.Errorf("Provider = %q; want claude-code", env.Provider)
	}
}

func TestClassifyClaudeEnvelope_AuthFromResultWhenStatusMissing(t *testing.T) {
	t.Parallel()
	stdout := []byte(`{"type":"result","is_error":true,"result":"Invalid API key provided"}`)
	env := classifyClaudeEnvelope(stdout, nil)
	if env == nil {
		t.Fatal("want typed error")
	}
	if env.Kind != agent.TextGenErrorAuth {
		t.Errorf("Kind = %q; want auth via phrase heuristic", env.Kind)
	}
}

func TestClassifyClaudeEnvelope_MalformedJSONDefersToCtxSentinel(t *testing.T) {
	t.Parallel()
	// Partial non-JSON stdout AND a ctx sentinel — the envelope parser must
	// return nil so the caller can surface context.Canceled / DeadlineExceeded
	// unwrapped. Regression guard for Cursor Bugbot finding on PR #1005.
	partial := []byte(`{"type":"resul`)
	if env := classifyClaudeEnvelope(partial, context.Canceled); env != nil {
		t.Errorf("want nil on parse failure + ctx.Canceled; got %#v", env)
	}
	if env := classifyClaudeEnvelope(partial, context.DeadlineExceeded); env != nil {
		t.Errorf("want nil on parse failure + ctx.DeadlineExceeded; got %#v", env)
	}
}

func TestClassifyClaudeEnvelope_MalformedJSONPreserves963Wording(t *testing.T) {
	t.Parallel()
	stdout := []byte(`garbage not json`)
	env := classifyClaudeEnvelope(stdout, nil)
	if env == nil {
		t.Fatal("want typed error on malformed JSON — caller at exit 0 needs a non-nil error")
	}
	if env.Kind != agent.TextGenErrorUnknown {
		t.Errorf("Kind = %q; want unknown for malformed JSON", env.Kind)
	}
	if !strings.HasPrefix(env.Message, "failed to parse claude CLI response") {
		t.Errorf("Message = %q; want to start with 963's parse-failure wording", env.Message)
	}
}

func TestClassifyClaudeEnvelope_HTTPStatusMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   int
		wantKind agent.TextGenErrorKind
	}{
		{"Auth401", 401, agent.TextGenErrorAuth},
		{"RateLimit429", 429, agent.TextGenErrorRateLimit},
		{"Config400", 400, agent.TextGenErrorConfig},
		{"Unknown5xx", 503, agent.TextGenErrorUnknown}, // 5xx is not Config per 963
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout := []byte(
				`{"type":"result","is_error":true,"api_error_status":` + strconv.Itoa(tc.status) + `,"result":"err"}`,
			)
			env := classifyClaudeEnvelope(stdout, nil)
			if env == nil {
				t.Fatal("want typed error")
			}
			if env.Kind != tc.wantKind {
				t.Errorf("status=%d Kind = %q; want %q", tc.status, env.Kind, tc.wantKind)
			}
		})
	}
}

// TestEnvelopeAndStderrClassifiersAgree pins that Claude's two reporting
// channels produce the same Kind for the same status.
//
// Claude surfaces an identical failure either as a structured
// api_error_status in its JSON envelope or as a status in stderr text. Those
// went through separate switches and drifted twice: 413/422 (Config via
// envelope, Unknown via stderr) and then 402 (RateLimit via stderr, Config via
// envelope, trail #193 finding 019fb568-645). Each time the user got different
// remediation for the same problem depending on which channel Claude used.
//
// Both now delegate to agent.KindForHTTPStatus. This test is the guard that a
// future edit re-introduces a switch in only one of them.
func TestEnvelopeAndStderrClassifiersAgree(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 401, 402, 403, 404, 408, 409, 413, 422, 429, 451} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			s := status
			viaEnvelope := classifyEnvelopeFields("failure", &s).Kind
			viaStderr := agent.ClassifyStderrHTTPStatus(fmt.Sprintf("HTTP %d: request failed", status))
			if viaEnvelope != viaStderr {
				t.Errorf("status %d: envelope says %q but stderr says %q — the same failure would get different remediation depending on how Claude reported it",
					status, viaEnvelope, viaStderr)
			}
		})
	}
}
