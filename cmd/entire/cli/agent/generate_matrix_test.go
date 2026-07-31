package agent_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// windowsOSTest mirrors the windowsOS const in the internal agent test package;
// this file is package agent_test and cannot reach it.
const windowsOSTest = "windows"

// TestGenerateText_Matrix exercises each injectable non-Claude summary provider
// against the canonical failure + success scenarios. Claude has its own tests
// in claudecode/ because its classification order (envelope first) differs.
// Gemini's provider-specific phrase heuristic is covered separately in
// geminicli/ since it is the only agent with an extraClassify hook.
//
// All five non-Claude summary providers are covered, pi included since it
// gained a CommandRunner field.
// matrixGenerator is the slice of the agent surface this table exercises.
type matrixGenerator interface {
	GenerateText(ctx context.Context, prompt, model string) (string, error)
}

// matrixAgent is one row of the provider table.
type matrixAgent struct {
	name     string
	provider types.AgentName
	emptyMsg string // exact Message on exit 0 with no stdout
	make     func(runner agent.TextCommandRunner) matrixGenerator
}

func matrixAgents() []matrixAgent {
	return []matrixAgent{
		{"codex", agent.AgentNameCodex, "codex CLI returned empty output", func(r agent.TextCommandRunner) matrixGenerator {
			return &codex.CodexAgent{CommandRunner: r}
		}},
		{"cursor", agent.AgentNameCursor, "cursor CLI returned empty output", func(r agent.TextCommandRunner) matrixGenerator {
			return &cursor.CursorAgent{CommandRunner: r}
		}},
		{"copilotcli", agent.AgentNameCopilotCLI, "copilot CLI returned empty output", func(r agent.TextCommandRunner) matrixGenerator {
			return &copilotcli.CopilotCLIAgent{CommandRunner: r}
		}},
		{"geminicli", agent.AgentNameGemini, "gemini CLI returned empty output", func(r agent.TextCommandRunner) matrixGenerator {
			return &geminicli.GeminiCLIAgent{CommandRunner: r}
		}},
		{"pi", agent.AgentNamePi, "pi CLI returned empty output", func(r agent.TextCommandRunner) matrixGenerator {
			return &pi.PiAgent{CommandRunner: r}
		}},
	}
}

// One error must satisfy all three lookups. Enforced here because dropping
// withEvidence from any single return degrades timeoutDiagnostic to its
// no-information branch while every other assertion still passes.
func assertComposition(t *testing.T, err error, wantKind agent.TextGenErrorKind, wantProvider types.AgentName) {
	t.Helper()
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("errors.As(*TextGenError) failed: %T %v", err, err)
	}
	if tge.Kind != wantKind {
		t.Errorf("Kind = %q; want %q", tge.Kind, wantKind)
	}
	if tge.Provider != wantProvider {
		t.Errorf("Provider = %q; want %q", tge.Provider, wantProvider)
	}
	var failure *agent.TextGenerationError
	if !errors.As(err, &failure) {
		t.Errorf("errors.As(*TextGenerationError) failed — evidence lost, timeout diagnostic degrades silently: %T", err)
	}
}

func shRunner(script string) agent.TextCommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

func matrixCLIMissing(t *testing.T, a matrixAgent) {
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/binary/that/does/not/exist")
	}
	_, err := a.make(runner).GenerateText(context.Background(), "prompt", "")
	assertComposition(t, err, agent.TextGenErrorCLIMissing, a.provider)
}

func matrixAuthFrom401(t *testing.T, a matrixAgent) {
	_, err := a.make(shRunner(`printf 'ERROR: 401 Unauthorized' 1>&2; exit 1`)).
		GenerateText(context.Background(), "prompt", "")
	assertComposition(t, err, agent.TextGenErrorAuth, a.provider)
	var tge *agent.TextGenError
	_ = errors.As(err, &tge)
	// The CLI's own stderr must reach the user verbatim, and the real exit code
	// must be carried — the whole point of the typed error for non-Claude.
	if tge.Message != "ERROR: 401 Unauthorized" {
		t.Errorf("Message = %q; want the stderr verbatim", tge.Message)
	}
	if tge.ExitCode != 1 {
		t.Errorf("ExitCode = %d; want 1", tge.ExitCode)
	}
}

