package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "antigravity" {
		return
	}
	if _, err := exec.LookPath(antigravityBinary); err != nil {
		return
	}
	Register(&Antigravity{})
	RegisterGate("antigravity", 2)
}

const (
	antigravityBinary       = "agy"
	antigravityDefaultModel = "gemini-2.5-flash"
)

// Antigravity implements the Agent interface for the agy CLI (Antigravity 2.0,
// successor to Gemini CLI).
type Antigravity struct{}

func (a *Antigravity) Name() string        { return "antigravity" }
func (a *Antigravity) Binary() string      { return antigravityBinary }
func (a *Antigravity) EntireAgent() string { return "antigravity" }

// PromptPattern is a placeholder until the real interactive prompt pattern is
// observed against a working `agy` session. Tracked in the Deferred table.
func (a *Antigravity) PromptPattern() string      { return `>` }
func (a *Antigravity) TimeoutMultiplier() float64 { return 2.5 }

func (a *Antigravity) Bootstrap() error { return nil }

func (a *Antigravity) IsTransientError(out Output, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	combined := out.Stdout + out.Stderr
	transientPatterns := []string{
		"timed out waiting for response",
		"429",
		"RESOURCE_EXHAUSTED",
		"UNAVAILABLE",
		"DEADLINE_EXCEEDED",
		"INTERNAL",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
	}
	for _, p := range transientPatterns {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

func (a *Antigravity) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: antigravityDefaultModel}
	for _, o := range opts {
		o(cfg)
	}

	timeout := 60 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", prompt, "--dangerously-skip-permissions"}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt), "--dangerously-skip-permissions"}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
		displayArgs = append(displayArgs, "--model", cfg.Model)
	}

	cmd := exec.CommandContext(promptCtx, a.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = antigravityPromptEnv(dir)
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
		if promptCtx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
		}
	}

	return Output{
		Command:  a.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (a *Antigravity) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("antigravity-test-%d", time.Now().UnixNano())

	envArgs := []string{"ACCESSIBLE=1", "HOME=" + antigravityTestHomeDir(dir)}
	for _, key := range []string{"TERM"} {
		if v := os.Getenv(key); v != "" {
			envArgs = append(envArgs, key+"="+v)
		}
	}

	args := append([]string{"env"}, envArgs...)
	args = append(args, a.Binary(), "--dangerously-skip-permissions")
	s, err := NewTmuxSession(name, dir, []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY", "HOME"}, args[0], args[1:]...)
	if err != nil {
		return nil, err
	}

	for range 10 {
		content, err := s.WaitFor(`(>|trust|Enter to select|Enter to confirm|Acknowledge)`, 30*time.Second)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for startup prompt: %w", err)
		}
		if strings.Contains(content, ">") {
			break
		}
		_ = s.SendKeys("Enter")
		time.Sleep(500 * time.Millisecond)
	}
	s.stableAtSend = ""

	return s, nil
}

func antigravityTestHomeDir(repoDir string) string {
	return filepath.Join(filepath.Dir(repoDir), filepath.Base(repoDir)+"-antigravity-home")
}

func antigravityPromptEnv(repoDir string) []string {
	return append(
		filterEnv(os.Environ(), "ENTIRE_TEST_TTY"),
		"ACCESSIBLE=1",
		"HOME="+antigravityTestHomeDir(repoDir),
	)
}
