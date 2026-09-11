package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestUninstallPrinter_MultiLineWarningStaysInItsBlock pins the shape of a
// warning whose message spans several lines. The reason text can be an
// external plugin's own stderr, which is arbitrary: every line has to sit
// under the ⚠ marker, or a chatty plugin's second line reads as a new
// top-level message.
func TestUninstallPrinter_MultiLineWarningStaysInItsBlock(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	p := newUninstallPrinter(&stdout, &stderr)
	p.warnUnder("failed to remove agent hooks: %v", multiLineError{})

	want := []string{
		"    ⚠ failed to remove agent hooks: plugin exploded",
		"      stack frame one",
		"      stack frame two",
	}
	got := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("warnUnder() printed %d lines, want %d:\n%s", len(got), len(want), stderr.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

type multiLineError struct{}

func (multiLineError) Error() string {
	return "plugin exploded\nstack frame one\nstack frame two\n"
}