// matrixDiagnosticOnStdout covers stdout-primary CLIs (codex/cursor/copilot)
// that print diagnostics to stdout and exit non-zero with empty stderr.
func matrixDiagnosticOnStdout(t *testing.T, a matrixAgent) {
	_, err := a.make(shRunner(`printf 'HTTP 429: quota exhausted, add credits'; exit 1`)).
		GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if !strings.Contains(tge.Message, "quota exhausted") {
		t.Errorf("Message = %q; want the stdout diagnostic", tge.Message)
	}
	// Must be classified, not merely shown.
	if tge.Kind != agent.TextGenErrorRateLimit {
		t.Errorf("Kind = %q; want rate_limit from the stdout diagnostic", tge.Kind)
	}
}

// The model describing a 401 is not the provider reporting one; classifying
// prose attaches a confident, wrong remediation row.
func matrixProseOnStdout(t *testing.T, a matrixAgent) {
	_, err := a.make(shRunner(`printf 'The user was debugging a 401 Unauthorized from the payments API.'; exit 1`)).
		GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorUnknown {
		t.Errorf("Kind = %q; want unknown (prose is not a diagnosis)", tge.Kind)
	}
}

func matrixEmptyStdout(t *testing.T, a matrixAgent) {
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	_, err := a.make(runner).GenerateText(context.Background(), "prompt", "")
	var tge *agent.TextGenError
	if !errors.As(err, &tge) {
		t.Fatalf("err = %v; want *agent.TextGenError", err)
	}
	if tge.Kind != agent.TextGenErrorUnknown {
		t.Errorf("Kind = %q; want unknown", tge.Kind)
	}
	if tge.Message != a.emptyMsg {
		t.Errorf("Message = %q; want %q", tge.Message, a.emptyMsg)
	}
}

// matrixCanceled covers the ctx path, where classification is meaningless so
// the sentinel and the evidence are the entire payload.
func matrixCanceled(t *testing.T, a matrixAgent) {
	// The child writes both streams, touches a sentinel, then stalls; the parent
	// cancels only once the sentinel appears. A fixed sleep raced subprocess
	// startup and was flaky in CI. Per-subtest path because these run in
	// parallel — a shared one would let one child release another's cancel.
	ready := filepath.Join(t.TempDir(), "ready")
	runner := shRunner("printf 'partial'; printf 'stalled talking to API' 1>&2; : > " + ready + "; exec sleep 10")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, statErr := os.Stat(ready); statErr == nil {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		cancel() // cancel regardless, so a stuck child fails loudly rather than hanging
	}()
	_, err := a.make(runner).GenerateText(ctx, "prompt", "")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) failed: %T %v", err, err)
	}
	var failure *agent.TextGenerationError
	if !errors.As(err, &failure) {
		t.Fatalf("errors.As(*TextGenerationError) failed — evidence lost: %T", err)
	}
	if failure.Stderr != "stalled talking to API" {
		t.Errorf("Stderr = %q; want the stderr captured before the kill", failure.Stderr)
	}
	if failure.StdoutBytes == 0 {
		t.Error("StdoutBytes = 0; want the partial stdout counted")
	}
}

func matrixSuccess(t *testing.T, a matrixAgent) {
	out, err := a.make(shRunner(`printf 'hello world\n'`)).GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("out = %q; want to contain 'hello world'", out)
	}
}

func TestGenerateText_Matrix(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOSTest {
		t.Skip("POSIX shell")
	}

	// The five providers all delegate to the same agent.HandleTextGenResult;
	// they differ only in binary, args and the Provider stamp. So the behavioural
	// scenarios run once, against codex, and every provider gets a wiring check
	// that it reaches the shared helper with its own identity. Running all seven
	// against all five was testing one function five times.
	scenarios := []struct {
		name string
		run  func(*testing.T, matrixAgent)
	}{
		{"AuthFrom401", matrixAuthFrom401},
		{"DiagnosticOnStdout", matrixDiagnosticOnStdout},
		{"ProseOnStdoutIsNotClassified", matrixProseOnStdout},
		{"CanceledCarriesSentinelAndEvidence", matrixCanceled},
		{"Success", matrixSuccess},
	}
	representative := matrixAgents()[0]
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, representative)
		})
	}

	// Per-provider wiring: CLIMissing pins the Provider stamp and the
	// composition contract, EmptyStdout pins the per-provider emptyMsg.
	for _, a := range matrixAgents() {
		t.Run("wiring/"+a.name, func(t *testing.T) {
			t.Parallel()
			matrixCLIMissing(t, a)
			matrixEmptyStdout(t, a)
		})
	}
}
