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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "cursor-cli" {
		return
	}
	Register(&CursorCLI{})
}

// CursorCLI implements the E2E Agent interface for the Cursor Agent CLI binary.
// The CLI binary is called "agent" and uses Cursor's hooks system via
// .cursor/hooks.json. It maps to the same Entire agent as Cursor IDE ("cursor").
//
// All E2E interactions use interactive (tmux) mode so that the full hook
// lifecycle fires (sessionStart, beforeSubmitPrompt, stop, sessionEnd).
// Headless (-p) mode skips beforeSubmitPrompt and stop hooks.
type CursorCLI struct{}

func (a *CursorCLI) Name() string               { return "cursor-cli" }
func (a *CursorCLI) Binary() string             { return "agent" }
func (a *CursorCLI) EntireAgent() string        { return "cursor" }
func (a *CursorCLI) TimeoutMultiplier() float64 { return 1.5 }

// PromptPattern returns a regex matching Cursor's ready-state prompt markers.
// Cursor has used multiple startup/completion markers across releases.
func (a *CursorCLI) PromptPattern() string {
	return `(/ commands|Plan, search, build anything|Add a follow-up)`
}

func (a *CursorCLI) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	combined := out.Stdout + out.Stderr
	for _, p := range []string{
		"overloaded",
		"rate limit",
		"429",
		"503",
		"529",
		"ECONNRESET",
		"ETIMEDOUT",
		"server error",
		"Internal Server Error",
	} {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

func (a *CursorCLI) Bootstrap() error {
	// The Cursor CLI authenticates via CURSOR_API_KEY env var or OAuth.
	// On CI, ensure CURSOR_API_KEY is set. Locally, OAuth/keychain works.
	if os.Getenv("CI") != "" && os.Getenv("CURSOR_API_KEY") == "" {
		return errors.New("CURSOR_API_KEY must be set on CI for cursor-cli E2E tests")
	}
	return nil
}

// cursorConfigDir creates an isolated per-session config directory for the
// Cursor CLI. Each session gets its own XDG_CONFIG_HOME so that parallel tests
// don't race on the shared ~/.config/cursor/cli-config.json file.
// The directory is pre-seeded with an empty cli-config.json to avoid the
// atomic temp-file rename that triggers the ENOENT race.
func cursorConfigDir() (string, error) {
	tmp, err := os.MkdirTemp("", "cursor-e2e-config-*")
	if err != nil {
		return "", fmt.Errorf("create temp config dir: %w", err)
	}
	cursorDir := filepath.Join(tmp, "cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cursor config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cursorDir, "cli-config.json"), []byte("{}"), 0o644); err != nil {
		return "", fmt.Errorf("seed cli-config.json: %w", err)
	}
	return tmp, nil
}

func (a *CursorCLI) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	timeout := 90 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}

	displayCmd := a.Binary() + " --force --workspace " + dir + " (interactive prompt: " + prompt + ")"

	// Start an interactive tmux session so all hooks fire
	// (beforeSubmitPrompt and stop don't fire in headless -p mode).
	s, err := a.startInteractiveSession(dir)
	if err != nil {
		return Output{Command: displayCmd, ExitCode: -1},
			fmt.Errorf("start interactive session: %w", err)
	}
	defer s.Close()

	// Wait for trust dialog and accept it.
	if err := a.acceptTrustDialogIfNeeded(s); err != nil {
		return Output{Command: displayCmd, Stdout: s.Capture(), ExitCode: -1}, err
	}

	// Wait for the TUI to be ready.
	if _, err := s.WaitFor(a.PromptPattern(), 45*time.Second); err != nil {
		return Output{Command: displayCmd, Stdout: s.Capture(), ExitCode: -1},
			fmt.Errorf("waiting for startup prompt: %w", err)
	}

	// Send the prompt.
	if err := s.Send(prompt); err != nil {
		return Output{Command: displayCmd, Stdout: s.Capture(), ExitCode: -1},
			fmt.Errorf("sending prompt: %w", err)
	}

	// One absolute deadline covers both the "Add a follow-up" wait and the
	// subsequent busy-hint wait, so the whole RunPrompt call honors the
	// caller-provided timeout (rather than ~2× it across two stages).
	deadline := time.Now().Add(timeout)

	// Wait for the "Add a follow-up" input placeholder. This alone isn't
	// sufficient — Cursor renders the placeholder while the agent is still
	// editing, alongside a busy-state hint "ctrl+c to stop" on the same line
	// (and the "Editing N tokens" spinner above). If we return now, the
	// caller's defer s.Close() kills tmux mid-turn, the cursor process exits
	// before its `stop` hook fires, SaveStep never runs, and the resulting
	// checkpoint has empty FilesTouched.
	content, waitErr := s.WaitFor(`Add a follow-up`, time.Until(deadline))
	if waitErr != nil {
		// Check for deadline exceeded to allow transient error detection.
		if ctx.Err() == context.DeadlineExceeded {
			waitErr = fmt.Errorf("%w: %w", waitErr, context.DeadlineExceeded)
		}
		return Output{Command: displayCmd, Stdout: content, ExitCode: -1}, waitErr
	}

	// Then wait for the busy hint to disappear, confirming the agent has
	// finished its turn so the `stop` hook can fire before tmux is torn down.
	for {
		if !strings.Contains(content, "ctrl+c to stop") {
			return Output{Command: displayCmd, Stdout: content, ExitCode: 0}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			err := fmt.Errorf("cursor agent did not finish turn (ctrl+c to stop still visible) within %s", timeout)
			if ctx.Err() == context.DeadlineExceeded {
				err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
			}
			return Output{Command: displayCmd, Stdout: content, ExitCode: -1}, err
		}
		sleep := 200 * time.Millisecond
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return Output{Command: displayCmd, Stdout: content, ExitCode: -1},
				fmt.Errorf("waiting for cursor agent to finish turn: %w", ctx.Err())
		case <-time.After(sleep):
		}
		content = s.Capture()
	}
}

