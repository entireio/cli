package agent

import (
	"context"
	"os"
	"testing"
)

func TestNewForegroundCommandDoesNotBindToCallerCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	cmd, err := NewForegroundCommand(ctx, exe, "-test.run=TestForegroundCommandHelperProcess", "--")
	if err != nil {
		t.Fatalf("NewForegroundCommand() error = %v", err)
	}
	cmd.Env = append(cmd.Env, "ENTIRE_FOREGROUND_HELPER_PROCESS=1")

	cancel()

	if err := cmd.Run(); err != nil {
		t.Fatalf("foreground command should ignore caller cancellation and let the child run: %v", err)
	}
}

func TestForegroundCommandHelperProcess(_ *testing.T) {
	if os.Getenv("ENTIRE_FOREGROUND_HELPER_PROCESS") != "1" {
		return
	}
	os.Exit(0)
}
