package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// forceInteractive makes interactive.CanPromptInteractively() return true
// for the duration of the test. Replaces the old t.Setenv("ENTIRE_TEST_TTY", "1").
func forceInteractive(t *testing.T) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return true }))
}

// forceNonInteractive makes interactive.CanPromptInteractively() return false
// for the duration of the test. Replaces the old t.Setenv("ENTIRE_TEST_TTY", "0").
func forceNonInteractive(t *testing.T) {
	t.Helper()
	t.Cleanup(interactive.OverrideForTest(func() bool { return false }))
}
