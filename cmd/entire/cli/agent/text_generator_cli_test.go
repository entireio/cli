package agent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

const textGeneratorHelperTest = "TestTextGeneratorCLIHelperProcess"
const textGeneratorHelperMarker = "__entire_text_generator_helper__"

// TestTextGeneratorCLIHelperProcess is the subprocess entrypoint used by this
// file's tests. It must not call t.Parallel because helper modes call os.Exit.
func TestTextGeneratorCLIHelperProcess(_ *testing.T) {
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
	if len(args) == 0 || args[0] != textGeneratorHelperMarker {
		return
	}
	args = args[1:]
	if len(args) == 0 {
		os.Exit(2)
	}

	switch args[0] {
	case "success":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "empty-stderr":
		_, _ = fmt.Fprint(os.Stderr, helperArg(args, 1))
		os.Exit(0)
	case "stderr-failure":
		_, _ = fmt.Fprint(os.Stderr, helperArg(args, 1))
		os.Exit(23)
	case "stdout-failure":
		_, _ = fmt.Fprint(os.Stdout, helperArg(args, 1))
		os.Exit(23)
	case "blocking":
		time.Sleep(time.Hour)
	default:
		os.Exit(2)
	}
}

func helperArg(args []string, index int) string {
	if index >= len(args) {
		return ""
	}
	return args[index]
}

func helperRunner(mode string, details ...string) agent.TextCommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		args := []string{"-test.run=^" + textGeneratorHelperTest + "$", "--", textGeneratorHelperMarker, mode}
		args = append(args, details...)
		return exec.CommandContext(ctx, os.Args[0], args...)
	}
}

func detachedHelperRunner(mode string, details ...string) agent.TextCommandRunner {
	return func(context.Context, string, ...string) *exec.Cmd {
		args := []string{"-test.run=^" + textGeneratorHelperTest + "$", "--", textGeneratorHelperMarker, mode}
		args = append(args, details...)
		return exec.CommandContext(context.Background(), os.Args[0], args...)
	}
}

func TestSummaryProviderMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider types.AgentName
		binary   string
		label    string
	}{
		{provider: agent.AgentNameClaudeCode, binary: "claude", label: "Claude"},
		{provider: agent.AgentNameCodex, binary: "codex", label: "Codex"},
		{provider: agent.AgentNameCopilotCLI, binary: "copilot", label: "Copilot CLI"},
		{provider: agent.AgentNameCursor, binary: "agent", label: "Cursor"},
		{provider: agent.AgentNameGemini, binary: "gemini", label: "Gemini"},
		{provider: agent.AgentNamePi, binary: "pi", label: "Pi"},
	}

	for _, test := range tests {
		t.Run(string(test.provider), func(t *testing.T) {
			t.Parallel()
			if got := agent.SummaryCLIBinaryName(test.provider); got != test.binary {
				t.Errorf("SummaryCLIBinaryName(%q) = %q, want %q", test.provider, got, test.binary)
			}
			if got := agent.SummaryProviderErrorLabel(test.provider); got != test.label {
				t.Errorf("SummaryProviderErrorLabel(%q) = %q, want %q", test.provider, got, test.label)
			}
		})
	}
}

func TestRunIsolatedTextGeneratorCLI_ClassifiesContextualHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message string
		kind    agent.TextGenerationErrorKind
	}{
		{message: "HTTP 401 Unauthorized", kind: agent.TextGenerationErrorAuth},
		{message: "HTTP status 403", kind: agent.TextGenerationErrorAuth},
		{message: "HTTP status code 429", kind: agent.TextGenerationErrorRateLimit},
		{message: "status code 400", kind: agent.TextGenerationErrorConfig},
		{message: "unexpected status 401 Unauthorized", kind: agent.TextGenerationErrorAuth},
		{message: "provider response status 404", kind: agent.TextGenerationErrorConfig},
		{message: "API response status: 429", kind: agent.TextGenerationErrorRateLimit},
	}

	for _, test := range tests {
		t.Run(test.message, func(t *testing.T) {
			t.Parallel()
			_, err := agent.RunIsolatedTextGeneratorCLI(
				context.Background(), helperRunner("stderr-failure", test.message), agent.AgentNameCodex, nil, "",
			)
			failure := requireTextGenerationError(t, err)
			if failure.Kind != test.kind {
				t.Errorf("Kind = %v, want %v (error: %v)", failure.Kind, test.kind, err)
			}
		})
	}
}

func TestRunIsolatedTextGeneratorCLI_DoesNotClassifyBareNumbers(t *testing.T) {
	t.Parallel()

	tests := []string{
		"bare 404",
		"worker 404 failed to start",
		"exit status 404",
		"port 14010",
		"took 429ms",
		"request-id=abc-401-def",
	}

	for _, message := range tests {
		t.Run(message, func(t *testing.T) {
			t.Parallel()
			_, err := agent.RunIsolatedTextGeneratorCLI(
				context.Background(), helperRunner("stderr-failure", message), agent.AgentNameCodex, nil, "",
			)
			failure := requireTextGenerationError(t, err)
			if failure.Kind != agent.TextGenerationErrorUnknown {
				t.Errorf("Kind = %v, want Unknown (error: %v)", failure.Kind, err)
			}
		})
	}
}

