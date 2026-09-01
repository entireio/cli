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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "grok" {
		return
	}
	Register(&Grok{})
	RegisterGate("grok", 2)
}

// Grok implements the E2E Agent interface for xAI's Grok Build CLI.
//
// See cmd/entire/cli/agent/grok/AGENT.md for the hook/transcript contract this
// runner is built against.
type Grok struct{}

// GrokSession carries the isolated GROK_HOME alongside the tmux pane so tests
// that need to inspect the session transcript can locate it.
type GrokSession struct {
	*TmuxSession

	home string
}

func (s *GrokSession) Home() string { return s.home }

func (g *Grok) Name() string        { return "grok" }
func (g *Grok) Binary() string      { return "grok" }
func (g *Grok) EntireAgent() string { return "grok" }

// PromptPattern is the TUI input caret (U+276F), observed in Grok 1.0.5.
func (g *Grok) PromptPattern() string { return `❯` }

func (g *Grok) TimeoutMultiplier() float64 { return 1.5 }

func (g *Grok) Bootstrap() error {
	if os.Getenv("CI") != "" && os.Getenv("XAI_API_KEY") == "" {
		return errors.New("XAI_API_KEY must be set on CI for Grok E2E tests")
	}
	return nil
}

// quotaExhaustedMarkers identify an account that is out of Grok Build quota.
//
// These are deliberately NOT transient: the balance does not refill within a
// test run, so retrying burns the remaining wall-clock budget and still fails.
// Keeping them separate is what makes the suite report "out of quota" instead
// of three identical timeouts per test.
var quotaExhaustedMarkers = []string{
	"usage limit",
	"SuperGrok",
}

