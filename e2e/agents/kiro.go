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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "kiro" {
		return
	}
	if _, err := exec.LookPath("kiro-cli"); err != nil {
		return
	}
	Register(&kiroAgent{timeout: 2 * time.Minute})
}

type kiroAgent struct {
	timeout time.Duration
}

func (a *kiroAgent) Name() string               { return "kiro" }
func (a *kiroAgent) Binary() string             { return "kiro-cli" }
func (a *kiroAgent) EntireAgent() string        { return "kiro" }
func (a *kiroAgent) PromptPattern() string      { return `❯` }
func (a *kiroAgent) TimeoutMultiplier() float64 { return 1.5 }

func (a *kiroAgent) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	combined := out.Stdout + out.Stderr
	transientPatterns := []string{
		"overloaded",
		"rate limit",
		"429",
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

func (a *kiroAgent) Bootstrap() error {
	return nil
}

func (a *kiroAgent) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	args := []string{"chat", "--no-interactive", "--agent", "entire", prompt}

	timeout := a.timeout
	if envTimeout := os.Getenv("E2E_TIMEOUT"); envTimeout != "" {
		if parsed, err := time.ParseDuration(envTimeout); err == nil {
			timeout = parsed
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.Binary(), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0")

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := Output{
		Command: a.Binary() + " " + strings.Join(args, " "),
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
		} else {
			out.ExitCode = -1
		}
		return out, err
	}

	return out, nil
}

func (a *kiroAgent) StartSession(ctx context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("kiro-test-%d", time.Now().UnixNano())
	s, err := NewTmuxSession(name, dir, nil, "env", "ENTIRE_TEST_TTY=0", a.Binary(), "chat", "--agent", "entire")
	if err != nil {
		return nil, err
	}

	if _, err := s.WaitFor(`❯`, 15*time.Second); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("waiting for startup: %w", err)
	}
	s.stableAtSend = ""
	return s, nil
}
