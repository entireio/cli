package agent_test

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
)

func TestStreamingGeneratorTemplate_Generate_Success(t *testing.T) {
	t.Parallel()

	parsed := false
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: "fake",
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
		AgentName: "fake",
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
		AgentName: "fake",
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

func TestStreamingGeneratorTemplate_Generate_DoneRequiresSuccessfulProcessExit(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		exitCode int
		wantDone bool
	}{
		{name: "success", exitCode: 0, wantDone: true},
		{name: "non-zero exit", exitCode: 23, wantDone: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var phases []agent.ProgressPhase
			tmpl := &agent.StreamingGeneratorTemplate{
				AgentName: string(agent.AgentNameCodex),
				BuildCmd: func(context.Context, string, string) *exec.Cmd {
					return testutil.FakeStreamCmd("complete", "", test.exitCode)(context.Background(), "fake")
				},
				Parser: func(stdout io.Reader, progress agent.ProgressFn) (string, error) {
					result, err := io.ReadAll(stdout)
					progress(agent.GenerationProgress{Phase: agent.PhaseDone})
					return string(result), err
				},
			}

			_, err := tmpl.Generate(context.Background(), "prompt", "model", func(p agent.GenerationProgress) {
				phases = append(phases, p.Phase)
			})
			if test.exitCode == 0 && err != nil {
				t.Fatalf("Generate() error = %v, want success", err)
			}
			if test.exitCode != 0 && err == nil {
				t.Fatal("Generate() error = nil, want process failure")
			}
			if gotDone := slices.Contains(phases, agent.PhaseDone); gotDone != test.wantDone {
				t.Errorf("PhaseDone observed = %v, want %v (phases: %v)", gotDone, test.wantDone, phases)
			}
		})
	}
}

func TestStreamingGeneratorTemplate_Generate_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: "fake",
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

func TestStreamingGeneratorTemplate_Generate_ProviderErrorOutranksConcurrentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	providerErr := errors.New("provider rejected the request")
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: "fake",
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return testutil.FakeStreamCmd("provider error event\n", "", 0)(ctx, "fake", []string{}...)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // parser result is the behavior under test
			cancel()
			return "", agent.MarkProviderStreamError(providerErr)
		},
	}

	_, err := tmpl.Generate(ctx, "prompt", "model", nil)
	if !errors.Is(err, providerErr) {
		t.Fatalf("err = %v, want provider error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, provider error must outrank concurrent cancellation", err)
	}
}

func TestStreamingGeneratorTemplate_Generate_KillInducedParserFailureUsesContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: "fake",
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return testutil.BlockingCmd(ctx)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // cancellation closes the subprocess pipe
			return "", errors.New("stream ended without terminal event")
		},
	}

	_, err := tmpl.Generate(ctx, "prompt", "model", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline sentinel", err)
	}
}
