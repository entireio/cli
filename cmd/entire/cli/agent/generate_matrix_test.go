package agent_test

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
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
// Pi is absent: PiAgent has no CommandRunner field and hardcodes nil in
// RunIsolatedTextGeneratorCLIRaw, so its subprocess cannot be stubbed. Adding
// that field would let pi join this table.
func TestGenerateText_Matrix(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == windowsOSTest {
		// The fake runners below shell out to POSIX `sh` and `true`.
		t.Skip("POSIX shell")
	}

	type textGenerator interface {
		GenerateText(ctx context.Context, prompt, model string) (string, error)
	}
	type agentCase struct {
		name     string
		provider types.AgentName
		emptyMsg string // exact Message on exit 0 with no stdout
		make     func(runner agent.TextCommandRunner) textGenerator
	}
	agents := []agentCase{
		{"cursor", agent.AgentNameCursor, "cursor CLI returned empty output", func(r agent.TextCommandRunner) textGenerator {
			return &cursor.CursorAgent{CommandRunner: r}
		}},
		{"codex", agent.AgentNameCodex, "codex CLI returned empty output", func(r agent.TextCommandRunner) textGenerator {
			return &codex.CodexAgent{CommandRunner: r}
		}},
		{"copilotcli", agent.AgentNameCopilotCLI, "copilot CLI returned empty output", func(r agent.TextCommandRunner) textGenerator {
			return &copilotcli.CopilotCLIAgent{CommandRunner: r}
		}},
		{"geminicli", agent.AgentNameGemini, "gemini CLI returned empty output", func(r agent.TextCommandRunner) textGenerator {
			return &geminicli.GeminiCLIAgent{CommandRunner: r}
		}},
	}

	missing := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/binary/that/does/not/exist")
	}
	auth401 := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf 'ERROR: 401 Unauthorized' 1>&2; exit 1`)
	}
	empty := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	success := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf 'hello world\n'`)
	}
	// A CLI that writes its diagnostic to stdout and exits non-zero. codex,
	// cursor and copilot are all stdout-primary tools, so this shape is common.
	stdoutOnly := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", `printf 'HTTP 429: quota exhausted, add credits'; exit 1`)
	}
	// stdout carrying the model's PROSE about an auth error, not a diagnostic.
	// Must NOT classify — a summary that discusses a 401 is not a 401.
	stdoutProse := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			`printf 'The user was debugging a 401 Unauthorized from the payments API.'; exit 1`)
	}

	for _, a := range agents {
		t.Run(a.name+"/CLIMissing", func(t *testing.T) {
			t.Parallel()
			_, err := a.make(missing).GenerateText(context.Background(), "prompt", "")
			var tge *agent.TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("CLIMissing: err = %v; want *agent.TextGenError", err)
			}
			if tge.Kind != agent.TextGenErrorCLIMissing {
				t.Errorf("CLIMissing: Kind = %q; want cli_missing", tge.Kind)
			}
			if tge.Provider != a.provider {
				t.Errorf("CLIMissing: Provider = %q; want %q", tge.Provider, a.provider)
			}
		})
		t.Run(a.name+"/AuthFrom401", func(t *testing.T) {
			t.Parallel()
			_, err := a.make(auth401).GenerateText(context.Background(), "prompt", "")
			var tge *agent.TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("AuthFrom401: err = %v; want *agent.TextGenError", err)
			}
			if tge.Kind != agent.TextGenErrorAuth {
				t.Errorf("AuthFrom401: Kind = %q; want auth", tge.Kind)
			}
			// The CLI's own stderr must reach the user verbatim, and the real
			// exit code must be carried — this is the whole point of the typed
			// error for non-Claude providers.
			if tge.Message != "ERROR: 401 Unauthorized" {
				t.Errorf("AuthFrom401: Message = %q; want the stderr verbatim", tge.Message)
			}
			if tge.ExitCode != 1 {
				t.Errorf("AuthFrom401: ExitCode = %d; want 1", tge.ExitCode)
			}
		})
		t.Run(a.name+"/DiagnosticOnStdout", func(t *testing.T) {
			t.Parallel()
			_, err := a.make(stdoutOnly).GenerateText(context.Background(), "prompt", "")
			var tge *agent.TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("DiagnosticOnStdout: err = %v; want *agent.TextGenError", err)
			}
			// Regression guard: stderr is empty here, so the detail has to come
			// from stdout. Reading stderr alone loses it and renders "(no
			// diagnostic detail available)".
			if !strings.Contains(tge.Message, "quota exhausted") {
				t.Errorf("DiagnosticOnStdout: Message = %q; want the stdout diagnostic", tge.Message)
			}
			// And it must be CLASSIFIED, not just shown. codex/cursor/copilot
			// are stdout-primary, so leaving these Unknown would mean three of
			// six providers never get actionable messaging.
			if tge.Kind != agent.TextGenErrorRateLimit {
				t.Errorf("DiagnosticOnStdout: Kind = %q; want rate_limit from the stdout diagnostic", tge.Kind)
			}
		})
		t.Run(a.name+"/ProseOnStdoutIsNotClassified", func(t *testing.T) {
			t.Parallel()
			_, err := a.make(stdoutProse).GenerateText(context.Background(), "prompt", "")
			var tge *agent.TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("ProseOnStdout: err = %v; want *agent.TextGenError", err)
			}
			// The model describing a 401 is not the provider reporting one.
			// Classifying prose attaches a confident, wrong remediation row —
			// strictly worse than Unknown, which shows the text honestly.
			if tge.Kind != agent.TextGenErrorUnknown {
				t.Errorf("ProseOnStdout: Kind = %q; want unknown (prose is not a diagnosis)", tge.Kind)
			}
		})
		t.Run(a.name+"/EmptyStdout", func(t *testing.T) {
			t.Parallel()
			_, err := a.make(empty).GenerateText(context.Background(), "prompt", "")
			var tge *agent.TextGenError
			if !errors.As(err, &tge) {
				t.Fatalf("EmptyStdout: err = %v; want *agent.TextGenError", err)
			}
			if tge.Kind != agent.TextGenErrorUnknown {
				t.Errorf("EmptyStdout: Kind = %q; want unknown", tge.Kind)
			}
			if tge.Message != a.emptyMsg {
				t.Errorf("EmptyStdout: Message = %q; want %q", tge.Message, a.emptyMsg)
			}
		})
		t.Run(a.name+"/Success", func(t *testing.T) {
			t.Parallel()
			out, err := a.make(success).GenerateText(context.Background(), "prompt", "")
			if err != nil {
				t.Fatalf("Success: unexpected error: %v", err)
			}
			if !strings.Contains(out, "hello world") {
				t.Errorf("Success: out = %q; want to contain 'hello world'", out)
			}
		})
	}
}