func (a *CursorCLI) StartSession(ctx context.Context, dir string) (Session, error) {
	s, err := a.startInteractiveSession(dir)
	if err != nil {
		return nil, err
	}

	if err := a.acceptTrustDialogIfNeeded(s); err != nil {
		_ = s.Close()
		return nil, err
	}

	// Wait for the TUI to be ready (input prompt).
	if _, err := s.WaitFor(a.PromptPattern(), 45*time.Second); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("waiting for startup prompt: %w", err)
	}
	s.stableAtSend = ""

	return s, nil
}

// startInteractiveSession creates a new tmux session running the Cursor CLI
// in interactive mode (no -p flag) so all hooks fire.
func (a *CursorCLI) startInteractiveSession(dir string) (*TmuxSession, error) {
	// Resolve to absolute path so tmux can find the binary even if its
	// shell doesn't inherit the test process's PATH (common on CI).
	bin, err := exec.LookPath(a.Binary())
	if err != nil {
		return nil, fmt.Errorf("agent binary not found: %w", err)
	}

	// Create an isolated config directory for this session so parallel tests
	// don't race on the shared ~/.config/cursor/cli-config.json file.
	configDir, err := cursorConfigDir()
	if err != nil {
		return nil, fmt.Errorf("create cursor config dir: %w", err)
	}

	// Build env-wrapped command so the tmux session inherits critical env vars.
	// tmux starts a new shell that doesn't inherit Go's os.Environ().
	var envArgs []string
	for _, key := range []string{"CURSOR_API_KEY", "PATH", "HOME", "TERM"} {
		if v := os.Getenv(key); v != "" {
			envArgs = append(envArgs, key+"="+v)
		}
	}
	// Point XDG_CONFIG_HOME to our isolated dir so cursor writes its config
	// there instead of ~/.config/cursor/. We keep the real HOME so that
	// auth credentials (keychain, OAuth tokens) remain accessible.
	envArgs = append(envArgs, "XDG_CONFIG_HOME="+configDir)

	args := append([]string{"env"}, envArgs...)
	args = append(args, bin, "--force", "--workspace", dir)

	name := fmt.Sprintf("cursor-cli-test-%d", time.Now().UnixNano())
	unset := []string{"CI"}
	s, err := NewTmuxSession(name, dir, unset, args[0], args[1:]...)
	if err != nil {
		_ = os.RemoveAll(configDir)
		return nil, err
	}
	s.OnClose(func() { _ = os.RemoveAll(configDir) })
	return s, nil
}

// acceptTrustDialogIfNeeded checks whether the workspace trust dialog appears
// and presses "a" to accept it. The dialog only shows on the first launch in
// a workspace — subsequent sessions in the same directory skip it.
func (a *CursorCLI) acceptTrustDialogIfNeeded(s *TmuxSession) error {
	// Race: either the trust dialog or the input prompt will appear first.
	// Use a short timeout to check for the trust dialog without blocking
	// too long if the workspace is already trusted.
	content, err := s.WaitFor(`Trust this workspace|`+a.PromptPattern(), 45*time.Second)
	if err != nil {
		return fmt.Errorf("waiting for trust dialog or prompt: %w", err)
	}
	if strings.Contains(content, "Trust this workspace") {
		if err := s.SendKeys("a"); err != nil {
			return fmt.Errorf("accepting trust dialog: %w", err)
		}
	}
	return nil
}
