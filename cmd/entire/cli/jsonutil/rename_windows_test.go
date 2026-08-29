//go:build windows

package jsonutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Test names carry "Windows" so CI's Windows job (-run '(Windows|MSYS)')
// selects them; the fake-predicate tests in rename_test.go never execute the
// real predicate.

// os.Rename returns a *os.LinkError; the predicate has to see through it to
// the Errno, or the retry silently never fires.
func TestRenameIsTransient_Windows(t *testing.T) {
	t.Parallel()
	for _, errno := range []windows.Errno{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_ACCESS_DENIED} {
		err := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: errno}
		if !renameIsTransient(err) {
			t.Errorf("renameIsTransient(%v) = false, want true", err)
		}
	}
	for _, err := range []error{
		&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_FILE_NOT_FOUND},
		fs.ErrNotExist,
		errors.New("unrelated"),
	} {
		if renameIsTransient(err) {
			t.Errorf("renameIsTransient(%v) = true, want false", err)
		}
	}
}

// A destination another handle holds open (Go's os.Open shares read/write but
// not delete) cannot be replaced until that handle closes. The retry budget
// must outlast a short hold.
func TestWriteFileAtomic_RetriesWhileDestinationIsOpen_Windows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(target, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * renameInitialDelay)
		_ = holder.Close()
	}()

	if err := WriteFileAtomic(target, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic while the destination was briefly open: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"v":2}` {
		t.Fatalf("content = %s, want the new bytes", got)
	}
}
