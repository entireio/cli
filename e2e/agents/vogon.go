package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "vogon" {
		return
	}
	// Only register if the binary exists (built by the test runner).
	if _, err := exec.LookPath("vogon"); err != nil {
		return
	}
	Register(&Vogon{})
}

// Vogon implements the Agent interface using a deterministic binary
// that creates files and fires hooks without making any API calls.
// Named after the Vogons from The Hitchhiker's Guide to the Galaxy.
type Vogon struct{}

func (v *Vogon) Name() string               { return "vogon" }
func (v *Vogon) Binary() string             { return "vogon" }
func (v *Vogon) EntireAgent() string        { return "vogon" }
func (v *Vogon) PromptPattern() string      { return `>` }
func (v *Vogon) TimeoutMultiplier() float64 { return 0.5 } // Faster than real agents

func (v *Vogon) Bootstrap() error { return nil }

func (v *Vogon) IsTransientError(_ Output, _ error) bool { return false }

func (v *Vogon) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	args := []string{"-p", prompt}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt)}

	cmd := exec.CommandContext(ctx, v.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = filterEnv(os.Environ(), "ENTIRE_TEST_TTY")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return Output{
		Command:  v.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (v *Vogon) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("vogon-test-%d", time.Now().UnixNano())
	s, err := NewTmuxSession(name, dir, []string{"ENTIRE_TEST_TTY"}, v.Binary())
	if err != nil {
		return nil, err
	}

	// Wait for the interactive prompt.
	if _, err := s.WaitFor(`>`, 10*time.Second); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("waiting for startup prompt: %w", err)
	}
	s.stableAtSend = ""

	return s, nil
}
