package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "gemini-cli" {
		return
	}
	Register(&Gemini{})
	RegisterGate("gemini-cli", 2)
}

const geminiDefaultModel = "gemini-2.5-flash"

type Gemini struct{}

func (g *Gemini) Name() string               { return "gemini-cli" }
func (g *Gemini) Binary() string             { return "gemini" }
func (g *Gemini) EntireAgent() string        { return "gemini" }
func (g *Gemini) PromptPattern() string      { return `Type your message` }
func (g *Gemini) TimeoutMultiplier() float64 { return 2.5 }

func (g *Gemini) IsTransientError(out Output, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	transientPatterns := []string{
		"INTERNAL",
		"Incomplete JSON segment",
		"429",
		"TooManyRequests",
		"RESOURCE_EXHAUSTED",
		"UNAVAILABLE",
		"DEADLINE_EXCEEDED",
		"unexpected critical error",
	}
	for _, p := range transientPatterns {
		if strings.Contains(out.Stderr, p) {
			return true
		}
	}
	return false
}

func (g *Gemini) Bootstrap() error {
	// Pre-configure auth so gemini doesn't show the onboarding dialog.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	dir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	config := `{"security":{"auth":{"selectedType":"gemini-api-key"}}}`
	return os.WriteFile(filepath.Join(dir, "settings.json"), []byte(config), 0o644)
}

func (g *Gemini) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: geminiDefaultModel}
	for _, o := range opts {
		o(cfg)
	}

	// Per-prompt timeout so a slow response gets killed early enough to
	// retry within the test's overall budget.
	timeout := 60 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", prompt, "--model", cfg.Model, "-y"}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt), "--model", cfg.Model, "-y"}
	cmd := exec.CommandContext(promptCtx, g.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = append(filterEnv(os.Environ(), "ENTIRE_TEST_TTY"), "ACCESSIBLE=1")
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
		// Wrap the prompt-level deadline so IsTransientError can detect it.
		// cmd.Run() returns "signal: killed", not the context error.
		if promptCtx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
		}
	}

	return Output{
		Command:  g.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (g *Gemini) StartSession(ctx context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("gemini-test-%d", time.Now().UnixNano())
	// Unset CI and GITHUB_ACTIONS so gemini doesn't force headless mode —
	// it checks both in isHeadlessMode() and skips interactive TUI entirely.
	s, err := NewTmuxSession(name, dir, []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}, "env", "ACCESSIBLE=1", g.Binary(), "--model", geminiDefaultModel, "-y")
	if err != nil {
		return nil, err
	}

	// Dismiss startup dialogs (auth, workspace trust, etc.)
	for range 10 {
		content, err := s.WaitFor(`(Type your message|trust|Enter to select|Enter to confirm)`, 30*time.Second)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for startup prompt: %w", err)
		}
		if strings.Contains(content, "Type your message") {
			break
		}
		_ = s.SendKeys("Enter")
		time.Sleep(500 * time.Millisecond)
	}
	s.stableAtSend = ""

	return s, nil
}
