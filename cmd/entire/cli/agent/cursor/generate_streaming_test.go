package cursor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestParseCursorStream_Success(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var phases []agent.ProgressPhase
	var doneProgress agent.GenerationProgress
	result, err := parseCursorStream(bytes.NewReader(data), func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
		if p.Phase == agent.PhaseDone {
			doneProgress = p
		}
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result != "Hello, world." {
		t.Errorf("result = %q, want %q (from result.result, not delta concat)", result, "Hello, world.")
	}

	counts := map[agent.ProgressPhase]int{}
	for _, p := range phases {
		counts[p]++
	}
	if counts[agent.PhaseConnecting] != 1 {
		t.Errorf("PhaseConnecting count = %d, want 1", counts[agent.PhaseConnecting])
	}
	if counts[agent.PhaseFirstToken] != 1 {
		t.Errorf("PhaseFirstToken count = %d, want 1", counts[agent.PhaseFirstToken])
	}
	if counts[agent.PhaseGenerating] < 1 {
		t.Errorf("PhaseGenerating count = %d, want >= 1", counts[agent.PhaseGenerating])
	}
	if counts[agent.PhaseDone] != 1 {
		t.Errorf("PhaseDone count = %d, want 1", counts[agent.PhaseDone])
	}
	if doneProgress.OutputTokens != 268 {
		t.Errorf("Done.OutputTokens = %d, want 268 (from result.usage.outputTokens)", doneProgress.OutputTokens)
	}
	if doneProgress.CachedInputTokens != 4608 {
		t.Errorf("Done.CachedInputTokens = %d, want 4608 (from result.usage.cacheReadTokens)", doneProgress.CachedInputTokens)
	}
	if doneProgress.DurationMs != 3372 {
		t.Errorf("Done.DurationMs = %d, want 3372 (from result.duration_ms)", doneProgress.DurationMs)
	}
}

func TestParseCursorStream_ErrorEnvelope(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_error.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	_, err = parseCursorStream(bytes.NewReader(data), nil)
	if err == nil {
		t.Fatal("expected error from result envelope with is_error=true")
	}
	if !strings.Contains(err.Error(), "Max turns") {
		t.Errorf("error %q should mention 'Max turns'", err)
	}
}

func TestParseCursorStream_MissingResult(t *testing.T) {
	t.Parallel()

	stream := `{"type":"system","subtype":"init","session_id":"t"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"}]},"session_id":"t","timestamp_ms":1}
`
	_, err := parseCursorStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected error when stream lacks result event")
	}
}

func TestParseCursorStream_EmptyResultText(t *testing.T) {
	t.Parallel()

	// A successful (is_error:false) result event with an empty `result`
	// field must error rather than returning ("", nil) — mirrors the
	// guarantee Codex's parser makes and that RunIsolatedTextGeneratorCLI
	// makes on the non-streaming path.
	stream := `{"type":"system","subtype":"init","session_id":"t"}
{"type":"result","subtype":"success","duration_ms":1,"is_error":false,"result":"","session_id":"t"}
`
	_, err := parseCursorStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected error when result event carries empty result text")
	}
	if !strings.Contains(err.Error(), "no result text") {
		t.Errorf("error %q should mention 'no result text'", err)
	}
}

func TestCursorGenerateTextStreaming_Success(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	c := &CursorAgent{
		CommandRunner: testutil.FakeStreamCmd(string(fixture), "", 0),
	}

	var phases []agent.ProgressPhase
	result, err := c.GenerateTextStreaming(context.Background(), "test prompt", "", func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Hello, world." {
		t.Errorf("result = %q, want %q (canonical from result.result)", result, "Hello, world.")
	}
	// Expect Connecting, FirstToken, Generating (>=1), Done.
	if len(phases) < 4 {
		t.Errorf("phases = %v (count %d), want >= 4 (Connecting, FirstToken, Generating+, Done)", phases, len(phases))
	}
}

func TestCursorGenerateTextStreaming_FallbackOnUnrecognizedFlag(t *testing.T) {
	t.Parallel()

	streamCall := testutil.FakeStreamCmd("", "error: unknown flag: --stream-json", 1)
	nonStreamCall := testutil.FakeStreamCmd("fallback response", "", 0)
	calls := 0
	c := &CursorAgent{
		CommandRunner: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if calls == 1 {
				return streamCall(ctx, name, args...)
			}
			return nonStreamCall(ctx, name, args...)
		},
	}

	result, err := c.GenerateTextStreaming(context.Background(), "test", "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "fallback") {
		t.Errorf("result = %q, want substring 'fallback'", result)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (streaming + fallback)", calls)
	}
}
