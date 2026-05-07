package review_test

import (
	"strings"
	"testing"

	cli "github.com/entireio/cli/cmd/entire/cli"
)

// TestReviewAttach_Help verifies that `entire review attach --help` surfaces
// the expected flags (--force, --agent, --skills) and the session-id argument.
func TestReviewAttach_Help(t *testing.T) {
	t.Parallel()
	rootCmd := cli.NewRootCmd()
	buf := &strings.Builder{}
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"review", "attach", "--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"attach", "--force", "--agent", "--skills", "session-id"} {
		if !strings.Contains(out, want) {
			t.Errorf("attach --help missing %q:\n%s", want, out)
		}
	}
}

// TestReviewAttach_NoArgsPrintsHelp verifies that calling attach without a
// session-id argument does not panic.
func TestReviewAttach_NoArgsPrintsHelp(t *testing.T) {
	t.Parallel()
	rootCmd := cli.NewRootCmd()
	outBuf := &strings.Builder{}
	errBuf := &strings.Builder{}
	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "attach"})
	// Execute prints help and returns nil when no args provided — no panic.
	_ = rootCmd.Execute() //nolint:errcheck // intentionally discarded; we assert on combined output below

	// Combined output must be non-empty (attach prints help or usage).
	combined := outBuf.String() + errBuf.String()
	if len(combined) == 0 {
		t.Error("expected some output from attach with no args, got nothing")
	}
}