// IsTransientError matches Grok's own StopFailure error taxonomy
// (rate_limit, server_error, ...) plus the usual network failures.
//
// resource-exhausted / "Too many requests" is per-team throttling rather than
// an exhausted balance, so it IS worth retrying — unlike the quota markers
// above, which are checked first and always lose.
func (g *Grok) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	combined := out.Stdout + out.Stderr
	for _, p := range quotaExhaustedMarkers {
		if strings.Contains(combined, p) {
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	for _, p := range []string{
		"overloaded", "rate limit", "rate_limit", "server_error",
		"resource-exhausted", "Too many requests",
		"503", "529", "ECONNRESET", "ETIMEDOUT",
	} {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

// grokHome creates an isolated GROK_HOME for a test run so sessions, trust
// decisions, and config never touch the developer's real ~/.grok.
//
// It lives under the user cache dir rather than the system temp dir for the
// same reason Codex's does: agent CLIs that install helper binaries tend to
// refuse to do so from /tmp.
func grokHome() (string, func(), error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", nil, fmt.Errorf("resolve user cache dir: %w", err)
	}
	base := filepath.Join(cache, "entire-e2e")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("create grok home base %q: %w", base, err)
	}
	dir, err := os.MkdirTemp(base, "grok-home-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary grok home under %q: %w", base, err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// seedGrokHome writes the config, trust, and auth an isolated GROK_HOME needs
// before Grok will run project hooks in projectDir.
//
// The [compat.*] block is load-bearing, not hygiene. Grok scans
// ~/.claude/settings.json and ~/.cursor/hooks.json for hooks by default, and
// those live under the real $HOME, which GROK_HOME does not isolate. On a
// developer machine with Entire installed globally that means every E2E turn
// would additionally fire `entire hooks claude-code ...` against Grok's
// payloads — polluting the run with a second, misattributed agent. Turning the
// vendor scans off is what keeps a Grok E2E test about Grok.
func seedGrokHome(home, projectDir string) error {
	if err := os.MkdirAll(home, 0o750); err != nil {
		return err
	}

	var cfg strings.Builder
	if model := os.Getenv("E2E_GROK_MODEL"); model != "" {
		fmt.Fprintf(&cfg, "model = %q\n\n", model)
	}
	cfg.WriteString("[features]\ntelemetry = false\n\n")
	cfg.WriteString("[compat.claude]\nhooks = false\n\n")
	cfg.WriteString("[compat.cursor]\nhooks = false\n")

	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg.String()), 0o600); err != nil {
		return fmt.Errorf("write grok config: %w", err)
	}

	// Project hooks under <project>/.grok/hooks/ are silently skipped until
	// the folder is trusted, so pre-trust it. --trust is also passed on the
	// command line; writing the store covers both entry points deterministically.
	trust := fmt.Sprintf("[folders.%q]\ntrusted = true\ndecided_at = %d\n", projectDir, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(home, "trusted_folders.toml"), []byte(trust), 0o600); err != nil {
		return fmt.Errorf("write grok trust store: %w", err)
	}

	// XAI_API_KEY is read from the environment and needs no file. Otherwise
	// borrow the developer's existing login.
	if os.Getenv("XAI_API_KEY") != "" {
		return nil
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil //nolint:nilerr // no home dir just means no credentials to borrow
	}
	src := filepath.Join(realHome, ".grok", "auth.json")
	if _, err := os.Stat(src); err == nil {
		_ = os.Symlink(src, filepath.Join(home, "auth.json"))
	}
	return nil
}

func (g *Grok) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
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

	home, cleanup, err := grokHome()
	if err != nil {
		return Output{}, fmt.Errorf("create grok home: %w", err)
	}
	defer cleanup()

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	if err := seedGrokHome(home, absDir); err != nil {
		return Output{}, fmt.Errorf("seed grok home: %w", err)
	}

	// Grok 1.0.5 has no --yolo; bypassPermissions is the documented equivalent.
	args := []string{"--trust", "--permission-mode", "bypassPermissions"}
	if cfg.Model != "" {
		args = append(args, "-m", cfg.Model)
	}
	args = append(args, "-p", prompt)

	env := append(filterEnv(os.Environ(), "ENTIRE_TEST_TTY", "GROK_HOME"),
		"GROK_HOME="+home,
	)

	cmd := exec.CommandContext(promptCtx, g.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = env
	setupProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode, err := runCapture(cmd, promptCtx)

	return Output{
		Command:  g.Binary() + " " + strings.Join(args[:len(args)-1], " ") + " " + fmt.Sprintf("%q", prompt),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (g *Grok) StartSession(ctx context.Context, dir string) (Session, error) {
	_ = ctx
	name := fmt.Sprintf("grok-test-%d", time.Now().UnixNano())

	home, cleanup, err := grokHome()
	if err != nil {
		return nil, fmt.Errorf("create grok home: %w", err)
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	if err := seedGrokHome(home, absDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("seed grok home: %w", err)
	}

	tmuxArgs := []string{
		"GROK_HOME=" + home,
		"HOME=" + os.Getenv("HOME"),
		"TERM=" + os.Getenv("TERM"),
		g.Binary(), "--trust", "--permission-mode", "bypassPermissions",
	}
	s, err := NewTmuxSession(name, dir, []string{"GROK_HOME", "ENTIRE_TEST_TTY"}, "env", tmuxArgs...)
	if err != nil {
		cleanup()
		return nil, err
	}
	s.OnClose(cleanup)

	// Grok's first run shows a coding-data consent banner ("Help improve Grok",
	// [Opt out] / [Opt in]) over the input caret. [features] telemetry = false
	// does not cover it — that switch is product analytics, and this banner is
	// the separate /privacy setting. Dismiss it, then wait for a clean prompt.
	for range 5 {
		content, waitErr := s.WaitFor(g.PromptPattern(), 20*time.Second)
		if waitErr != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for grok prompt: %w", waitErr)
		}
		if !strings.Contains(content, "Help improve Grok") &&
			!strings.Contains(content, "Opt out") {
			break
		}
		_ = s.SendKeys("Escape")
		time.Sleep(500 * time.Millisecond)
	}

	return &GrokSession{TmuxSession: s, home: home}, nil
}
