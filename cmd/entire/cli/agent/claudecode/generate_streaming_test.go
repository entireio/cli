package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

const helloWorldResult = "Hello, world."

func TestGenerateTextStreaming_Success(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	agentInst := &ClaudeCodeAgent{
		CommandRunner: testutil.FakeStreamCmd(string(fixture), "", 0),
	}

	var events []agent.GenerationProgress
	result, err := agentInst.GenerateTextStreaming(
		context.Background(), "test prompt", "haiku",
		func(p agent.GenerationProgress) {
			events = append(events, p)
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != helloWorldResult {
		t.Errorf("result = %q, want %q", result, helloWorldResult)
	}
	// We expect Connecting, FirstToken, Generating x2 from the stream,
	// plus a final Done emitted by GenerateTextStreaming itself.
	phases := make([]agent.ProgressPhase, 0, len(events))
	for _, e := range events {
		phases = append(phases, e.Phase)
	}
	want := []agent.ProgressPhase{
		agent.PhaseConnecting,
		agent.PhaseFirstToken,
		agent.PhaseGenerating,
		agent.PhaseGenerating,
		agent.PhaseDone,
	}
	if !equalPhases(phases, want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}

	// Field payloads must match the fixture, not just the phase sequence.
	firstToken := events[1]
	if firstToken.TTFTms != 935 {
		t.Errorf("FirstToken.TTFTms = %d, want 935 (top-level ttft_ms in fixture)", firstToken.TTFTms)
	}
	if firstToken.InputTokens != 9 {
		t.Errorf("FirstToken.InputTokens = %d, want 9", firstToken.InputTokens)
	}
	if firstToken.CachedInputTokens != 1234 {
		t.Errorf("FirstToken.CachedInputTokens = %d, want 1234", firstToken.CachedInputTokens)
	}
	// The token estimate accumulates raw chars across deltas ("Hello, " = 7,
	// "world." = 6) and divides the running total — per-delta division would
	// truncate small deltas to 0 and freeze the UI at "~0 tokens".
	if got := events[2].OutputTokens; got != 7/4 {
		t.Errorf("Generating[0].OutputTokens = %d, want %d", got, 7/4)
	}
	if got := events[3].OutputTokens; got != 13/4 {
		t.Errorf("Generating[1].OutputTokens = %d, want %d (running total, not per-delta)", got, 13/4)
	}
	done := events[4]
	if done.OutputTokens != 3 {
		t.Errorf("Done.OutputTokens = %d, want 3 (usage.output_tokens from result envelope)", done.OutputTokens)
	}
	if done.DurationMs != 2509 {
		t.Errorf("Done.DurationMs = %d, want 2509", done.DurationMs)
	}
}

func TestGenerateTextStreaming_InjectsAPIKeyHelperSettings(t *testing.T) {
	// t.Setenv: no t.Parallel.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"apiKeyHelper":"echo test-key"}`), 0o600); err != nil {
		t.Fatalf("write settings fixture: %v", err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// The streaming path must re-inject the user's apiKeyHelper via a
	// --settings file exactly like GenerateText does; --setting-sources ""
	// alone would silently drop API-billing auth on every streaming call.
	var gotArgs []string
	fake := testutil.FakeStreamCmd(string(fixture), "", 0)
	agentInst := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotArgs = args
			return fake(ctx, name)
		},
	}
	if _, err := agentInst.GenerateTextStreaming(context.Background(), "test", "haiku", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	settingsPath, ok := flagValue(gotArgs, "--settings")
	if !ok {
		t.Fatalf("streaming argv is missing the --settings auth injection: %v", gotArgs)
	}
	if !strings.Contains(settingsPath, "entire-claude-auth") {
		t.Errorf("--settings = %q, want the injected auth settings file path", settingsPath)
	}
	if got, _ := flagValue(gotArgs, "--setting-sources"); got != "" {
		t.Errorf("--setting-sources = %q, want empty (isolation must be preserved)", got)
	}
}

func TestGenerateTextStreaming_FallbackOnUnrecognizedFlag(t *testing.T) {
	t.Parallel()

	// Old CLI: exit non-zero with stderr complaining about --output-format=stream-json.
	// Fallback path is exercised by routing the *second* call (GenerateText) to a
	// canned non-streaming envelope.
	streamCall := testutil.FakeStreamCmd("", "error: unknown flag: --output-format=stream-json", 1)
	nonStreamCall := testutil.FakeStreamCmd(`{"is_error":false,"result":"fallback ok","subtype":"success"}`, "", 0)
	calls := 0
	agentInst := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if calls == 1 {
				return streamCall(ctx, name, args...)
			}
			return nonStreamCall(ctx, name, args...)
		},
	}

	result, err := agentInst.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "fallback ok" {
		t.Errorf("result = %q, want %q", result, "fallback ok")
	}
	if calls != 2 {
		t.Errorf("expected 2 subprocess invocations (streaming + fallback), got %d", calls)
	}
}

