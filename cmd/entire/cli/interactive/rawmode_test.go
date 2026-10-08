package interactive

import (
	"os"
	"path/filepath"
	"testing"
)

// A non-terminal file has no terminal mode to read, so detection must fail open
// (report "not raw"): the raw-mode check may only ever suppress prompts we
// positively know are unusable.
func TestTTYInRawMode_NonTerminalFailsOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "not-a-tty")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()

	if ttyInRawMode(f) {
		t.Error("ttyInRawMode(regular file) = true; want false (fail open)")
	}
}
