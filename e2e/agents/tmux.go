package agents

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// TmuxSession implements Session using tmux for PTY-based interactive agents.
type TmuxSession struct {
	name         string
	stableAtSend string   // stable content snapshot when Send was last called
	cleanups     []func() // run on Close
}

// OnClose registers a function to run when the session is closed.
func (s *TmuxSession) OnClose(fn func()) {
	s.cleanups = append(s.cleanups, fn)
}

// NewTmuxSession creates a new tmux session running the given command in dir.
// unsetEnv lists environment variable names to strip from the session.
//
// The command is wrapped with `env` to propagate PATH from the current process.
// tmux sessions inherit the tmux server's environment (not the client's), so
// without this, binaries added to PATH by the test runner (e.g., freshly built
// `entire` and `vogon`) would not be found inside the session.
func NewTmuxSession(name string, dir string, unsetEnv []string, command string, args ...string) (*TmuxSession, error) {
	s := &TmuxSession{name: name}

	tmuxArgs := []string{"new-session", "-d", "-s", name, "-c", dir}
	// Build a shell command string using `env` to:
	//   1. Propagate PATH from the current process (tmux server may have stale PATH)
	//   2. Unset variables listed in unsetEnv
	// All arguments are shell-quoted to prevent injection or splitting.
	// PATH is always forwarded from the current process so that the freshly-built
	// "entire" binary (prepended to PATH by main_test.go) is available inside the
	// tmux session. Without this, tmux inherits the server's environment which may
	// have an older binary (or none at all).
	var parts []string
	parts = append(parts, "env")
	// Options (-u) must precede variable assignments for BSD env on macOS.
	for _, v := range unsetEnv {
		parts = append(parts, "-u", shellQuote(v))
	}
	parts = append(parts, "PATH="+shellQuote(os.Getenv("PATH")))
	parts = append(parts, shellQuote(command))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	tmuxArgs = append(tmuxArgs, strings.Join(parts, " "))

	cmd := exec.Command("tmux", tmuxArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("tmux new-session: %w\n%s", err, out)
	}
	// Keep the pane around after the command exits so we can capture error output.
	setCmd := exec.Command("tmux", "set-option", "-t", name, "remain-on-exit", "on")
	_ = setCmd.Run()
	return s, nil
}

func (s *TmuxSession) Send(input string) error {
	preSendRaw := s.Capture()
	// Send text and Enter separately — TUIs (Claude, droid) can swallow Enter
	// if it arrives before the input handler finishes processing the text.
	// Droid ingests long pasted prompts over several seconds, so wait until
	// the echoed input has fully rendered before submitting.
	if err := s.SendKeys(input); err != nil {
		return err
	}
	settled := s.waitForInputIngested(preSendRaw)

	// Snapshot the post-echo, pre-submit content. WaitFor requires content to
	// change from this snapshot before it can settle, preventing false matches
	// on prompt characters (e.g. ❯) in the echoed input. Taken before Enter so
	// it can never include response output from a fast agent.
	s.stableAtSend = stableContent(settled)

	// Verify the pane reacted to Enter; a swallowed Enter leaves the prompt
	// sitting unsubmitted in the input box. Retry a couple of times — TUIs
	// treat Enter on an already-submitted (empty) input box as a no-op, and
	// the vogon REPL ignores empty lines.
	preEnter := settled
	for range 3 {
		if err := s.SendKeys("Enter"); err != nil {
			return err
		}
		if s.paneChangedFrom(preEnter, 2*time.Second) {
			break
		}
		preEnter = s.Capture()
	}
	return nil
}

