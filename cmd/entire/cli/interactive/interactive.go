// Package interactive provides TTY-related helpers shared between the cli
// and strategy packages without inducing an import cycle (strategy cannot
// import cli).
package interactive

import "os"

// canPrompt is the swappable TTY detection function. Tests replace it via
// OverrideForTest; production uses checkCanPrompt.
var canPrompt = checkCanPrompt

// CanPromptInteractively reports whether interactive confirmation prompts
// (huh forms, yes/no questions, etc.) can be shown. Returns false when
// there is no controlling terminal, or when running inside a known AI
// coding agent subprocess that inherits the user's TTY but cannot respond
// to prompts on the user's behalf.
func CanPromptInteractively() bool {
	return canPrompt()
}

func checkCanPrompt() bool {
	// Subprocess escape hatch for integration tests.
	// Production callers do not set this variable. In-process unit tests use
	// OverrideForTest instead. The only consumers are integration tests that
	// spawn the entire CLI as a subprocess and cannot use OverrideForTest.
	// Keeping the check *before* /dev/tty detection means it also lets tests
	// force the non-interactive path deterministically (value "0" or similar)
	// on systems where /dev/tty happens to be accessible.
	if v, ok := os.LookupEnv("ENTIRE_TEST_TTY"); ok {
		return v == "1"
	}
	if isAgentSubprocess() {
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

// OverrideForTest replaces TTY detection for the duration of a test.
// Returns a restore function that must be deferred (or passed to
// t.Cleanup).
//
//	restore := interactive.OverrideForTest(func() bool { return false })
//	defer restore()
func OverrideForTest(fn func() bool) func() {
	old := canPrompt
	canPrompt = fn
	return func() { canPrompt = old }
}
