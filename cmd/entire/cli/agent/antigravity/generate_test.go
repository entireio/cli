package antigravity

import (
	"context"
	"os/exec"
	"testing"
)

// agy 1.2.7 ignores stdin in print mode (`-p " "` fails with "empty prompt",
// `-p -` is answered as the literal message "-"), so the prompt must travel in
// argv. This pins the shape `entire dispatch --local --agent antigravity` and
// `explain --generate` depend on.
func TestGenerateText_PassesPromptInArgv(t *testing.T) {
	t.Parallel()
	var gotBinary string
	var gotArgs []string
	a := &AntigravityAgent{CommandRunner: func(ctx context.Context, binary string, argv ...string) *exec.Cmd {
		gotBinary, gotArgs = binary, argv
		return exec.CommandContext(ctx, "echo", "PONG")
	}}

	out, err := a.GenerateText(context.Background(), "Summarize this transcript.", "gemini-3.8-flash-low")
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if out != "PONG" {
		t.Fatalf("GenerateText output = %q, want the CLI's stdout", out)
	}
	if gotBinary != "agy" {
		t.Fatalf("binary = %q, want agy", gotBinary)
	}
	want := []string{"-p", "Summarize this transcript.", "--model", "gemini-3.8-flash-low"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %q, want %q", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %q, want %q", gotArgs, want)
		}
	}
}
