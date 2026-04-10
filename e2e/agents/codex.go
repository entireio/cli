package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "codex" {
		return
	}
	Register(&Codex{})
	RegisterGate("codex", 2)
}

// Codex implements the E2E Agent interface for OpenAI's Codex CLI.
type Codex struct{}

type CodexSession struct {
	*TmuxSession

	home string
}

func (s *CodexSession) Home() string { return s.home }

func (c *Codex) Name() string               { return "codex" }
func (c *Codex) Binary() string             { return "codex" }
func (c *Codex) EntireAgent() string        { return "codex" }
func (c *Codex) PromptPattern() string      { return `›` }
func (c *Codex) TimeoutMultiplier() float64 { return 1.5 }

func (c *Codex) Bootstrap() error {
	if os.Getenv("CI") != "" && os.Getenv("OPENAI_API_KEY") == "" {
		return errors.New("OPENAI_API_KEY must be set on CI for Codex E2E tests")
	}
	return nil
}

func (c *Codex) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	combined := out.Stdout + out.Stderr
	for _, p := range []string{"overloaded", "rate limit", "rate_limit", "503", "529", "ECONNRESET", "ETIMEDOUT"} {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

// codexHome creates an isolated CODEX_HOME for a test run.
// Auth still works via OPENAI_API_KEY env var or symlinked auth.json.
func codexHome() (string, func(), error) {
	dir, err := os.MkdirTemp("", "codex-home-*")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func (c *Codex) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	timeout := 60 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	home, cleanup, err := codexHome()
	if err != nil {
		return Output{}, fmt.Errorf("create codex home: %w", err)
	}
	defer cleanup()

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	if err := seedCodexHome(home, absDir); err != nil {
		return Output{}, fmt.Errorf("seed codex home: %w", err)
	}

	args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}
	if cfg.Model != "" {
		args = append(args, "-m", cfg.Model)
	}
	args = append(args, prompt)

	env := append(filterEnv(os.Environ(), "ENTIRE_TEST_TTY", "CODEX_HOME"),
		"CODEX_HOME="+home,
	)

	cmd := exec.CommandContext(promptCtx, c.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = env
	setupProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
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
		Command:  c.Binary() + " " + strings.Join(args[:len(args)-1], " ") + " " + fmt.Sprintf("%q", prompt),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (c *Codex) StartSession(ctx context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("codex-test-%d", time.Now().UnixNano())

	home, cleanup, err := codexHome()
	if err != nil {
		return nil, fmt.Errorf("create codex home: %w", err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	if err := seedCodexHome(home, absDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("seed codex home: %w", err)
	}

	s, err := c.startTmuxSession(name, dir, home, "codex", "--dangerously-bypass-approvals-and-sandbox")
	if err != nil {
		cleanup()
		return nil, err
	}
	s.OnClose(cleanup)

	// Dismiss startup dialogs (model upgrade prompts, etc.) until we reach
	// the input prompt. Similar to Claude's startup dialog handling.
	for range 5 {
		content, waitErr := s.WaitFor(c.PromptPattern(), 15*time.Second)
		if waitErr != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for codex prompt: %w", waitErr)
		}
		if !strings.Contains(content, "press enter to confirm") &&
			!strings.Contains(content, "Use ↑/↓ to move") {
			break
		}
		_ = s.SendKeys("Enter")
		time.Sleep(500 * time.Millisecond)
	}

	return &CodexSession{TmuxSession: s, home: home}, nil
}

func (c *Codex) ResumeSession(ctx context.Context, dir, home, sessionID string) (Session, error) {
	_ = ctx
	name := fmt.Sprintf("codex-resume-%d", time.Now().UnixNano())

	s, err := c.startTmuxSession(name, dir, home, "codex", "--dangerously-bypass-approvals-and-sandbox", "resume", sessionID)
	if err != nil {
		return nil, err
	}

	// Dismiss any startup prompts until the input prompt appears.
	for range 5 {
		content, waitErr := s.WaitFor(c.PromptPattern(), 15*time.Second)
		if waitErr != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for codex resumed prompt: %w", waitErr)
		}
		if !strings.Contains(content, "press enter to confirm") &&
			!strings.Contains(content, "Use ↑/↓ to move") {
			break
		}
		_ = s.SendKeys("Enter")
		time.Sleep(500 * time.Millisecond)
	}

	return &CodexSession{TmuxSession: s, home: home}, nil
}

func (c *Codex) startTmuxSession(name, dir, home string, args ...string) (*TmuxSession, error) {
	tmuxArgs := append([]string{"CODEX_HOME=" + home, "HOME=" + os.Getenv("HOME"), "TERM=" + os.Getenv("TERM")}, args...)
	return NewTmuxSession(name, dir, []string{"CODEX_HOME", "ENTIRE_TEST_TTY"}, "env", tmuxArgs...)
}

// seedCodexHome writes trust + feature flag config and links auth credentials
// so Codex loads the project's .codex/ layer and can authenticate.
func seedCodexHome(home, projectDir string) error {
	if err := os.MkdirAll(home, 0o750); err != nil {
		return err
	}

	// Write config with trust, feature flag, and pinned model to skip upgrade dialogs.
	model := os.Getenv("E2E_CODEX_MODEL")
	if model == "" {
		model = "gpt-5.4"
	}
	config := fmt.Sprintf("model = %q\n\n[features]\ncodex_hooks = true\n\n[projects.%q]\ntrust_level = \"trusted\"\n", model, projectDir)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		return err
	}

	if apiKey := os.Getenv("OPENAI_API_KEY"); apiKey != "" {
		return writeCodexAPIKeyAuth(home, apiKey)
	}

	// Symlink auth.json from the real ~/.codex/ so API credentials are available.
	if realHome, err := os.UserHomeDir(); err == nil {
		src := filepath.Join(realHome, ".codex", "auth.json")
		if _, err := os.Stat(src); err == nil {
			_ = os.Symlink(src, filepath.Join(home, "auth.json"))
		}
	}

	return nil
}

func writeCodexAPIKeyAuth(home, apiKey string) error {
	auth := struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	}{
		AuthMode:     "apikey",
		OpenAIAPIKey: apiKey,
	}

	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal codex auth: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		return fmt.Errorf("write codex auth: %w", err)
	}

	return nil
}
