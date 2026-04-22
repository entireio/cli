package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// forceInteractive makes both interactive.CanPromptInteractively and
// interactive.CanPromptFromHook return true for the duration of the test.
func forceInteractive(t *testing.T) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return true }))
}

// forceNonInteractive makes both interactive detection paths return false
// for the duration of the test.
func forceNonInteractive(t *testing.T) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return false }))
}
