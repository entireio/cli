package agent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

const streamingTemplateHelperMarker = "__entire_streaming_template_helper__"

// TestStreamingGeneratorTemplateHelperProcess is the subprocess entrypoint
// used by this file. It must not call t.Parallel because it calls os.Exit.
func TestStreamingGeneratorTemplateHelperProcess(_ *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 {
		return
	}

	args := os.Args[separator+1:]
	if len(args) < 2 || args[0] != streamingTemplateHelperMarker {
		return
	}

	switch args[1] {
	case "output":
		_, _ = fmt.Fprint(os.Stdout, streamingTemplateHelperArg(args, 2))
		_, _ = fmt.Fprint(os.Stderr, streamingTemplateHelperArg(args, 3))
		exitCode, err := strconv.Atoi(streamingTemplateHelperArg(args, 4))
		if err != nil {
			os.Exit(2)
		}
		os.Exit(exitCode)
	case "blocking":
		time.Sleep(time.Hour)
	default:
		os.Exit(2)
	}
}

func streamingTemplateHelperArg(args []string, index int) string {
	if index >= len(args) {
		return ""
	}
	return args[index]
}

func streamingTemplateCmd(ctx context.Context, stdout, stderr string, exitCode int) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestStreamingGeneratorTemplateHelperProcess$", "--",
		streamingTemplateHelperMarker, "output", stdout, stderr, strconv.Itoa(exitCode))
}

func detachedStreamingTemplateCmd(stdout, stderr string, exitCode int) *exec.Cmd {
	return streamingTemplateCmd(context.Background(), stdout, stderr, exitCode)
}

func TestStreamingGeneratorTemplate_Generate_Success(t *testing.T) {
	t.Parallel()

	parsed := false
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return streamingTemplateCmd(ctx, "hello\nworld\n", "", 0)
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
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return streamingTemplateCmd(ctx, "", "error: unknown flag: --stream-json", 1)
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
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return streamingTemplateCmd(ctx, "partial\n", "boom\n", 1)
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
					return detachedStreamingTemplateCmd("complete", "", test.exitCode)
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
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return streamingTemplateCmd(ctx, "ok\n", "", 0)
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

func TestStreamingGeneratorTemplate_Generate_SuccessOutranksLateCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(context.Context, string, string) *exec.Cmd {
			return detachedStreamingTemplateCmd("complete", "", 0)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			result, err := io.ReadAll(stdout)
			cancel()
			return string(result), err
		},
	}

	result, err := tmpl.Generate(ctx, "prompt", "model", nil)
	if err != nil {
		t.Fatalf("Generate() error = %v, want success", err)
	}
	if result != "complete" {
		t.Errorf("result = %q, want complete", result)
	}
}

func TestStreamingGeneratorTemplate_Generate_UnknownProviderErrorPreservesConcurrentCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	providerErr := errors.New("provider rejected the request")
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return streamingTemplateCmd(ctx, "provider error event\n", "", 0)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // parser result is the behavior under test
			cancel()
			return "", agent.MarkProviderStreamError(providerErr)
		},
	}

	_, err := tmpl.Generate(ctx, "prompt", "model", nil)
	failure := requireStreamingTextGenerationError(t, err)
	if failure.Kind != agent.TextGenerationErrorUnknown {
		t.Errorf("Kind = %v, want Unknown", failure.Kind)
	}
	if !errors.Is(err, providerErr) {
		t.Fatalf("err = %v, want provider error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want concurrent cancellation preserved", err)
	}
}

func TestStreamingGeneratorTemplate_Generate_KillInducedParserFailureUsesContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return exec.CommandContext(ctx, os.Args[0],
				"-test.run=^TestStreamingGeneratorTemplateHelperProcess$", "--",
				streamingTemplateHelperMarker, "blocking")
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

func TestStreamingGeneratorTemplate_Generate_MissingCLIIsClassified(t *testing.T) {
	t.Parallel()

	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(ctx context.Context, _, _ string) *exec.Cmd {
			return exec.CommandContext(ctx, "definitely-not-installed-streaming-summary-cli")
		},
		Parser: drainStreamingTemplateOutput,
	}

	_, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
	failure := requireStreamingTextGenerationError(t, err)
	if failure.Kind != agent.TextGenerationErrorCLIMissing {
		t.Errorf("Kind = %v, want CLIMissing", failure.Kind)
	}
	if failure.Provider != agent.AgentNameCodex {
		t.Errorf("Provider = %q, want %q", failure.Provider, agent.AgentNameCodex)
	}
	assertOneStreamingTextGenerationError(t, err)
}

