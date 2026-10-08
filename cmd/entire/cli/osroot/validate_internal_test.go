package osroot

import (
	"os"
	"path/filepath"
	"testing"
)

// The pre-open Lstat and the open are separate syscalls. Model a replacement
// between them deterministically by validating a descriptor for one file
// against the path of another; the check must reject this rather than let the
// caller read from the descriptor it already holds.
func TestValidateOpenedFile_RejectsReplacedEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "planted.json"), []byte(`{"enabled":false}`), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	planted, err := root.Open("planted.json")
	if err != nil {
		t.Fatal(err)
	}
	defer planted.Close()

	if err := validateOpenedFile(root, "settings.json", planted); err == nil {
		t.Fatal("validateOpenedFile accepted a descriptor for a replacement file")
	}
}
