package agent_test

import (
	"context"
	"os/exec"
	"slices"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
)

// catRunner returns a TextCommandRunner that invokes `cat`, which echoes
// stdin to stdout — letting us verify the prompt round-trips through stdin.
// It also captures the args passed to the binary for flag assertions.
func catRunner(capturedArgs *[]string) agent.TextCommandRunner {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		*capturedArgs = args
		return exec.CommandContext(ctx, "cat")
	}
}

func TestGenerateText_PromptViaStdin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		agent         agent.TextGenerator
		requiredFlags []string
		// extraCheck runs agent-specific assertions on the captured args.
		extraCheck func(t *testing.T, args []string)
	}{
		{
			name:          "codex",
			agent:         &codex.CodexAgent{},
			requiredFlags: []string{"exec", "--skip-git-repo-check"},
			extraCheck: func(t *testing.T, args []string) {
				t.Helper()
				if len(args) == 0 || args[len(args)-1] != "-" {
					t.Fatalf("expected trailing %q stdin sentinel, got %v", "-", args)
				}
			},
		},
		{
			name:          "copilot",
			agent:         &copilotcli.CopilotCLIAgent{},
			requiredFlags: []string{"--allow-all-tools", "--disable-builtin-mcps"},
		},
		{
			name:          "cursor",
			agent:         &cursor.CursorAgent{},
			requiredFlags: []string{"--print", "--force", "--trust", "--workspace"},
		},
		{
			name:          "gemini",
			agent:         &geminicli.GeminiCLIAgent{},
			requiredFlags: []string{"-p"},
			extraCheck: func(t *testing.T, args []string) {
				t.Helper()
				pIdx := slices.Index(args, "-p")
				if pIdx < 0 || pIdx+1 >= len(args) || args[pIdx+1] != " " {
					t.Fatalf("expected -p followed by space placeholder, got %v", args)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var capturedArgs []string
			setRunner(tt.agent, catRunner(&capturedArgs))

			prompt := "this prompt must arrive via stdin, not argv"
			result, err := tt.agent.GenerateText(context.Background(), prompt, "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != prompt {
				t.Fatalf("stdin round-trip failed: result=%q, want=%q", result, prompt)
			}
			if slices.Contains(capturedArgs, prompt) {
				t.Fatalf("prompt leaked into argv: %v", capturedArgs)
			}
			for _, flag := range tt.requiredFlags {
				if !slices.Contains(capturedArgs, flag) {
					t.Fatalf("expected %q in args, got %v", flag, capturedArgs)
				}
			}
			if tt.extraCheck != nil {
				tt.extraCheck(t, capturedArgs)
			}
		})
	}
}

// setRunner injects a test CommandRunner into any of the 4 supported agent
// types. This is the external-test equivalent of the package-level var
// mutation the old per-package tests used.
func setRunner(tg agent.TextGenerator, runner agent.TextCommandRunner) {
	switch a := tg.(type) {
	case *codex.CodexAgent:
		a.CommandRunner = runner
	case *copilotcli.CopilotCLIAgent:
		a.CommandRunner = runner
	case *cursor.CursorAgent:
		a.CommandRunner = runner
	case *geminicli.GeminiCLIAgent:
		a.CommandRunner = runner
	}
}

// At least one agent must implement Launcher. This test also documents the
// contract: the Launch call blocks until the agent subprocess exits.
func TestLauncher_AtLeastOneImplementor(t *testing.T) {
	t.Parallel()
	var found bool
	for _, name := range agent.List() {
		a, err := agent.Get(name)
		if err != nil {
			t.Fatalf("get %q: %v", name, err)
		}
		if _, ok := a.(agent.Launcher); ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no agent implements Launcher — entire review cannot spawn")
	}
}
