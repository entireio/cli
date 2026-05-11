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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "pi" {
		return
	}
	Register(&Pi{})
	RegisterGate("pi", 2)
}

// Pi implements the E2E Agent interface for the Pi coding agent.
type Pi struct{}

// PiSession exposes the per-test isolated Pi home so test fixtures can
// inspect or clean it up.
type PiSession struct {
	*TmuxSession

	home string
}

func (s *PiSession) Home() string { return s.home }

func (p *Pi) Name() string               { return "pi" }
func (p *Pi) Binary() string             { return "pi" }
func (p *Pi) EntireAgent() string        { return "pi" }
func (p *Pi) PromptPattern() string      { return `\$\d` }
func (p *Pi) TimeoutMultiplier() float64 { return 1.5 }

func (p *Pi) Bootstrap() error {
	return nil
}

func (p *Pi) IsTransientError(out Output, _ error) bool {
	combined := out.Stdout + out.Stderr
	for _, pat := range []string{
		"overloaded",
		"rate limit",
		"429",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
		"timeout",
	} {
		if strings.Contains(combined, pat) {
			return true
		}
	}
	return false
}

// piHome creates an isolated PI_CODING_AGENT_DIR for a test run so
// parallel tests don't share session state with the real ~/.pi/agent.
// Auth (auth.json) and user-tunable defaults (settings.json) are
// symlinked from the real home if present so OAuth tokens flow through
// without re-prompting; tests that run with an explicit
// ANTHROPIC_API_KEY / OPENAI_API_KEY env will pick those up via the
// inherited environment regardless.
func piHome() (string, func(), error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", nil, fmt.Errorf("resolve user cache dir: %w", err)
	}
	base := filepath.Join(cache, "entire-e2e")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, fmt.Errorf("create pi home base %q: %w", base, err)
	}
	dir, err := os.MkdirTemp(base, "pi-home-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary pi home under %q: %w", base, err)
	}
	if err := seedPiHome(dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("seed pi home: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// seedPiHome links the user's auth.json + settings.json into the
// isolated home (best-effort) so Pi can authenticate without
// re-prompting. Sessions still write into the isolated home's
// sessions/ subdir, keeping per-test session state hermetic.
func seedPiHome(home string) error {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil //nolint:nilerr // no user home → can't seed; tests with API-key env still work
	}
	src := filepath.Join(realHome, ".pi", "agent")
	for _, name := range []string{"auth.json", "settings.json"} {
		from := filepath.Join(src, name)
		if _, statErr := os.Stat(from); statErr != nil {
			continue // file not present — skip
		}
		to := filepath.Join(home, name)
		if linkErr := os.Symlink(from, to); linkErr != nil {
			return fmt.Errorf("symlink %s: %w", name, linkErr)
		}
	}
	return nil
}

func (p *Pi) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	bin, err := exec.LookPath(p.Binary())
	if err != nil {
		return Output{}, fmt.Errorf("%s not in PATH: %w", p.Binary(), err)
	}

	home, cleanup, err := piHome()
	if err != nil {
		return Output{}, fmt.Errorf("create pi home: %w", err)
	}
	defer cleanup()

	args := []string{"-p", prompt, "--no-skills", "--no-prompt-templates", "--no-themes"}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt), "--no-skills", "--no-prompt-templates", "--no-themes"}

	env := append(filterEnv(os.Environ(), "ENTIRE_TEST_TTY", "PI_CODING_AGENT_DIR"),
		"PI_CODING_AGENT_DIR="+home,
	)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
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
	}

	return Output{
		Command:  p.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (p *Pi) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("pi-test-%d", time.Now().UnixNano())

	home, cleanup, err := piHome()
	if err != nil {
		return nil, fmt.Errorf("create pi home: %w", err)
	}

	s, err := p.startTmuxSession(name, dir, home, p.Binary())
	if err != nil {
		cleanup()
		return nil, err
	}
	s.OnClose(cleanup)

	if _, err := s.WaitFor(p.PromptPattern(), 30*time.Second); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("waiting for initial prompt: %w", err)
	}
	s.stableAtSend = ""

	return &PiSession{TmuxSession: s, home: home}, nil
}

// startTmuxSession spawns Pi inside tmux with PI_CODING_AGENT_DIR set
// via the `env` command. Mirrors Codex's startTmuxSession pattern so
// the per-test home propagates into the agent process.
func (p *Pi) startTmuxSession(name, dir, home string, args ...string) (*TmuxSession, error) {
	tmuxArgs := append([]string{
		"PI_CODING_AGENT_DIR=" + home,
		"HOME=" + os.Getenv("HOME"),
		"TERM=" + os.Getenv("TERM"),
	}, args...)
	return NewTmuxSession(name, dir, []string{"PI_CODING_AGENT_DIR", "ENTIRE_TEST_TTY"}, "env", tmuxArgs...)
}
