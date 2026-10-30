package jsonutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCreateFileDurableIn_NeverReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := CreateFileDurableIn(root, "key.json", []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateFileDurableIn(root, "key.json", []byte("second"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: err = %v, want fs.ErrExist", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "key.json"))
	if err != nil || string(got) != "first" {
		t.Fatalf("content = %q (err %v), want the first write untouched", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the key (no temp files)", len(entries))
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(filepath.Join(dir, "key.json")); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v (err %v), want 0600", info.Mode().Perm(), err)
		}
	}
}

func TestWriteFileDurableIn_Replaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, content := range []string{"one", "two"} {
		if err := WriteFileDurableIn(root, "state.json", []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || string(got) != "two" {
		t.Fatalf("content = %q (err %v), want two", got, err)
	}
}

func TestRemoveDurableIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := CreateFileDurableIn(root, "key.json", []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second call removes nothing and is not an error
		if err := RemoveDurableIn(root, "key.json"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "key.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after remove: %v", err)
	}
}
