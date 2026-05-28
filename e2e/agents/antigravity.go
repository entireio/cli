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

	// agy (as of 1.0.2) has no --model flag — model selection happens via
	// settings.json (selectedModel) or the in-product picker. Passing a
	// `--model <name>` arg made agy exit 2 with the flag-not-defined error.
	// We accept the agy default here; tests that need a specific model
	// should seed settings.json instead.
	_ = cfg.Model
	// agy -p ignores cwd: without --add-dir it runs in
	// ~/.gemini/antigravity-cli/scratch/ instead of the test repo
	// (observed in PR #1287 validation — agy initialized a brand-new git
	// repo in scratch and committed the requested file there). --add-dir
	// pins agy to the workspace we actually want it to modify.
	args := []string{"-p", prompt, "--dangerously-skip-permissions", "--add-dir", dir}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt), "--dangerously-skip-permissions", "--add-dir", dir}

	cmd := exec.CommandContext(promptCtx, a.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = antigravityPromptEnv()
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

	envArgs := []string{"ACCESSIBLE=1"}
	for _, key := range []string{"TERM"} {
		if v := os.Getenv(key); v != "" {
			envArgs = append(envArgs, key+"="+v)
		}
	}

	args := append([]string{"env"}, envArgs...)
	args = append(args, a.Binary(), "--dangerously-skip-permissions")
	s, err := NewTmuxSession(name, dir, []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}, args[0], args[1:]...)
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

// antigravityPromptEnv returns the env for spawning agy in print mode.
//
// Local-validation NOTE: HOME isolation was removed because agy stores its
// OAuth, installation id, and onboarding state under HOME/.gemini/, and
// pointing HOME at a fresh dir (even with selective symlinks) made agy
// re-trigger the browser auth flow. Sharing the user's real HOME lets agy
// authenticate; the test repo (cmd.Dir) still provides workspace isolation
// for files agy creates. Side effect: test conversations and brain state
// land in the user's real ~/.gemini/antigravity-cli/ and need manual
// cleanup. The harness should grow a proper HOME-isolation mechanism (e.g.
// importing the auth/state surface from the real home) before this is run
// in CI — tracked as PR review feedback on #1287.
func antigravityPromptEnv() []string {
	return append(filterEnv(os.Environ(), "ENTIRE_TEST_TTY"), "ACCESSIBLE=1")
}