func TestRunIsolatedTextGeneratorCLI_FallbackContract(t *testing.T) {
	t.Parallel()

	t.Run("missing binary", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "definitely-not-installed-summary-cli")
			}, agent.AgentNameCodex, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorCLIMissing {
			t.Errorf("Kind = %v, want CLIMissing", failure.Kind)
		}
		if failure.Provider != agent.AgentNameCodex {
			t.Errorf("Provider = %q, want %q", failure.Provider, agent.AgentNameCodex)
		}
	})

	t.Run("provider label is not duplicated", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, "definitely-not-installed-summary-cli")
			}, agent.AgentNameCopilotCLI, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if strings.Contains(failure.Message, "CLI CLI") {
			t.Errorf("Message = %q, contains duplicated CLI label", failure.Message)
		}
	})

	t.Run("auth stderr", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), helperRunner("stderr-failure", "HTTP 401 Unauthorized"), agent.AgentNameCodex, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorAuth {
			t.Errorf("Kind = %v, want Auth", failure.Kind)
		}
		if failure.ExitCode != 23 {
			t.Errorf("ExitCode = %d, want 23", failure.ExitCode)
		}
		if failure.Stderr != "HTTP 401 Unauthorized" {
			t.Errorf("Stderr = %q, want captured auth stderr", failure.Stderr)
		}
	})

	t.Run("generic nonzero", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), helperRunner("stderr-failure", "provider exploded"), agent.AgentNameCodex, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorUnknown {
			t.Errorf("Kind = %v, want Unknown", failure.Kind)
		}
		if !strings.Contains(failure.Message, "provider exploded") {
			t.Errorf("Message = %q, want stderr detail", failure.Message)
		}
	})

	t.Run("stdout fallback", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), helperRunner("stdout-failure", "stdout detail"), agent.AgentNameCodex, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if failure.StdoutBytes != len("stdout detail") {
			t.Errorf("StdoutBytes = %d, want %d", failure.StdoutBytes, len("stdout detail"))
		}
		if !strings.Contains(failure.Message, "stdout detail") {
			t.Errorf("Message = %q, want stdout fallback detail", failure.Message)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		_, err := agent.RunIsolatedTextGeneratorCLI(ctx, helperRunner("blocking"), agent.AgentNameCodex, nil, "")
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorUnknown {
			t.Errorf("Kind = %v, want Unknown", failure.Kind)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled in chain", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		t.Cleanup(cancel)
		_, err := agent.RunIsolatedTextGeneratorCLI(ctx, helperRunner("blocking"), agent.AgentNameCodex, nil, "")
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorUnknown {
			t.Errorf("Kind = %v, want Unknown", failure.Kind)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded in chain", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		result, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), helperRunner("success"), agent.AgentNameCodex, nil, "generated summary\n",
		)
		if err != nil {
			t.Fatalf("RunIsolatedTextGeneratorCLI() error = %v", err)
		}
		if result != "generated summary" {
			t.Errorf("result = %q, want %q", result, "generated summary")
		}
	})

	t.Run("empty stdout with recognized auth stderr", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), helperRunner("empty-stderr", "HTTP status 401"), agent.AgentNameCodex, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorAuth {
			t.Errorf("Kind = %v, want Auth", failure.Kind)
		}
		if failure.Stderr != "HTTP status 401" {
			t.Errorf("Stderr = %q, want retained stderr", failure.Stderr)
		}
	})

	t.Run("empty stdout with generic stderr", func(t *testing.T) {
		t.Parallel()
		_, err := agent.RunIsolatedTextGeneratorCLI(
			context.Background(), helperRunner("empty-stderr", "diagnostic detail"), agent.AgentNameCodex, nil, "",
		)
		failure := requireTextGenerationError(t, err)
		if failure.Kind != agent.TextGenerationErrorUnknown {
			t.Errorf("Kind = %v, want Unknown", failure.Kind)
		}
		if failure.Stderr != "diagnostic detail" {
			t.Errorf("Stderr = %q, want retained stderr", failure.Stderr)
		}
		if !strings.Contains(failure.Message, "diagnostic detail") {
			t.Errorf("Message = %q, want retained stderr detail", failure.Message)
		}
	})
}

