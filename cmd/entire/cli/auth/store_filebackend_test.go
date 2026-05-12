//go:build authfilestore

package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStore_UsesTestStoreFile(t *testing.T) {
	storeFile := filepath.Join(t.TempDir(), "auth-store.json")
	t.Setenv(testAuthStoreFileEnv, storeFile)

	store := NewStore()
	if err := store.SaveToken("http://localhost:8787", "  file-token  "); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}

	otherProcessStore := NewStore()
	got, err := otherProcessStore.GetToken("http://localhost:8787")
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != "file-token" {
		t.Fatalf("GetToken() = %q, want %q", got, "file-token")
	}

	info, err := os.Stat(storeFile)
	if err != nil {
		t.Fatalf("stat store file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("store file mode = %v, want 0600", got)
	}
}

func TestNewStoreWithService_UsesTestStoreFileAndRestrictsExistingFile(t *testing.T) {
	storeFile := filepath.Join(t.TempDir(), "auth-store.json")
	t.Setenv(testAuthStoreFileEnv, storeFile)
	if err := os.WriteFile(storeFile, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("precreate store file: %v", err)
	}
	if err := os.Chmod(storeFile, 0o644); err != nil {
		t.Fatalf("set broad store file permissions: %v", err)
	}

	store := NewStoreWithService("custom-service")
	if err := store.SaveToken("http://localhost:8787", "service-token"); err != nil {
		t.Fatalf("SaveToken() error = %v", err)
	}

	otherProcessStore := NewStoreWithService("custom-service")
	got, err := otherProcessStore.GetToken("http://localhost:8787")
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != "service-token" {
		t.Fatalf("GetToken() = %q, want %q", got, "service-token")
	}

	info, err := os.Stat(storeFile)
	if err != nil {
		t.Fatalf("stat store file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("store file mode = %v, want 0600", got)
	}
}
