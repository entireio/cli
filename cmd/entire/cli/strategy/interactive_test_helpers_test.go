package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// forceInteractive makes interactive.CanPromptInteractively() return true
// for the duration of the test. Replaces the old t.Setenv("ENTIRE_TEST_TTY", "1").
// When the hook path would try to read /dev/tty via askConfirmTTY, tests should
// additionally override askConfirmTTY via overrideAskConfirmForTest to control the response.
func forceInteractive(t testing.TB) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return true }))
}

// forceNonInteractive makes interactive.CanPromptInteractively() return false
// for the duration of the test. Replaces the old t.Setenv("ENTIRE_TEST_TTY", "0").
func forceNonInteractive(t testing.TB) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return false }))
}

// stubAskConfirm replaces askConfirmTTY with a fixed-result stub for the
// duration of the test. Tests that exercise the interactive-commit path
// use this to pick a deterministic ttyResult without touching /dev/tty.
func stubAskConfirm(t testing.TB, result ttyResult) {
	t.Helper()
	t.Cleanup(overrideAskConfirmForTest(func(_ string, _ []string, _ string, _ bool) ttyResult {
		return result
	}))
}