func TestRunIsolatedTextGeneratorCLI_SpecificProviderSignalOutranksContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		newCtx   func(t *testing.T) context.Context
		sentinel error
	}{
		{
			name: "cancellation",
			newCtx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				cancel()
				return ctx
			},
			sentinel: context.Canceled,
		},
		{
			name: "deadline",
			newCtx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			sentinel: context.DeadlineExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := test.newCtx(t)
			_, err := agent.RunIsolatedTextGeneratorCLI(
				ctx, detachedHelperRunner("stderr-failure", "HTTP 401 Unauthorized"), agent.AgentNameCodex, nil, "",
			)
			failure := requireTextGenerationError(t, err)
			if failure.Kind != agent.TextGenerationErrorAuth {
				t.Errorf("Kind = %v, want Auth (error: %v)", failure.Kind, err)
			}
			if errors.Is(err, test.sentinel) {
				t.Errorf("specific provider error should outrank %v: %v", test.sentinel, err)
			}
		})
	}
}

func TestRunIsolatedTextGeneratorCLI_UnknownFailurePreservesCompletedContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ctx      context.Context
		sentinel error
	}{
		{name: "cancellation", ctx: canceledContext(), sentinel: context.Canceled},
		{name: "deadline", ctx: expiredContext(), sentinel: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := agent.RunIsolatedTextGeneratorCLI(
				test.ctx, detachedHelperRunner("stderr-failure", "provider exploded"), agent.AgentNameCodex, nil, "",
			)
			failure := requireTextGenerationError(t, err)
			if failure.Kind != agent.TextGenerationErrorUnknown {
				t.Errorf("Kind = %v, want Unknown", failure.Kind)
			}
			if !errors.Is(err, test.sentinel) {
				t.Errorf("err = %v, want %v in chain", err, test.sentinel)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Errorf("err = %v, want subprocess cause preserved", err)
			}
		})
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func expiredContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	return ctx
}

func TestRunIsolatedTextGeneratorCLI_GeminiAuthPhrase(t *testing.T) {
	t.Parallel()

	_, err := agent.RunIsolatedTextGeneratorCLI(
		context.Background(), helperRunner("stderr-failure", "Please set an Auth method"), agent.AgentNameGemini, nil, "",
	)
	failure := requireTextGenerationError(t, err)
	if failure.Kind != agent.TextGenerationErrorAuth {
		t.Errorf("Kind = %v, want Auth", failure.Kind)
	}
}

func TestFallbackGeneratorsContainExactlyOneTextGenerationError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		generator agent.TextGenerator
	}{
		{name: "codex", generator: &codex.CodexAgent{CommandRunner: helperRunner("stderr-failure", "HTTP 401 Unauthorized")}},
		{name: "copilot", generator: &copilotcli.CopilotCLIAgent{CommandRunner: helperRunner("stderr-failure", "HTTP 401 Unauthorized")}},
		{name: "cursor", generator: &cursor.CursorAgent{CommandRunner: helperRunner("stderr-failure", "HTTP 401 Unauthorized")}},
		{name: "gemini", generator: &geminicli.GeminiCLIAgent{CommandRunner: helperRunner("stderr-failure", "HTTP 401 Unauthorized")}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := test.generator.GenerateText(context.Background(), "prompt", "")
			if count := countConcreteTextGenerationErrors(err); count != 1 {
				t.Fatalf("TextGenerationError count = %d, want 1 (chain: %v)", count, err)
			}
		})
	}
}

func requireTextGenerationError(t *testing.T, err error) *agent.TextGenerationError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var failure *agent.TextGenerationError
	if !errors.As(err, &failure) {
		t.Fatalf("err = %T %v, want *TextGenerationError", err, err)
	}
	if failure.Err == nil {
		t.Fatal("TextGenerationError.Err must be non-nil")
	}
	return failure
}

func countConcreteTextGenerationErrors(err error) int {
	count := 0
	targetType := reflect.TypeFor[*agent.TextGenerationError]()
	for err != nil {
		if reflect.TypeOf(err) == targetType {
			count++
		}
		err = errors.Unwrap(err)
	}
	return count
}

func TestTextGenerationError_DefensiveNilReceiverAndErr(t *testing.T) {
	t.Parallel()

	var nilFailure *agent.TextGenerationError
	if got := nilFailure.Error(); got == "" {
		t.Error("nil receiver Error() returned an empty message")
	}
	if err := nilFailure.Unwrap(); err != nil {
		t.Errorf("nil receiver Unwrap() = %v, want nil", err)
	}

	failure := &agent.TextGenerationError{Message: "fallback message"}
	if got := failure.Error(); got != "fallback message" {
		t.Errorf("Error() = %q, want %q", got, "fallback message")
	}
	if err := failure.Unwrap(); err != nil {
		t.Errorf("Unwrap() = %v, want nil", err)
	}
}

func TestStripGitEnv(t *testing.T) {
	t.Parallel()

	env := []string{
		"HOME=/home/user",
		"GIT_DIR=/some/dir",
		"PATH=/usr/bin",
		"GIT_WORK_TREE=/some/tree",
		"EDITOR=vim",
	}
	filtered := agent.StripGitEnv(env)

	for _, entry := range filtered {
		if strings.HasPrefix(entry, "GIT_") {
			t.Fatalf("GIT_ variable not stripped: %s", entry)
		}
	}
	if len(filtered) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(filtered), filtered)
	}
}
