package codex

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

func TestParseCodexStream_Success(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var phases []agent.ProgressPhase
	result, err := parseCodexStream(bytes.NewReader(data), func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result != "Hello, world." {
		t.Errorf("result = %q, want %q", result, "Hello, world.")
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
	if counts[agent.PhaseDone] != 1 {
		t.Errorf("PhaseDone count = %d, want 1", counts[agent.PhaseDone])
	}
}

func TestParseCodexStream_ErrorEnvelope(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "stream_error.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	_, err = parseCodexStream(bytes.NewReader(data), nil)
	if err == nil {
		t.Fatal("expected error from error envelope")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error %q should mention 'model not found'", err)
	}
}

func TestParseCodexStream_MissingTurnCompleted(t *testing.T) {
	t.Parallel()

	stream := `{"type":"thread.started","thread_id":"t"}
{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"partial"}}
`
	_, err := parseCodexStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected error when stream lacks turn.completed")
	}
}

func TestCodexGenerateTextStreaming_Success(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	c := &CodexAgent{
		CommandRunner: testutil.FakeStreamCmd(string(fixture), "", 0),
	}

	var phases []agent.ProgressPhase
	result, err := c.GenerateTextStreaming(context.Background(), "test prompt", "haiku", func(p agent.GenerationProgress) {
		phases = append(phases, p.Phase)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Hello, world." {
		t.Errorf("result = %q, want %q", result, "Hello, world.")
	}
	if len(phases) != 3 {
		t.Errorf("phases = %v (count %d), want 3 (Connecting, FirstToken, Done)", phases, len(phases))
	}
}

func TestCodexGenerateTextStreaming_FallbackOnUnrecognizedFlag(t *testing.T) {
	t.Parallel()

	streamCall := testutil.FakeStreamCmd("", "error: unknown flag: --json", 1)
	nonStreamCall := testutil.FakeStreamCmd("fallback response", "", 0)
	calls := 0
	c := &CodexAgent{
		CommandRunner: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if calls == 1 {
				return streamCall(ctx, name, args...)
			}
			return nonStreamCall(ctx, name, args...)
		},
	}

	result, err := c.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
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
