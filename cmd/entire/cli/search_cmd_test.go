package cli

import (
	"strings"
	"testing"
)

// TestSearchCmd_AccessibleModeRequiresQuery verifies that accessible mode
// is treated like --json: a query is required when ACCESSIBLE=1.
// Note: this test modifies process-global state (env var), so it must NOT
// use t.Parallel().
func TestSearchCmd_AccessibleModeRequiresQuery(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--json"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no query with --json + ACCESSIBLE=1")
	}

	want := "query required when using --json, accessible mode, or piped output"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err.Error(), want)
	}
}
