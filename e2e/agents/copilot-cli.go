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
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "copilot-cli" {
		return
	}
	Register(&CopilotCLI{})
	RegisterGate("copilot-cli", 4)
}

type CopilotCLI struct{}

func (c *CopilotCLI) Name() string               { return "copilot-cli" }
func (c *CopilotCLI) Binary() string             { return "copilot" }
func (c *CopilotCLI) EntireAgent() string        { return "copilot-cli" }
func (c *CopilotCLI) PromptPattern() string      { return `❯` }
func (c *CopilotCLI) TimeoutMultiplier() float64 { return 1.5 }

func (c *CopilotCLI) IsTransientError(out Output, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	combined := out.Stdout + out.Stderr
	for _, p := range []string{
		"overloaded",
		"rate limit",
		"503",
		"529",
		"ECONNRESET",
		"ETIMEDOUT",
		"Too Many Requests",
		// gpt-4.1 sometimes calls Copilot's Edit tool without old_str,
		// resulting in zero code changes despite a successful exit.
		"old_str is required",
	} {
		if strings.Contains(combined, p) {
			return true
		}
	}
	return false
}

func (c *CopilotCLI) Bootstrap() error {
	// Copilot CLI uses GitHub authentication (gh auth or GITHUB_TOKEN).
	// No additional bootstrap needed — auth should be pre-configured.
	return nil
}

func (c *CopilotCLI) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{Model: "gpt-4.1"}
	for _, o := range opts {
		o(cfg)
	}

	timeout := 60 * time.Second
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	promptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", prompt, "--model", cfg.Model, "--allow-all"}
	displayArgs := []string{"-p", fmt.Sprintf("%q", prompt), "--model", cfg.Model, "--allow-all"}
	cmd := exec.CommandContext(promptCtx, c.Binary(), args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=0")
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

	out := Output{
		Command:  c.Binary() + " " + strings.Join(displayArgs, " "),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}

	// gpt-4.1 sometimes calls Copilot's Edit tool without required parameters,
	// producing zero code changes despite exit 0. Surface this as an error so
	// the transient-error retry mechanism can restart the scenario.
	// Only trigger when copilot reports zero changes — it may retry internally.
	if err == nil && strings.Contains(out.Stdout, "old_str is required") &&
		strings.Contains(out.Stderr, "Total code changes:     +0 -0") {
		err = errors.New("copilot Edit tool failed: old_str is required")
	}

	return out, err
}

// CopilotSession wraps TmuxSession to handle Copilot CLI's dual input modes.
// Copilot CLI can be in "Chat" mode (Enter submits) or "Edit" mode (Ctrl+S
// submits). After completing a prompt, copilot may return to either mode
// non-deterministically. This wrapper detects Edit mode after sending Enter
// and falls back to Ctrl+S if needed.
type CopilotSession struct {
	*TmuxSession
}

func (cs *CopilotSession) Send(input string) error {
	if err := cs.clearInputLine(); err != nil {
		return err
	}

	preSend := stableContent(cs.Capture())

	if err := cs.sendOnce(input, preSend); err != nil {
		return err
	}

	// Copilot CLI's autocomplete can non-deterministically trigger during
	// text input (e.g. a "/" in "docs/red.md" opens the slash-command menu).
	// If detected, dismiss with Escape, clear the input, and retry once.
	time.Sleep(300 * time.Millisecond)
	if isAutocompleteMenu(cs.Capture()) {
		if err := cs.dismissAutocompleteAndClear(); err != nil {
			return err
		}
		if err := cs.sendOnce(input, stableContent(cs.Capture())); err != nil {
			return err
		}
	}

	// Copilot can leave behind a stray slash-command invocation between turns.
	// Only retry if that error appeared in output produced by this submission,
	// not merely somewhere in pane scrollback from an earlier turn.
	if content := cs.Capture(); appearedInNewOutput(preSend, content, "Unknown command: /") {
		if err := cs.clearInputLine(); err != nil {
			return err
		}
		if err := cs.sendOnce(input, stableContent(cs.Capture())); err != nil {
			return err
		}
	}

	// Wait for the terminal to reflect the echoed input, then snapshot.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		current := stableContent(cs.Capture())
		if current != preSend {
			cs.stableAtSend = current
			return nil
		}
	}
	cs.stableAtSend = stableContent(cs.Capture())
	return nil
}

// clearInputLine removes any partially typed prompt text without sending other
// UI navigation keys. In Copilot CLI, Escape can trigger undo/snapshot actions,
// so routine pre-send cleanup must avoid it.
func (cs *CopilotSession) clearInputLine() error {
	if err := cs.SendKeys("C-u"); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	return nil
}

