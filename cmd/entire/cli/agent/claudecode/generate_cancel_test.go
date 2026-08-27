//go:build unix

package claudecode

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/execx"
)

// Both Claude text-generation entry points run the CLI directly instead of going
// through agent.RunIsolatedTextGeneratorCLI, so each needs its own
// execx.TerminateOnCancel. Without it a backgrounded grandchild inherits the
// output pipe and outlives the direct child, so the copy goroutine (GenerateText)
// or the explicit drain (GenerateTextStreaming) blocks and the deadline never
// takes effect — the `entire review` judge hang, which reaches GenerateText
// because defaultJudge prefers claude-code.
func TestGenerateText_ReturnsOnDeadlineWithPipeHoldingGrandchild(t *testing.T) {
	t.Parallel()

	// `sleep 60 &` backgrounds a grandchild holding stdout; `wait` keeps the
	// shell alive so only a group-wide kill ends both.
	hangRunner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 60 & echo ready; wait")
	}

	tests := []struct {
		name string
		call func(context.Context, *ClaudeCodeAgent) error
	}{
		{
			name: "GenerateText",
			call: func(ctx context.Context, ag *ClaudeCodeAgent) error {
				_, err := ag.GenerateText(ctx, "prompt", "haiku")
				return err
			},
		},
		{
			name: "GenerateTextStreaming",
			call: func(ctx context.Context, ag *ClaudeCodeAgent) error {
				_, err := ag.GenerateTextStreaming(ctx, "prompt", "haiku", nil)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- tt.call(ctx, &ClaudeCodeAgent{CommandRunner: hangRunner}) }()

			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v; want context.DeadlineExceeded", err)
				}
			// Stay strictly below KillWaitDelay: that backstop force-closes the
			// pipes on its own, so a looser deadline here would pass even if the
			// group-kill regressed.
			case <-time.After(execx.KillWaitDelay / 2):
				t.Fatalf("%s hung past the deadline: a grandchild held the output pipe open", tt.name)
			}
		})
	}
}
