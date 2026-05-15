package geminicli

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

func TestParseGeminiStream_Success(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var phases []agent.ProgressPhase
	result, err := parseGeminiStream(bytes.NewReader(data), func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(result, "Hello") || !strings.Contains(result, "world") {
		t.Errorf("result = %q, want concatenated assistant content", result)
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
		t.Errorf("PhaseGenerating count = %d, want at least 1", counts[agent.PhaseGenerating])
	}
	if counts[agent.PhaseDone] != 1 {
		t.Errorf("PhaseDone count = %d, want 1 (emitted at result event)", counts[agent.PhaseDone])
	}
}

func TestParseGeminiStream_ErrorEnvelope(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_error.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	_, err = parseGeminiStream(bytes.NewReader(data), nil)
	if err == nil {
		t.Fatal("expected error from error envelope")
	}
	if !strings.Contains(err.Error(), "invalid request") {
		t.Errorf("error %q should mention 'invalid request' from error.message", err)
	}
}

func TestParseGeminiStream_EmptyStream(t *testing.T) {
	t.Parallel()

	_, err := parseGeminiStream(strings.NewReader(""), nil)
	if err == nil {
		t.Fatal("expected error from empty stream (no init, no assistant content)")
	}
}

func TestGeminiCLIGenerateTextStreaming_Success(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	g := &GeminiCLIAgent{
		CommandRunner: testutil.FakeStreamCmd(string(fixture), "", 0),
	}

	var phases []agent.ProgressPhase
	result, err := g.GenerateTextStreaming(context.Background(), "test prompt", "haiku", func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Hello") || !strings.Contains(result, "world") {
		t.Errorf("result = %q, want concatenated assistant content", result)
	}
	// Expect Connecting, FirstToken, Generating (at least once), Done.
	if len(phases) < 4 {
		t.Errorf("phases = %v (count %d), want >= 4 (Connecting, FirstToken, Generating+, Done)", phases, len(phases))
	}
}

func TestGeminiCLIGenerateTextStreaming_FallbackOnUnrecognizedFlag(t *testing.T) {
	t.Parallel()

	streamCall := testutil.FakeStreamCmd("", "error: unknown flag: --output-format", 1)
	nonStreamCall := testutil.FakeStreamCmd("fallback response", "", 0)
	calls := 0
	g := &GeminiCLIAgent{
		CommandRunner: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if calls == 1 {
				return streamCall(ctx, name, args...)
			}
			return nonStreamCall(ctx, name, args...)
		},
	}

	result, err := g.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
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
