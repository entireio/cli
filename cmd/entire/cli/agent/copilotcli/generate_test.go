package copilotcli

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestGenerateText_PinsMinimalToolSurface pins the exact argv handed to the
// Copilot CLI. The tool policy is a security contract (see generateTextArgs):
// text generation runs with the execution-bearing tool kinds denied and no
// blanket approval, so a change to these flags must be a reviewed decision.
func TestGenerateText_PinsMinimalToolSurface(t *testing.T) {
	t.Parallel()

	var gotArgs []string
	ag := &CopilotCLIAgent{
		CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			gotArgs = slices.Clone(args)
			return exec.CommandContext(ctx, "sh", "-c", "printf OK")
		},
	}

	result, err := ag.GenerateText(context.Background(), "prompt", "gpt-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "OK" {
		t.Fatalf("GenerateText() = %q, want %q", result, "OK")
	}

	want := []string{
		"--deny-tool", "shell",
		"--deny-tool", "write",
		"--deny-tool", "url",
		"--no-ask-user",
		"--no-custom-instructions",
		"--disable-builtin-mcps",
		"--model", "gpt-5",
	}
	if !slices.Equal(gotArgs, want) {
		t.Fatalf("argv = %q, want %q", gotArgs, want)
	}
}

func TestGenerateText_OmitsModelFlagWhenEmpty(t *testing.T) {
	t.Parallel()

	var gotArgs []string
	ag := &CopilotCLIAgent{
		CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			gotArgs = slices.Clone(args)
			return exec.CommandContext(ctx, "sh", "-c", "printf OK")
		},
	}

	if _, err := ag.GenerateText(context.Background(), "prompt", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slices.Contains(gotArgs, "--model") {
		t.Fatalf("argv %q contains --model for an empty model hint", gotArgs)
	}
}

// TestGenerateText_SmokeRealCLI exercises the pinned tool policy against the
// real Copilot CLI, because the failure mode of over-restricting the tool
// surface is a broken summarizer rather than anything subtle. It makes a real
// billable API call, so it is opt-in: run with ENTIRE_COPILOT_SMOKE=1 on a
// machine where copilot is installed and authenticated (COPILOT_GITHUB_TOKEN
// or a stored `copilot login` credential).
func TestGenerateText_SmokeRealCLI(t *testing.T) {
	t.Parallel()

	if os.Getenv("ENTIRE_COPILOT_SMOKE") == "" {
		t.Skip("set ENTIRE_COPILOT_SMOKE=1 to run the real-CLI smoke test (makes a billable API call)")
	}

	ag := &CopilotCLIAgent{}
	got, err := ag.GenerateText(context.Background(), "Reply with the single word OK and nothing else.", "")
	if err != nil {
		t.Fatalf("text generation under the minimal tool surface failed: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("text generation under the minimal tool surface returned an empty response")
	}
}