func TestGenerateTextStreaming_EnvelopeErrorSurfaced(t *testing.T) {
	t.Parallel()

	// Verify that an is_error envelope (e.g. HTTP 404) from the result event
	// is surfaced as a typed error containing the API status. The production
	// code checks envelope.IsError BEFORE checking ctx.Err(), so an envelope
	// error wins over context cancellation if both are present — the
	// precedence is verifiable by inspection of generate_streaming.go (the
	// envelope check at the top of the post-Wait branch precedes the
	// ctx.Err() check). This test exercises the envelope-error surfacing
	// path; the precedence ordering itself is not testable here without
	// deterministic timing control over the subprocess lifecycle.
	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_error_404.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	agentInst := &ClaudeCodeAgent{
		CommandRunner: testutil.FakeStreamCmd(string(fixture), "", 0),
	}
	_, err = agentInst.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
	if err == nil {
		t.Fatal("expected error from is_error envelope")
	}
	// Streaming envelope errors must surface as a typed *agent.TextGenError so
	// the explain layer's renderTextGenError can route on Kind
	// (auth/rate-limit/config) instead of substring-matching err.Error().
	// Both Claude paths share classifyEnvelopeFields, so this also pins that
	// the streaming path cannot drift from the non-streaming one.
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("expected *agent.TextGenError, got %T: %v", err, err)
	}
	if tge.APIStatus != 404 {
		t.Errorf("APIStatus = %d, want 404", tge.APIStatus)
	}
	if tge.Kind != agent.TextGenErrorConfig {
		t.Errorf("Kind = %q, want %q", tge.Kind, agent.TextGenErrorConfig)
	}
	if tge.Provider != agent.AgentNameClaudeCode {
		t.Errorf("Provider = %q, want %q", tge.Provider, agent.AgentNameClaudeCode)
	}
}

