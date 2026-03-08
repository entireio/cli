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

type openCodeAgent struct {
	model   string
	timeout time.Duration
}

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "opencode" {
		return
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		return
	}
	model := os.Getenv("E2E_OPENCODE_MODEL")
	if model == "" {
		model = "anthropic/claude-haiku-4-5"
	}
	Register(&openCodeAgent{model: model, timeout: 2 * time.Minute})
}

func (a *openCodeAgent) Name() string               { return "opencode" }
func (a *openCodeAgent) Binary() string             { return "opencode" }
func (a *openCodeAgent) EntireAgent() string        { return "opencode" }
func (a *openCodeAgent) PromptPattern() string      { return `(Ask anything|▣)` }
func (a *openCodeAgent) TimeoutMultiplier() float64 { return 2.0 }

func (a *openCodeAgent) IsTransientError(out Output, _ error) bool {
	transientPatterns := []string{
		"overloaded",
		"rate limit",
		"529",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
		"Token refresh failed",
		"database is locked",
	}
	for _, p := range transientPatterns {
		if strings.Contains(out.Stderr, p) {
			return true
		}
	}
	return false
}

func (a *openCodeAgent) Bootstrap() error {
	// opencode has first-run DB migration + node_modules resolution that
	// races with parallel test execution (upstream issue #6935).
	// Run a trivial prompt to force full initialization before tests.
	for i := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, a.Binary(), "run", "--model", a.model, "say hi")
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return nil
		}
		if i < 2 {
			fmt.Fprintf(os.Stderr, "opencode warmup attempt %d failed: %s\n%s\n", i+1, err, out)
			time.Sleep(5 * time.Second)
		}
	}
	// Non-fatal: warmup failure shouldn't block tests entirely.
	fmt.Fprintln(os.Stderr, "opencode warmup failed after 3 attempts, proceeding anyway")
	return nil
}

func (a *openCodeAgent) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	model := a.model
	if cfg.Model != "" {
		model = cfg.Model
	}

	args := []string{"run"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

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
	cmd.Env = filterEnv(os.Environ(), "ENTIRE_TEST_TTY")

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

func (a *openCodeAgent) StartSession(ctx context.Context, dir string) (Session, error) {
	// opencode's TUI occasionally fails to render on CI (empty pane).
	// Retry once if the first attempt produces no output at all.
	var s *TmuxSession
	var lastErr error
	for attempt := range 2 {
		name := fmt.Sprintf("opencode-test-%d", time.Now().UnixNano())
		var err error
		s, err = NewTmuxSession(name, dir, []string{"ENTIRE_TEST_TTY"}, a.Binary(), "--model", a.model)
		if err != nil {
			return nil, err
		}

		// Wait for TUI to be ready (input area with placeholder text).
		if _, err := s.WaitFor(`Ask anything`, 15*time.Second); err != nil {
			content := s.Capture()
			_ = s.Close()
			if strings.TrimSpace(content) == "" && attempt == 0 {
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("waiting for startup: %w", err)
		}
		s.stableAtSend = ""
		return s, nil
	}
	return nil, fmt.Errorf("opencode TUI failed to start after retry: %w", lastErr)
}
