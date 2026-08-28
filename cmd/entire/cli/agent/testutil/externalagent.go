package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteExternalAgentBinary writes an executable entire-agent-<name> into dir and
// returns its path.
func WriteExternalAgentBinary(t *testing.T, dir, name, script string) string {
	t.Helper()

	binPath := filepath.Join(dir, "entire-agent-"+name)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write external agent mock %q: %v", binPath, err)
	}
	return binPath
}
