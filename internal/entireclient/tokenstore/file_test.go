package tokenstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *fileStore {
	t.Helper()
	return &fileStore{path: filepath.Join(t.TempDir(), "tokens.json")}
}

func TestFileStore_GetMissingFile(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get("svc", "user")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_SetAndGet(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("got %q, want %q", got, "secret")
	}
}

func TestFileStore_GetWrongService(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("other", "alice")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_GetWrongUser(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("svc", "bob")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_Overwrite(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("svc", "alice", "new"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Fatalf("got %q, want %q", got, "new")
	}
}

func TestFileStore_MultipleServices(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc1", "alice", "pw1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("svc2", "bob", "pw2"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pw1" {
		t.Fatalf("got %q, want %q", got, "pw1")
	}

	got, err = s.Get("svc2", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pw2" {
		t.Fatalf("got %q, want %q", got, "pw2")
	}
}

func TestFileStore_Delete(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("svc", "alice")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_DeleteCleansEmptyService(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatal(err)
	}

	// Re-read the file to confirm the service key is gone entirely.
	store, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store["svc"]; ok {
		t.Fatal("expected service key to be removed when last user is deleted")
	}
}

func TestFileStore_DeletePreservesOtherUsers(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "pw1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("svc", "bob", "pw2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("svc", "alice"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != "pw2" {
		t.Fatalf("got %q, want %q", got, "pw2")
	}
}

func TestFileStore_DeleteNotFound(t *testing.T) {
	s := newTestStore(t)

	err := s.Delete("svc", "alice")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_DeleteNotFoundUser(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	err := s.Delete("svc", "bob")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFileStore_LoadCorruptFile(t *testing.T) {
	s := newTestStore(t)

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get("svc", "user")
	if err == nil {
		t.Fatal("expected error for corrupt file")
	}
}

func TestFileStore_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	s := &fileStore{path: filepath.Join(dir, "tokens.json")}

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("svc", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("got %q, want %q", got, "secret")
	}
}

func TestFileStore_FilePermissions(t *testing.T) {
	s := newTestStore(t)

	if err := s.Set("svc", "alice", "secret"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Fatalf("file permissions = %o, want 0600", perm)
	}
}
