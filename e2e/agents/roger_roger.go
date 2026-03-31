package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "roger-roger" {
		return
	}
	// Only register if both binaries exist (roger-roger REPL + protocol handler).
	if _, err := exec.LookPath("roger-roger"); err != nil {
		return
	}
	if _, err := exec.LookPath("entire-agent-roger-roger"); err != nil {
		return
	}
	Register(&RogerRoger{})
}

// RogerRoger implements the Agent interface using a deterministic external
// agent binary that creates files and fires hooks without making any API calls.
// Unlike built-in agents (Vogon, Claude Code, etc.), roger-roger is discovered
// via the external agent protocol (entire-agent-roger-roger binary in $PATH).
type RogerRoger struct{}

func (r *RogerRoger) Name() string                            { return "roger-roger" }
func (r *RogerRoger) Binary() string                          { return "roger-roger" }
func (r *RogerRoger) EntireAgent() string                     { return "roger-roger" }
func (r *RogerRoger) PromptPattern() string                   { return `>` }
func (r *RogerRoger) TimeoutMultiplier() float64              { return 0.5 } // Deterministic, no API calls
func (r *RogerRoger) Bootstrap() error                        { return nil }
func (r *RogerRoger) IsTransientError(_ Output, _ error) bool { return false }

// IsExternalAgent returns true — roger-roger is discovered via the external agent protocol.
func (r *RogerRoger) IsExternalAgent() bool { return true }

func (r *RogerRoger) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	// roger-roger reads prompts from stdin line by line.
	// An empty line causes the REPL to exit gracefully.
	cmd := exec.CommandContext(ctx, r.Binary())
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt + "\n\n")
	cmd.Env = filterEnv(os.Environ(), "ENTIRE_TEST_TTY")
	setupProcessGroup(cmd)
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
		Command:  "roger-roger <<< " + fmt.Sprintf("%q", prompt),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (r *RogerRoger) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("rr-test-%d", time.Now().UnixNano())
	s, err := NewTmuxSession(name, dir, []string{"ENTIRE_TEST_TTY"}, r.Binary())
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