func TestGenerateTextStreaming_SuccessOutranksLateCancellation(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fake := testutil.FakeStreamCmd(string(fixture), "", 0)
	agentInst := &ClaudeCodeAgent{
		CommandRunner: func(context.Context, string, ...string) *exec.Cmd {
			return fake(context.Background(), "claude")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	result, err := agentInst.GenerateTextStreaming(ctx, "test", "haiku", func(p agent.GenerationProgress) {
		if p.Phase == agent.PhaseGenerating {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("GenerateTextStreaming() error = %v, want completed success", err)
	}
	if result != helloWorldResult {
		t.Errorf("result = %q, want %s", result, helloWorldResult)
	}
}

func TestGenerateTextStreaming_CtxKillAfterDecodedResultReturnsSuccess(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// The child writes the complete stream (including the result envelope) and
	// then hangs; canceling ctx kills it by signal. The decoded success must
	// win over the context-caused kill — the summary was fully paid for and
	// delivered before the kill landed.
	agentInst := &ClaudeCodeAgent{CommandRunner: testutil.FakeStreamCmdHang(string(fixture), "")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	result, err := agentInst.GenerateTextStreaming(ctx, "test", "haiku", func(p agent.GenerationProgress) {
		if p.Phase == agent.PhaseGenerating {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("GenerateTextStreaming() error = %v, want decoded success to win over ctx-caused kill", err)
	}
	if result != helloWorldResult {
		t.Errorf("result = %q, want %q", result, helloWorldResult)
	}
}

func TestGenerateTextStreaming_CtxKillMidStreamCarriesEvidence(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Truncate the fixture before the result envelope: the stream dies
	// mid-generation when ctx kills the subprocess.
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	partial := strings.Join(lines[:len(lines)-1], "\n") + "\n"
	const stallMsg = "network stall: upstream not responding"

	agentInst := &ClaudeCodeAgent{CommandRunner: testutil.FakeStreamCmdHang(partial, stallMsg)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, err = agentInst.GenerateTextStreaming(ctx, "test", "haiku", func(p agent.GenerationProgress) {
		if p.Phase == agent.PhaseGenerating {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("GenerateTextStreaming() error = nil, want ctx-kill failure")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled sentinel to survive the wrapper", err)
	}
	// The wrapper must carry the diagnostic evidence: captured stderr and how
	// much stdout arrived before the kill. A bare sentinel here forces the
	// explain layer's timeout diagnostic to fabricate a cause.
	var genErr *agent.TextGenerationError
	if !errors.As(err, &genErr) {
		t.Fatalf("error = %T (%v), want *agent.TextGenerationError carrying evidence", err, err)
	}
	if genErr.Stderr != stallMsg {
		t.Errorf("Stderr = %q, want %q", genErr.Stderr, stallMsg)
	}
	if genErr.StdoutBytes == 0 {
		t.Error("StdoutBytes = 0, want the bytes read before the kill to be counted")
	}
}

func TestGenerateTextStreaming_FinalResultDoesNotHideProcessFailure(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fake := testutil.FakeStreamCmd(string(fixture), "transport closed", 23)
	agentInst := &ClaudeCodeAgent{CommandRunner: func(context.Context, string, ...string) *exec.Cmd {
		return fake(context.Background(), "claude")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var phases []agent.ProgressPhase

	_, err = agentInst.GenerateTextStreaming(ctx, "test", "haiku", func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
		if p.Phase == agent.PhaseGenerating {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("GenerateTextStreaming() error = nil, want process failure")
	}
	if slices.Contains(phases, agent.PhaseDone) {
		t.Errorf("phases = %v, must not report Done after process failure", phases)
	}
}

func equalPhases(a, b []agent.ProgressPhase) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestGenerateTextStreaming_ClassifiesStderrFailures pins that the streaming
// path classifies envelope-less failures the same way GenerateText does.
//
// This is the path `explain --generate` actually takes for Claude
// (TextGeneratorAdapter prefers streaming), and until now nine of its ten
// failure returns were plain fmt.Errorf. A stale key produced
// "claude stream failed: Invalid API key ... exit status 2" with no
// remediation, while the non-streaming fallback correctly said
// "Claude authentication failed".
func TestGenerateTextStreaming_ClassifiesStderrFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		stderr   string
		wantKind agent.TextGenErrorKind
	}{
		{"auth phrase", "Invalid API key · Please run /login", agent.TextGenErrorAuth},
		{"http 401", "ERROR: 401 Unauthorized", agent.TextGenErrorAuth},
		{"http 429", "ERROR: 429 Too Many Requests", agent.TextGenErrorRateLimit},
		{"http 404", "ERROR: 404 Not Found", agent.TextGenErrorConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &ClaudeCodeAgent{CommandRunner: testutil.FakeStreamCmd("", tc.stderr, 2)}
			_, err := ag.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			var tge *agent.TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("errors.As(*TextGenError) failed: %T %v", err, err)
			}
			if tge.Kind != tc.wantKind {
				t.Errorf("Kind = %q; want %q", tge.Kind, tc.wantKind)
			}
			if tge.Provider != agent.AgentNameClaudeCode {
				t.Errorf("Provider = %q; want claude-code", tge.Provider)
			}
			// Evidence must ride along too, same as every other failure site.
			var failure *agent.TextGenerationError
			if !errors.As(err, &failure) {
				t.Errorf("errors.As(*TextGenerationError) failed — evidence lost: %T", err)
			}
		})
	}
}
