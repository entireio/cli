package jsonutil

import (
	"path/filepath"
	"testing"
)

func TestSyncDir(t *testing.T) {
	t.Parallel()
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
}

func TestSyncDir_MissingDirectoryFails(t *testing.T) {
	t.Parallel()
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("SyncDir missing directory must fail")
	}
}