// dismissAutocompleteAndClear closes the slash-command autocomplete dropdown
// and then clears the input line before retrying the prompt.
func (cs *CopilotSession) dismissAutocompleteAndClear() error {
	if err := cs.SendKeys("Escape"); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	return cs.clearInputLine()
}

// appearedInNewOutput reports whether pattern is present in content that was
// added after the pre-send snapshot. This avoids retriggering on old pane
// scrollback from previous turns.
func appearedInNewOutput(before string, after string, pattern string) bool {
	if after == before {
		return false
	}
	if strings.HasPrefix(after, before) {
		return strings.Contains(after[len(before):], pattern)
	}

	// If tmux capture lost a clean prefix relationship due to scrollback churn,
	// fall back to checking only the most recent lines.
	lines := strings.Split(after, "\n")
	start := max(0, len(lines)-12)
	return strings.Contains(strings.Join(lines[start:], "\n"), pattern)
}

// sendOnce types the input text, sends Enter, and handles Edit mode fallback.
func (cs *CopilotSession) sendOnce(input string, preSend string) error {
	// Use -l (literal) flag to prevent tmux from interpreting characters
	// in the prompt text as special key names.
	args := []string{"send-keys", "-l", "-t", cs.name, input}
	cmd := exec.Command("tmux", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux send-keys -l: %w\n%s", err, out)
	}
	time.Sleep(200 * time.Millisecond)
	if err := cs.SendKeys("Enter"); err != nil {
		return err
	}

	// Briefly poll to see if copilot is still in Edit mode (Enter didn't submit).
	// In Edit mode the status bar shows "ctrl+s run command". If detected,
	// send Ctrl+S to actually submit the prompt. Break early if the content
	// changes from the pre-send snapshot, indicating submission has started.
	editDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(editDeadline) {
		content := cs.Capture()
		if isEditMode(content) {
			if err := cs.SendKeys("C-s"); err != nil {
				return err
			}
			break
		}
		if stableContent(content) != preSend {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// isEditMode checks if copilot-cli is in Edit mode by looking for the
// "ctrl+s run command" indicator in the last few lines (status bar area).
// Restricting the search avoids false positives if the phrase appears in
// agent output.
func isEditMode(content string) bool {
	lines := strings.Split(content, "\n")
	const statusPhrase = "ctrl+s run command"
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-3; i-- {
		if strings.Contains(lines[i], statusPhrase) {
			return true
		}
	}
	return false
}

// isAutocompleteMenu detects copilot's slash-command autocomplete dropdown.
// When triggered, copilot shows a list of commands starting with "▋  /"
// below the input line, preventing prompt submission.
func isAutocompleteMenu(content string) bool {
	lines := strings.Split(content, "\n")
	matches := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "▋") && strings.Contains(trimmed, "/") {
			matches++
			if matches >= 2 {
				return true
			}
		}
	}
	return false
}

func (c *CopilotCLI) StartSession(ctx context.Context, dir string) (Session, error) {
	bin, err := exec.LookPath(c.Binary())
	if err != nil {
		return nil, fmt.Errorf("agent binary not found: %w", err)
	}

	// Forward auth-related env vars into the tmux session. tmux starts a new
	// shell that doesn't inherit Go's os.Environ(), so without this the
	// session can lose both token-based auth and local gh/copilot login state.
	var envArgs []string
	for _, key := range []string{"COPILOT_GITHUB_TOKEN", "HOME", "TERM", "XDG_CONFIG_HOME", "GH_CONFIG_DIR"} {
		if v := os.Getenv(key); v != "" {
			envArgs = append(envArgs, key+"="+v)
		}
	}
	args := append([]string{"env"}, envArgs...)
	args = append(args, bin, "--model", "gpt-4.1", "--allow-all")

	name := fmt.Sprintf("copilot-test-%d", time.Now().UnixNano())
	// Strip CI env vars that may affect interactive mode.
	unset := []string{"CI", "GITHUB_ACTIONS", "ENTIRE_TEST_TTY"}
	s, err := NewTmuxSession(name, dir, unset, args[0], args[1:]...)
	if err != nil {
		return nil, err
	}

	// Dismiss startup dialogs (folder trust, etc.) then wait for the "❯" prompt.
	// Copilot CLI shows a "Confirm folder trust" dialog in interactive mode for
	// new directories. "Yes" is pre-selected, so Enter dismisses it.
	foundPrompt := false
	for range 5 {
		content, err := s.WaitFor(`(❯|Enter to select)`, 30*time.Second)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("waiting for startup prompt: %w", err)
		}
		if strings.Contains(content, "❯") && !strings.Contains(content, "Enter to select") {
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
		return nil, errors.New("copilot CLI did not reach interactive prompt after dismissing startup dialogs")
	}
	s.stableAtSend = ""

	return &CopilotSession{TmuxSession: s}, nil
}
