package interactive

import (
	"bytes"
	"os"
	"testing"
)

func TestCanPromptInteractively_ForcedOn(t *testing.T) {
	t.Setenv(EnvTestTTY, "1")
	if !CanPromptInteractively() {
		t.Errorf("CanPromptInteractively() = false; want true when %s=1", EnvTestTTY)
	}
}

func TestCanPromptInteractively_ForcedOff(t *testing.T) {
	t.Setenv(EnvTestTTY, "0")
	if CanPromptInteractively() {
		t.Errorf("CanPromptInteractively() = true; want false when %s=0", EnvTestTTY)
	}
}

// Under `go test` without an explicit override, testing.Testing() short-circuits
// to non-interactive.
func TestCanPromptInteractively_TestingDefaultsOff(t *testing.T) {
	t.Setenv(EnvTestTTY, "")
	if CanPromptInteractively() {
		t.Error("CanPromptInteractively() = true; want false under testing.Testing()")
	}
}

// CI=false is the `is-ci` escape hatch: a dev may set it to override an
// inherited CI=true. With EnvTestTTY=1 standing in for a real TTY, the gate
// must not short-circuit to false.
func TestCanPromptInteractively_CIFalseOverride(t *testing.T) {
	t.Setenv("CI", "false")
	t.Setenv(EnvTestTTY, "1")
	if !CanPromptInteractively() {
		t.Error("CanPromptInteractively() = false; want true when CI=false")
	}
}

func TestIsAgentSubprocessEnv(t *testing.T) {
	cases := []struct {
		name, key, val string
	}{
		{"gemini", "GEMINI_CLI", "1"},
		{"copilot", "COPILOT_CLI", "1"},
		{"pi", "PI_CODING_AGENT", "true"},
		{"git-terminal-prompt-off", "GIT_TERMINAL_PROMPT", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.key, c.val)
			if !isAgentSubprocessEnv() {
				t.Errorf("isAgentSubprocessEnv() = false; want true when %s=%s", c.key, c.val)
			}
		})
	}
}

// GIT_TERMINAL_PROMPT only counts when explicitly set to "0". Other values
// (or absence) shouldn't trigger the guard.
func TestIsAgentSubprocessEnv_GitTerminalPromptOnIsNotAgent(t *testing.T) {
	// Clear sibling agent-detection vars so the test is hermetic regardless of
	// parent environment (e.g. running inside pi, gemini-cli, copilot-cli).
	t.Setenv("GEMINI_CLI", "")
	t.Setenv("COPILOT_CLI", "")
	t.Setenv("PI_CODING_AGENT", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	if isAgentSubprocessEnv() {
		t.Error("isAgentSubprocessEnv() = true; want false when GIT_TERMINAL_PROMPT=1")
	}
}

func TestUnderTest_TrueByTestingHarness(t *testing.T) {
	t.Setenv(EnvTestTTY, "")
	if !UnderTest() {
		t.Error("UnderTest() = false; want true under testing.Testing()")
	}
}

func TestIsTerminalWriter_NonFile(t *testing.T) {
	t.Parallel()
	if IsTerminalWriter(&bytes.Buffer{}) {
		t.Error("IsTerminalWriter(*bytes.Buffer) = true; want false")
	}
}

func TestIsTerminalWriter_Pipe(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminalWriter(w) {
		t.Error("IsTerminalWriter(pipe) = true; want false")
	}
}

// TestShouldStyle_Gates exercises the pure decision with a simulated
// terminal writer (isTerminalWriter=true) so the NO_COLOR and TERM gates are
// actually reached — `go test` has no real terminal, so calling ShouldStyle
// directly would short-circuit on the terminal check and pass vacuously.
// TERM=cygwin case is the regression test for GH #1267.
func TestShouldStyle_Gates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		noColor    string
		term       string
		isTerminal bool
		want       bool
	}{
		{"terminal with ANSI-capable TERM", "", "xterm-256color", true, true},
		{"TERM=cygwin disables on a terminal", "", "cygwin", true, false},
		{"NO_COLOR disables on a terminal", "1", "xterm-256color", true, false},
		{"non-terminal writer disables", "", "xterm-256color", false, false},
		{"TERM=dumb defers to the terminal check", "", "dumb", true, true},
		{"empty TERM defers to the terminal check", "", "", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldStyle(c.noColor, c.term, c.isTerminal); got != c.want {
				t.Errorf("shouldStyle(%q, %q, %v) = %v; want %v",
					c.noColor, c.term, c.isTerminal, got, c.want)
			}
		})
	}
}

// TestShouldStyle_ReadsEnv verifies the exported wrapper plumbs the process
// env into the decision: NO_COLOR is the first gate, so it disables styling
// regardless of whether stdout is a terminal.
func TestShouldStyle_ReadsEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if ShouldStyle(os.Stdout) {
		t.Error("ShouldStyle(os.Stdout) = true with NO_COLOR set; want false")
	}
}
