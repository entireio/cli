package agent_test

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestStreamingGeneratorTemplate_Generate_Success(t *testing.T) {
	t.Parallel()

	parsed := false
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName:   "fake",
		DisplayName: "fake",
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return testutil.FakeStreamCmd("hello\nworld\n", "", 0)(ctx, "fake", []string{}...)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			b, err := io.ReadAll(stdout)
			if err != nil {
				return "", err
			}
			parsed = true
			return strings.TrimSpace(string(b)), nil
		},
	}

	result, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello\nworld" {
		t.Errorf("result = %q, want %q", result, "hello\nworld")
	}
	if !parsed {
		t.Error("expected parser to have been called")
	}
}

func TestStreamingGeneratorTemplate_Generate_NilFieldsReturnError(t *testing.T) {
	t.Parallel()

	tmpl := &agent.StreamingGeneratorTemplate{}
	_, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
	if !errors.Is(err, agent.ErrTemplateMisconfigured) {
		t.Errorf("err = %v, want ErrTemplateMisconfigured", err)
	}
}

func TestStreamingGeneratorTemplate_Generate_UnrecognizedFlagFallback(t *testing.T) {
	t.Parallel()

	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName:   "fake",
		DisplayName: "fake",
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return testutil.FakeStreamCmd("", "error: unknown flag: --stream-json", 1)(ctx, "fake", []string{}...)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // best-effort drain in test fake; failure here is irrelevant
			return "", nil
		},
		LooksLikeUnrecognizedFlag: func(stderr string) bool {
			return strings.Contains(stderr, "unknown flag") && strings.Contains(stderr, "stream-json")
		},
	}

	_, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
	if !errors.Is(err, agent.ErrUnrecognizedStreamingFlag) {
		t.Errorf("err = %v, want ErrUnrecognizedStreamingFlag", err)
	}
}

func TestStreamingGeneratorTemplate_Generate_NonZeroExitWrapsError(t *testing.T) {
	t.Parallel()

	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName:   "fake",
		DisplayName: "fake",
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return testutil.FakeStreamCmd("partial\n", "boom\n", 1)(ctx, "fake", []string{}...)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // best-effort drain in test fake; failure here is irrelevant
			return "", nil
		},
	}

	_, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
	var failure *agent.TextGenerationError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want *TextGenerationError", err)
	}
	if !strings.Contains(failure.Stderr, "boom") {
		t.Errorf("stderr captured = %q, want substring 'boom'", failure.Stderr)
	}
	if failure.StdoutBytes == 0 {
		t.Errorf("stdoutBytes = 0, want > 0 (subprocess emitted 'partial\\n')")
	}
}

func TestStreamingGeneratorTemplate_Generate_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName:   "fake",
		DisplayName: "fake",
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return testutil.FakeStreamCmd("ok\n", "", 0)(ctx, "fake", []string{}...)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // best-effort drain in test fake; failure here is irrelevant
			return "", nil
		},
	}

	_, err := tmpl.Generate(ctx, "prompt", "model", nil)
	var failure *agent.TextGenerationError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want *TextGenerationError wrapping context error", err)
	}
	if !errors.Is(failure.Err, context.Canceled) {
		t.Errorf("inner err = %v, want context.Canceled", failure.Err)
	}
}
