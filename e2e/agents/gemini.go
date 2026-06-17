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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "gemini-cli" {
		return
	}
	Register(&Gemini{})
	RegisterGate("gemini-cli", 2)
}

const geminiDefaultModel = "gemini-2.5-flash"
const geminiTrustWorkspaceEnvKey = "GEMINI_CLI_TRUST_WORKSPACE"
const geminiTrustWorkspaceEnv = geminiTrustWorkspaceEnvKey + "=true"

// geminiAbortSignatures are stderr markers for a server-side turn abort
// (empty/malformed model response). gemini-cli prints these to stderr but
// still exits 0 with empty stdout, so RunPrompt must surface them as an error
// for the transient-retry path (RepoState.RunPrompt -> IsTransientError) to
// fire. Without this, the turn never completes, the after-agent lifecycle hook
// never runs, no checkpoint is created, and tests fail with a misleading
// "checkpoint ref did not advance" instead of retrying.
var geminiAbortSignatures = []string{
	"Invalid stream",
	"empty response or malformed tool call",
}

func geminiAbortedTurn(stderr string) bool {
	for _, sig := range geminiAbortSignatures {
		if strings.Contains(stderr, sig) {
			return true
		}
	}
	return false
}

// geminiModel returns the model to use for e2e runs. E2E_GEMINI_MODEL overrides
// the default so CI can pin a more reliable model than the cheap local default
// (gemini-2.5-flash, which frequently aborts turns with "Invalid stream") without
// a code change. Mirrors E2E_CODEX_MODEL / E2E_OPENCODE_MODEL.
func geminiModel() string {
	if m := os.Getenv("E2E_GEMINI_MODEL"); m != "" {
		return m
	}
	return geminiDefaultModel
}

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
	if geminiAbortedTurn(out.Stderr) {
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
	return nil
}

func (g *Gemini) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: geminiModel()}
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
	cmd.Env = geminiPromptEnv(dir)
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
		// Wrap the prompt-level deadline so IsTransientError can detect it.
		// cmd.Run() returns "signal: killed", not the context error.
		if promptCtx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
		}
	}

	// gemini-cli can abort a turn server-side (empty/malformed model response)
	// yet still exit 0 with empty stdout. Surface it as an error so the
	// transient-retry path restarts the scenario instead of proceeding to fail
	// on a missing checkpoint. The real abort marker stays in stderr, which
	// IsTransientError matches via geminiAbortSignatures.
	if err == nil && geminiAbortedTurn(stderr.String()) {
		err = errors.New("gemini aborted turn: empty or malformed model response")
	}

	return Output{
		Command:  g.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (g *Gemini) StartSession(_ context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("gemini-test-%d", time.Now().UnixNano())

	envArgs := []string{"ACCESSIBLE=1", geminiTrustWorkspaceEnv, "HOME=" + geminiTestHomeDir(dir)}
	for _, key := range []string{"TERM"} {
		if v := os.Getenv(key); v != "" {
			envArgs = append(envArgs, key+"="+v)
		}
	}

	// Unset CI and GITHUB_ACTIONS so gemini doesn't force headless mode —
	// it checks both in isHeadlessMode() and skips interactive TUI entirely.
	args := append([]string{"env"}, envArgs...)
	args = append(args, g.Binary(), "--model", geminiModel(), "-y")
	s, err := NewTmuxSession(name, dir, []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY", "HOME"}, args[0], args[1:]...)
	if err != nil {
		return nil, err
	}

	// Dismiss startup dialogs (auth, workspace trust, etc.)
	for range 10 {
		content, err := s.WaitFor(`(Type your message|trust|Enter to select|Enter to confirm|New Agents Discovered|Acknowledge and Enable)`, 30*time.Second)
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

func geminiTestHomeDir(repoDir string) string {
	return filepath.Join(filepath.Dir(repoDir), filepath.Base(repoDir)+"-gemini-home")
}

func geminiPromptEnv(repoDir string) []string {
	return append(
		filterEnv(os.Environ(), "ENTIRE_TEST_TTY", geminiTrustWorkspaceEnvKey),
		"ACCESSIBLE=1",
		geminiTrustWorkspaceEnv,
		"HOME="+geminiTestHomeDir(repoDir),
	)
}
