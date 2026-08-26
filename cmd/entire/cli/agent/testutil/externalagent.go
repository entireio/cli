package testutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// WriteExternalAgentBinary writes an executable entire-agent-<name> into dir and
// returns its path.
//
// It then runs the binary's `info` subcommand once and discards the result. That
// warm-up is what makes the test reliable: external agent discovery gives each
// binary a short budget (a few hundred milliseconds), and the *first* exec of a
// freshly written file is far slower than later ones — on macOS it pays
// code-signing validation and cold page-in, measured at 320-394ms sequentially
// and over a second when several such binaries start at once. Without the
// warm-up, a perfectly healthy mock intermittently fails to load and the test
// looks like a discovery bug.
//
// Warming is deliberately not a fix for the budget itself: a real user
// installing a plugin gets no warm-up, and hits exactly the cold path this
// avoids.
func WriteExternalAgentBinary(t *testing.T, dir, name, script string) string {
	t.Helper()

	binPath := filepath.Join(dir, "entire-agent-"+name)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write external agent mock %q: %v", binPath, err)
	}
	// Best-effort: some mocks intentionally fail `info`, and that is the
	// scenario under test, not a setup error.
	//nolint:errcheck // the exit status is irrelevant; this call exists only to pay the cold-start cost
	_ = exec.CommandContext(context.Background(), binPath, "info").Run()
	return binPath
}
