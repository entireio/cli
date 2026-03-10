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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "copilot-cli" {
		return
	}
	Register(&CopilotCLI{})
	RegisterGate("copilot-cli", 4)
}

type CopilotCLI struct{}

func (c *CopilotCLI) Name() string               { return "copilot-cli" }
func (c *CopilotCLI) Binary() string             { return "copilot" }
func (c *CopilotCLI) EntireAgent() string        { return "copilot-cli" }
func (c *CopilotCLI) PromptPattern() string      { return `❯` }
func (c *CopilotCLI) TimeoutMultiplier() float64 { return 1.5 }

func (c *CopilotCLI) IsTransientError(out Output, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	combined := out.Stdout + out.Stderr
	for _, p := range []string{
		"overloaded",
		"rate limit",
		"503",
		"529",
		"ECONNRESET",
		"ETIMEDOUT",
		"Too Many Requests",
		// gpt-4.1 sometimes calls Copilot's Edit tool without old_str,
		// resulting in zero code changes despite a successful exit.
		"old_str is required",
	} {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

func (c *CopilotCLI) Bootstrap() error {
	// Copilot CLI uses GitHub authentication (gh auth or GITHUB_TOKEN).
	// No additional bootstrap needed — auth should be pre-configured.
	return nil
}

func (c *CopilotCLI) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: "gpt-4.1"}
	for _, o := range opts {
		o(cfg)
	}

	timeout := 60 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", prompt, "--model", cfg.Model, "--allow-all"}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt), "--model", cfg.Model, "--allow-all"}
	cmd := exec.CommandContext(promptCtx, c.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0")
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
		if promptCtx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
		}
	}

	out := Output{
		Command:  c.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}

	// gpt-4.1 sometimes calls Copilot's Edit tool without required parameters,
	// producing zero code changes despite exit 0. Surface this as an error so
	// the transient-error retry mechanism can restart the scenario.
	// Only trigger when copilot reports zero changes — it may retry internally.
	if err == nil && strings.Contains(out.Stdout, "old_str is required") &&
		strings.Contains(out.Stderr, "Total code changes:     +0 -0") {
		err = errors.New("copilot Edit tool failed: old_str is required")
	}

	return out, err
}

func (c *CopilotCLI) StartSession(ctx context.Context, dir string) (Session, error) {
	bin, err := exec.LookPath(c.Binary())
	if err != nil {
		return nil, fmt.Errorf("agent binary not found: %w", err)
	}

	// Forward critical env vars into the tmux session. tmux starts a new
	// shell that doesn't inherit Go's os.Environ(), so without this the
	// session lacks auth tokens (COPILOT_GITHUB_TOKEN) and HOME (for gh auth).
	if os.Getenv("COPILOT_GITHUB_TOKEN") == "" {
		return nil, errors.New("COPILOT_GITHUB_TOKEN is not set; copilot-cli interactive session requires authentication")
	}
	var envArgs []string
	for _, key := range []string{"COPILOT_GITHUB_TOKEN", "HOME", "TERM"} {
		if v := os.Getenv(key); v != "" {
			envArgs = append(envArgs, key+"="+v)
		}
	}
	args := append([]string{"env"}, envArgs...)
	args = append(args, bin, "--model", "gpt-4.1", "--allow-all")

	name := fmt.Sprintf("copilot-test-%d", time.Now().UnixNano())
	// Strip CI env vars that may affect interactive mode.
	unset := []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}
	s, err := NewTmuxSession(name, dir, unset, args[0], args[1:]...)
	if err != nil {
		return nil, err
	}

	// Dismiss startup dialogs (folder trust, etc.) then wait for the "❯" prompt.
	// Copilot CLI shows a "Confirm folder trust" dialog in interactive mode for
	// new directories. "Yes" is pre-selected, so Enter dismisses it.
	foundPrompt := false
	for range 5 {
		content, err := s.WaitFor(`(❯|Enter to select)`, 30*time.Second)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for startup prompt: %w", err)
		}
		if strings.Contains(content, "❯") && !strings.Contains(content, "Enter to select") {
			foundPrompt = true
			break
		}
		if err := s.SendKeys("Enter"); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("dismissing startup dialog: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !foundPrompt {
		_ = s.Close()
		return nil, errors.New("copilot CLI did not reach interactive prompt after dismissing startup dialogs")
	}
	s.stableAtSend = ""

	return s, nil
}
