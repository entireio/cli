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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "factoryai-droid" {
		return
	}
	if _, err := exec.LookPath("droid"); err != nil {
		return
	}
	Register(&Droid{})
	// factoryai-droid is run serially in CI (E2E_CONCURRENT_TEST_LIMIT=1).
	// Without a registered gate, AcquireSlot is a no-op and that limit has no
	// effect; register the gate so the intended serialization is respected.
	RegisterGate("factoryai-droid", 1)
}

// Droid implements the Agent interface for Factory AI Droid.
type Droid struct{}

func (d *Droid) Name() string               { return "factoryai-droid" }
func (d *Droid) Binary() string             { return "droid" }
func (d *Droid) EntireAgent() string        { return "factoryai-droid" }
func (d *Droid) PromptPattern() string      { return `>` }
func (d *Droid) TimeoutMultiplier() float64 { return 2.0 }

func (d *Droid) IsTransientError(out Output, err error) bool {
	if err == nil {
		return false
	}
	combined := out.Stdout + out.Stderr
	transientPatterns := []string{
		"overloaded",
		"rate limit",
		"529",
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

const defaultDroidModel = "claude-haiku-4-5-20251001"

// DefaultDroidModel returns the Factory-managed model used by Droid tests.
func DefaultDroidModel() string { return defaultDroidModel }

// Bootstrap needs no model configuration: Factory authenticates with FACTORY_API_KEY.
func (d *Droid) Bootstrap() error { return nil }

func (d *Droid) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: defaultDroidModel}
	for _, o := range opts {
		o(cfg)
	}

	model := cfg.Model
	if model == "" {
		model = defaultDroidModel
	}

	ctx, cancel, err := boundPrompt(ctx, 0, cfg)
	if err != nil {
		return Output{}, err
	}
	defer cancel()

	args := []string{"exec", "--skip-permissions-unsafe", "--model", model, prompt}
	displayArgs := []string{"exec", "--skip-permissions-unsafe", "--model", model, fmt.Sprintf("%q", prompt)}

	cmd := exec.CommandContext(ctx, d.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = filterEnv(os.Environ(), "ENTIRE_TEST_TTY", "CI", "GITHUB_ACTIONS")
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
		Command:  d.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, err
}

func (d *Droid) StartSession(ctx context.Context, dir string) (Session, error) {
	name := fmt.Sprintf("droid-test-%d", time.Now().UnixNano())
	// Unset CI and GITHUB_ACTIONS so Droid doesn't enter single-turn/headless
	// mode — it checks these vars and skips interactive input after the first turn.
	s, err := NewTmuxSession(name, dir, []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}, d.Binary())
	if err != nil {
		return nil, err
	}

	// Dismiss startup dialogs (folder trust, etc.) then wait for the input
	// prompt. Droid v0.178.0 added a "Trust this folder?" dialog in interactive
	// mode for untrusted directories. Its "1. Trust this folder" option is
	// pre-selected, so Enter confirms it. The dialog renders its selected option
	// as "> 1. Trust this folder", so a bare ">" match cannot distinguish the
	// dialog from the real input box — we must key off the dialog chrome (see
	// isDroidStartupDialog) before treating ">" as ready.
	foundPrompt := false
	for range 5 {
		content, err := s.WaitFor(`>`, 30*time.Second)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for startup prompt: %w", err)
		}
		if !isDroidStartupDialog(content) {
			foundPrompt = true
			break
		}
		if err := s.SendKeys("Enter"); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("dismissing startup dialog: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !foundPrompt {
		_ = s.Close()
		return nil, errors.New("droid did not reach interactive prompt after dismissing startup dialogs")
	}

	// Droid auto-generates a greeting on startup which fires a Stop hook.
	// Wait for the greeting turn to fully complete before accepting prompts.
	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil, fmt.Errorf("context cancelled during startup wait: %w", ctx.Err())
	case <-time.After(2 * time.Second):
	}
	s.stableAtSend = ""

	return s, nil
}

// isDroidStartupDialog reports whether the captured pane is showing a Droid
// startup dialog (currently the "Trust this folder?" prompt) rather than the
// interactive input box. The dialog renders its pre-selected option as
// "> 1. Trust this folder", so the presence of ">" alone cannot distinguish it
// from the real prompt — we key off the dialog title ("trust this folder") and
// its "exit without trusting" option label instead.
// Matching is case-insensitive to stay resilient to Droid re-casing its copy.
func isDroidStartupDialog(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "trust this folder") ||
		strings.Contains(lower, "exit without trusting")
}