func TestStreamingGeneratorTemplate_Generate_ClassifiesExitStderr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stderr string
		kind   agent.TextGenerationErrorKind
	}{
		{name: "auth", stderr: "unexpected status 401", kind: agent.TextGenerationErrorAuth},
		{name: "bare worker number", stderr: "worker 404 failed to start", kind: agent.TextGenerationErrorUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			tmpl := &agent.StreamingGeneratorTemplate{
				AgentName: string(agent.AgentNameCodex),
				BuildCmd: func(context.Context, string, string) *exec.Cmd {
					return detachedStreamingTemplateCmd("partial", test.stderr, 23)
				},
				Parser: drainStreamingTemplateOutput,
			}

			_, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
			failure := requireStreamingTextGenerationError(t, err)
			if failure.Kind != test.kind {
				t.Errorf("Kind = %v, want %v", failure.Kind, test.kind)
			}
			if failure.Stderr != test.stderr {
				t.Errorf("Stderr = %q, want %q", failure.Stderr, test.stderr)
			}
			if failure.Message != test.stderr {
				t.Errorf("Message = %q, want provider detail %q", failure.Message, test.stderr)
			}
			if failure.ExitCode != 23 {
				t.Errorf("ExitCode = %d, want 23", failure.ExitCode)
			}
			assertOneStreamingTextGenerationError(t, err)
		})
	}
}

func TestStreamingGeneratorTemplate_Generate_UnknownFailurePreservesDeadlineAndCause(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(context.Context, string, string) *exec.Cmd {
			return detachedStreamingTemplateCmd("", "provider exploded", 23)
		},
		Parser: drainStreamingTemplateOutput,
	}

	_, err := tmpl.Generate(ctx, "prompt", "model", nil)
	failure := requireStreamingTextGenerationError(t, err)
	if failure.Kind != agent.TextGenerationErrorUnknown {
		t.Errorf("Kind = %v, want Unknown", failure.Kind)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded in chain", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("err = %v, want subprocess cause preserved", err)
	}
	assertOneStreamingTextGenerationError(t, err)
}

func TestStreamingGeneratorTemplate_Generate_PreservesParserAndProcessFailures(t *testing.T) {
	t.Parallel()

	parserErr := errors.New("parser rejected terminal event")
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(context.Context, string, string) *exec.Cmd {
			return detachedStreamingTemplateCmd("partial", "transport closed", 23)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // parser error is the behavior under test
			return "", parserErr
		},
	}

	_, err := tmpl.Generate(context.Background(), "prompt", "model", nil)
	if !errors.Is(err, parserErr) {
		t.Errorf("err = %v, want parser cause", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("err = %v, want process cause", err)
	}
}

func TestStreamingGeneratorTemplate_Generate_SpecificProviderErrorOutranksDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	providerErr := errors.New("unexpected status 401")
	tmpl := &agent.StreamingGeneratorTemplate{
		AgentName: string(agent.AgentNameCodex),
		BuildCmd: func(context.Context, string, string) *exec.Cmd {
			return detachedStreamingTemplateCmd("provider error event", "transport closed", 23)
		},
		Parser: func(stdout io.Reader, _ agent.ProgressFn) (string, error) {
			_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // parser result is the behavior under test
			return "", agent.MarkProviderStreamError(providerErr)
		},
	}

	_, err := tmpl.Generate(ctx, "prompt", "model", nil)
	failure := requireStreamingTextGenerationError(t, err)
	if failure.Kind != agent.TextGenerationErrorAuth {
		t.Errorf("Kind = %v, want Auth", failure.Kind)
	}
	if failure.Message != providerErr.Error() {
		t.Errorf("Message = %q, want decoded provider detail %q", failure.Message, providerErr)
	}
	if !errors.Is(err, providerErr) {
		t.Errorf("err = %v, want provider error in chain", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("specific provider error should outrank deadline: %v", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("err = %v, want process cause preserved", err)
	}
	assertOneStreamingTextGenerationError(t, err)
}

func drainStreamingTemplateOutput(stdout io.Reader, _ agent.ProgressFn) (string, error) {
	_, err := io.Copy(io.Discard, stdout)
	return "", err
}

func requireStreamingTextGenerationError(t *testing.T, err error) *agent.TextGenerationError {
	t.Helper()
	var failure *agent.TextGenerationError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want *TextGenerationError", err)
	}
	return failure
}

func assertOneStreamingTextGenerationError(t *testing.T, err error) {
	t.Helper()
	count := 0
	targetType := reflect.TypeFor[*agent.TextGenerationError]()
	for current := err; current != nil; current = errors.Unwrap(current) {
		if reflect.TypeOf(current) == targetType {
			count++
		}
	}
	if count != 1 {
		t.Errorf("TextGenerationError count = %d, want 1 (chain: %v)", count, err)
	}
}
