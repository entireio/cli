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

// hangRunner backgrounds a grandchild that inherits stdout and outlives the
// direct child, so killing only the child leaves the output pipe open. `wait`
// keeps the shell alive as group leader, so only a group-wide kill ends both.
func hangRunner(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 60 & echo ready; wait")
}

// assertReturnsOnDeadline fails unless call returns context.DeadlineExceeded
// once the deadline fires, rather than blocking on the held pipe forever.
func assertReturnsOnDeadline(t *testing.T, call func(context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- call(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v; want context.DeadlineExceeded", err)
		}
	// Stay strictly below KillWaitDelay: that backstop force-closes the pipes on
	// its own, so a looser bound here would pass even if the group-kill regressed.
	case <-time.After(execx.KillWaitDelay / 2):
		t.Fatal("hung past the deadline: a grandchild held the output pipe open")
	}
}

// Both Claude text-generation paths run the CLI directly instead of going through
// agent.RunIsolatedTextGeneratorCLI, so both depend on agent.PrepareIsolatedCLICmd
// for their cancellation bound. This is the `entire review` judge hang, which
// reaches GenerateText because defaultJudge prefers claude-code.
func TestGenerateText_ReturnsOnDeadlineWithPipeHoldingGrandchild(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{CommandRunner: hangRunner}
	assertReturnsOnDeadline(t, func(ctx context.Context) error {
		_, err := ag.GenerateText(ctx, "prompt", "haiku")
		return err
	})
}

func TestGenerateTextStreaming_ReturnsOnDeadlineWithPipeHoldingGrandchild(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{CommandRunner: hangRunner}
	assertReturnsOnDeadline(t, func(ctx context.Context) error {
		_, err := ag.GenerateTextStreaming(ctx, "prompt", "haiku", nil)
		return err
	})
}
