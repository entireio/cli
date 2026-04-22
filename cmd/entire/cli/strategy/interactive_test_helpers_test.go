package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// forceInteractive makes both interactive.CanPromptInteractively and
// interactive.CanPromptFromHook return true for the duration of the test.
// When the hook path would try to read /dev/tty via askConfirmTTY, tests
// should additionally call stubAskConfirm to control the response without
// touching a real terminal.
func forceInteractive(t testing.TB) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return true }))
}

// forceNonInteractive makes both interactive detection paths return false
// for the duration of the test.
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