// waitForInputIngested waits until the pane content has changed from preSend
// (the echoed input is visible) and held still for two consecutive polls
// (the TUI's input handler has caught up — droid renders long pastes in
// bursts, so a single quiet interval can fake stability), then returns the
// settled content. Gives up after 15s and returns the last capture.
//
// Compares raw captures: the input box lives in the bottom lines that
// stableContent strips, so stable comparison would be blind to the echo.
func (s *TmuxSession) waitForInputIngested(preSend string) string {
	deadline := time.Now().Add(15 * time.Second)
	last := s.Capture()
	stablePolls := 0
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		current := s.Capture()
		if current != last || current == preSend {
			stablePolls = 0
			last = current
			continue
		}
		stablePolls++
		if stablePolls >= 2 {
			return current
		}
	}
	return last
}

func (s *TmuxSession) paneChangedFrom(prev string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if s.Capture() != prev {
			return true
		}
	}
	return false
}

// SendKeys sends raw tmux key names without appending Enter.
func (s *TmuxSession) SendKeys(keys ...string) error {
	args := append([]string{"send-keys", "-t", s.name}, keys...)
	cmd := exec.Command("tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send-keys: %w\n%s", err, out)
	}
	return nil
}

const (
	settleTime   = 2 * time.Second
	pollInterval = 500 * time.Millisecond
)

// stableContent returns the content with the last few lines stripped,
// so that TUI status bar updates don't prevent the settle timer.
func stableContent(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 3 {
		lines = lines[:len(lines)-3]
	}
	return strings.Join(lines, "\n")
}

func (s *TmuxSession) WaitFor(pattern string, timeout time.Duration) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var matchedAt time.Time
	var lastStable string
	contentChanged := s.stableAtSend == "" // skip change requirement for initial waits

	for time.Now().Before(deadline) {
		content := s.Capture()
		stable := stableContent(content)

		// Bail early if the process has exited and the pattern doesn't match.
		// remain-on-exit keeps the pane alive, so without this check we'd
		// poll a dead pane for the full timeout duration.
		if !re.MatchString(content) {
			if s.IsPaneDead() {
				return content, fmt.Errorf("process exited while waiting for %q\n--- pane content ---\n%s\n--- end pane content ---", pattern, content)
			}
			// Pattern lost — reset
			matchedAt = time.Time{}
			lastStable = ""
			time.Sleep(pollInterval)
			continue
		}

		// Detect content change since Send was called
		if !contentChanged && stable != s.stableAtSend {
			contentChanged = true
		}

		if stable != lastStable {
			// Pattern matches but content is still changing — reset settle timer
			matchedAt = time.Now()
			lastStable = stable
			time.Sleep(pollInterval)
			continue
		}

		// Pattern matches and content hasn't changed since matchedAt.
		// Only settle if content changed at least once after Send
		// (prevents false settle on echoed input before agent starts).
		if contentChanged && time.Since(matchedAt) >= settleTime {
			return content, nil
		}

		time.Sleep(pollInterval)
	}
	content := s.Capture()
	return content, fmt.Errorf("timed out waiting for %q after %s\n--- pane content ---\n%s\n--- end pane content ---", pattern, timeout, content)
}

// IsPaneDead returns true if the process inside the tmux pane has exited.
// This relies on the remain-on-exit option being set (which NewTmuxSession does).
func (s *TmuxSession) IsPaneDead() bool {
	cmd := exec.Command("tmux", "display-message", "-t", s.name, "-p", "#{pane_dead}")
	out, err := cmd.Output()
	if err != nil {
		// If tmux itself fails (e.g. session/pane no longer exists),
		// treat it as dead so WaitFor doesn't poll until timeout.
		return true
	}
	return strings.TrimSpace(string(out)) == "1"
}

func (s *TmuxSession) Capture() string {
	cmd := exec.Command("tmux", "capture-pane", "-t", s.name, "-p")
	out, _ := cmd.Output()
	return strings.TrimRight(string(out), "\n")
}

func (s *TmuxSession) Close() error {
	for _, fn := range s.cleanups {
		fn()
	}
	cmd := exec.Command("tmux", "kill-session", "-t", s.name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux kill-session: %w\n%s", err, out)
	}
	return nil
}

// shellQuote wraps s in single quotes with proper escaping for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
