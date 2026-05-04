// Package interactive provides TTY-related helpers shared between the cli
// and strategy packages without inducing an import cycle (strategy cannot
// import cli).
package interactive

import (
	"io"
	"os"
	"testing"

	"golang.org/x/term"
)

// EnvTestTTY is the env-var name for the force-interactive test override.
//   - EnvTestTTY=1 → CanPromptInteractively returns true.
//   - EnvTestTTY set to any other value → returns false.
//   - EnvTestTTY unset → real detection via testing.Testing(), agent
//     sentinels, CI, then /dev/tty probe.
const EnvTestTTY = "ENTIRE_TEST_TTY"

// CanPromptInteractively reports whether interactive confirmation prompts
// (huh forms, yes/no questions, etc.) can be shown. Returns false in CI,
// agent subprocesses that inherit a TTY but can't respond to prompts,
// and other environments without a controlling TTY.
//
// Precedence (first match wins):
//  1. EnvTestTTY=1 forces interactive ON; any other non-empty value forces OFF.
//  2. testing.Testing() — `go test` runs default to OFF so in-process tests
//     don't hang on developer terminals that happen to have a real /dev/tty.
//     Subprocess tests must spawn via execx.NonInteractive (or set EnvTestTTY).
//  3. Agent sentinels — vendor-set by agent subprocesses.
//  4. CI=<non-empty-non-false> — de-facto CI convention.
//  5. /dev/tty probe.
func CanPromptInteractively() bool {
	if v := os.Getenv(EnvTestTTY); v != "" {
		return v == "1"
	}
	if testing.Testing() {
		return false
	}
	if isAgentSubprocessEnv() {
		return false
	}
	// CI=<non-empty> is the de-facto CI-provider convention (GitHub Actions,
	// CircleCI, GitLab, Travis, Buildkite). Self-hosted runners expose /dev/tty,
	// so the probe below isn't enough — an interactive prompt on CI hangs.
	// CI=false is the `is-ci` escape hatch for developers who need to override
	// an inherited value.
	if v := os.Getenv("CI"); v != "" && v != "false" {
		return false
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// UnderTest reports whether the process is running in a test context — either
// inside `go test` (testing.Testing()) or with EnvTestTTY explicitly set. Use
// to skip operations that read from the real terminal (e.g. opening /dev/tty)
// even when CanPromptInteractively() returns true.
func UnderTest() bool {
	return testing.Testing() || os.Getenv(EnvTestTTY) != ""
}

// isAgentSubprocessEnv reports whether the env indicates we're running inside
// an agent subprocess that inherited a TTY but can't respond to prompts:
//   - GEMINI_CLI=1: Gemini CLI shell tool (https://geminicli.com/docs/tools/shell/)
//   - COPILOT_CLI=1: Copilot CLI hook subprocesses (v0.0.421+)
//   - PI_CODING_AGENT=true: Pi Coding Agent shell tool
//   - GIT_TERMINAL_PROMPT=0: caller (CI, Factory AI Droid, etc.) asked git
//     to stop prompting; respect it from git-hook context too.
func isAgentSubprocessEnv() bool {
	return os.Getenv("GEMINI_CLI") != "" ||
		os.Getenv("COPILOT_CLI") != "" ||
		os.Getenv("PI_CODING_AGENT") != "" ||
		os.Getenv("GIT_TERMINAL_PROMPT") == "0"
}

// IsTerminalWriter reports whether w is an *os.File backed by a terminal.
// Use for deciding on color, pager, progress bars, or other writer-scoped
// TTY formatting. For "can I prompt the user?" use CanPromptInteractively.
func IsTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd())) //nolint:gosec // G115: uintptr->int is safe for fd
}
