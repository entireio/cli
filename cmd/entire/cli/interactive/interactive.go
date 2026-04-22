// Package interactive provides TTY-related helpers shared between the cli
// and strategy packages without inducing an import cycle (strategy cannot
// import cli).
package interactive

import (
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	// PromptModeEnv controls CLI prompt policy.
	PromptModeEnv = "ENTIRE_PROMPTS"

	promptModeAuto  = "auto"
	promptModeNever = "never"
	promptModePlain = "plain"
)

// canPromptCLI and canPromptHook are swappable detection functions. Tests
// replace them via OverrideForTest; production uses checkCLIPrompt /
// checkHookPrompt.
var (
	canPromptCLI  = checkCLIPrompt
	canPromptHook = checkHookPrompt
)

// CanPromptInteractively reports whether a regular CLI command (cobra
// subcommand) can show interactive prompts. Uses term.IsTerminal on stdin,
// which is the standard Go idiom. Returns false when stdin is a pipe, the
// process runs inside a known AI agent subprocess, or stdin is not a
// terminal for any other reason.
//
// Callers inside git hooks must use CanPromptFromHook instead — git
// redirects stdin, so stdin-based detection always reports false there.
func CanPromptInteractively() bool {
	return canPromptCLI()
}

// CanPromptFromHook reports whether a git hook subprocess can show
// interactive prompts. Git hooks run with redirected stdin/stdout, so
// CanPromptInteractively would always report false. CanPromptFromHook
// opens /dev/tty (the controlling terminal) instead, which survives git's
// redirection.
//
// This path also honors ENTIRE_TEST_TTY as a subprocess escape hatch for
// integration tests that spawn the entire CLI as a hook subprocess and
// cannot use OverrideForTest. Production code outside the test harness
// does not set this variable.
func CanPromptFromHook() bool {
	return canPromptHook()
}

// PromptMode returns the configured prompt policy.
func PromptMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PromptModeEnv))) {
	case "", promptModeAuto:
		return promptModeAuto
	case promptModeNever:
		return promptModeNever
	case promptModePlain:
		return promptModePlain
	default:
		return promptModeAuto
	}
}

func checkCLIPrompt() bool {
	switch PromptMode() {
	case promptModeNever:
		return false
	case promptModePlain:
		return term.IsTerminal(int(os.Stdin.Fd())) //nolint:gosec // stdin fd is 0; uintptr->int cannot overflow
	default:
		if isAgentSubprocess() {
			return false
		}
		return term.IsTerminal(int(os.Stdin.Fd())) //nolint:gosec // stdin fd is 0; uintptr->int cannot overflow
	}
}

func checkHookPrompt() bool {
	if v, ok := os.LookupEnv("ENTIRE_TEST_TTY"); ok {
		return v == "1"
	}
	if PromptMode() == promptModeNever {
		return false
	}
	if PromptMode() == promptModeAuto && isAgentSubprocess() {
		return false
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// isAgentSubprocess returns true when the process is a subprocess of a
// known AI coding agent. These agents may pass through a real TTY (e.g.
// tmux) but cannot respond to interactive prompts on the user's behalf,
// so the CLI must treat them as non-interactive regardless of /dev/tty.
func isAgentSubprocess() bool {
	switch {
	case os.Getenv("GEMINI_CLI") != "":
		return true
	case os.Getenv("COPILOT_CLI") != "":
		return true
	case os.Getenv("PI_CODING_AGENT") != "":
		return true
	case os.Getenv("GIT_TERMINAL_PROMPT") == "0":
		return true
	}
	return false
}

// OverrideForTest replaces both CanPromptInteractively and CanPromptFromHook
// detection for the duration of a test. Returns a restore function that
// must be deferred (or passed to t.Cleanup).
//
//	restore := interactive.OverrideForTest(func() bool { return false })
//	defer restore()
func OverrideForTest(fn func() bool) func() {
	oldCLI, oldHook := canPromptCLI, canPromptHook
	canPromptCLI = fn
	canPromptHook = fn
	return func() {
		canPromptCLI = oldCLI
		canPromptHook = oldHook
	}
}
