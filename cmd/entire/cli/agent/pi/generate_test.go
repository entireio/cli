package pi

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The prompt carries condensed transcript content, so it must travel on stdin:
// argv is visible to other local users through the process table.
func TestGenerateText_PassesPromptOnStdinNotArgv(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses cat to echo the child's stdin")
	}

	const prompt = "Summarize this transcript."
	var gotBinary string
	var gotArgs []string
	a := &PiAgent{CommandRunner: func(ctx context.Context, binary string, args ...string) *exec.Cmd {
		gotBinary, gotArgs = binary, slices.Clone(args)
		return exec.CommandContext(ctx, "cat")
	}}

	out, err := a.GenerateText(t.Context(), prompt, "anthropic/claude-haiku-5-5")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if out != prompt {
		t.Fatalf("stdin = %q, want the prompt %q", out, prompt)
	}
	if gotBinary != "pi" {
		t.Fatalf("binary = %q, want pi", gotBinary)
	}
	want := []string{"--print", "--no-tools", "--no-session", "--model", "anthropic/claude-haiku-5-5"}
	if !slices.Equal(gotArgs, want) {
		t.Fatalf("argv = %q, want %q", gotArgs, want)
	}
}

func TestGenerateText_OmitsModelFlagWhenEmpty(t *testing.T) {
	t.Parallel()

	var gotArgs []string
	a := &PiAgent{CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		gotArgs = slices.Clone(args)
		return exec.CommandContext(ctx, "echo", "OK")
	}}

	if _, err := a.GenerateText(t.Context(), "prompt", ""); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if want := []string{"--print", "--no-tools", "--no-session"}; !slices.Equal(gotArgs, want) {
		t.Fatalf("argv = %q, want %q", gotArgs, want)
	}
}

// TestGenerateText_SmokeRealCLI checks that the real Pi CLI answers a prompt
// given only on stdin. It makes a real billable API call, so it is opt-in: run
// with ENTIRE_PI_SMOKE=1 on a machine where pi is installed and signed in.
func TestGenerateText_SmokeRealCLI(t *testing.T) {
	t.Parallel()

	if os.Getenv("ENTIRE_PI_SMOKE") == "" {
		t.Skip("set ENTIRE_PI_SMOKE=1 to run the real-CLI smoke test (makes a billable API call)")
	}

	got, err := (&PiAgent{}).GenerateText(context.Background(), "Reply with the single word OK and nothing else.", "")
	if err != nil {
		t.Fatalf("text generation with the prompt on stdin failed: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("text generation with the prompt on stdin returned an empty response")
	}
}
