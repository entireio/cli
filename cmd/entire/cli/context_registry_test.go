package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
)

func TestContextRegistryNamespaceAndPermissions(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	a := contextRegistryPath(base, "https://us.auth.entire.io", "account-a")
	b := contextRegistryPath(base, "https://eu.auth.entire.io", "account-a")
	c := contextRegistryPath(base, "https://us.auth.entire.io", "account-b")
	if a == b || a == c || b == c {
		t.Fatal("registry namespaces must include both Core URL and account ID")
	}
	registry := contextRegistry{Sessions: []localContextSession{{SessionID: "s1", LastSeen: time.Now()}}}
	if err := writeContextRegistry(a, registry); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry mode = %o, want 600", got)
	}
	if got := filepath.Dir(a); got == base {
		t.Fatal("registry must live in a namespaced directory")
	}
	dirInfo, err := os.Stat(filepath.Dir(a))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("registry directory mode = %o, want 700", got)
	}
}

func TestPruneContextRegistry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := contextRegistry{Sessions: []localContextSession{
		{SessionID: "recent", LastSeen: now.Add(-29 * 24 * time.Hour)},
		{SessionID: "old", LastSeen: now.Add(-31 * 24 * time.Hour)},
	}}
	pruneContextRegistry(&r, now)
	if len(r.Sessions) != 1 || r.Sessions[0].SessionID != "recent" {
		t.Fatalf("pruned sessions = %+v", r.Sessions)
	}
}

func TestReadContextRegistryPersistsPruning(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "namespace", "registry.json")
	registry := contextRegistry{Sessions: []localContextSession{
		{SessionID: "recent", LastSeen: time.Now()},
		{SessionID: "old", LastSeen: time.Now().Add(-31 * 24 * time.Hour)},
	}}
	if err := writeContextRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := readContextRegistry(path); err != nil {
		t.Fatal(err)
	}
	persisted, err := readContextRegistryNoLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Sessions) != 1 || persisted.Sessions[0].SessionID != "recent" {
		t.Fatalf("persisted sessions = %+v", persisted.Sessions)
	}
}

func TestTranscriptPathWithinSessionDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	inside := filepath.Join(dir, "session.jsonl")
	outside := filepath.Join(filepath.Dir(dir), "outside.jsonl")
	if !transcriptPathWithinSessionDir(inside, dir) {
		t.Fatal("path beneath session dir rejected")
	}
	if transcriptPathWithinSessionDir(outside, dir) {
		t.Fatal("path outside session dir accepted")
	}
}

func TestRemoveContextNamespaceWaitsForRegistryWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context", "namespace", "registry.json")
	if err := writeContextRegistry(path, contextRegistry{}); err != nil {
		t.Fatal(err)
	}
	release, err := flock.Acquire(filepath.Dir(path) + ".lock")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- removeContextNamespace(context.Background(), path) }()
	select {
	case err := <-done:
		release()
		t.Fatalf("remove completed while writer lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("namespace still exists after serialized removal: %v", err)
	}
}

func TestRemoveContextNamespaceWithoutExistingCacheRoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing", "context", "namespace", "registry.json")
	if err := removeContextNamespace(t.Context(), path); err != nil {
		t.Fatalf("remove absent namespace: %v", err)
	}
}

func TestRefreshContextRegistrySessionUpdatesHeartbeat(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "context", "namespace", "registry.json")
	old := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	now := old.Add(3 * time.Hour)
	if err := writeContextRegistry(path, contextRegistry{Sessions: []localContextSession{{SessionID: "s1", LastSeen: old}}}); err != nil {
		t.Fatal(err)
	}
	if err := refreshContextRegistrySession(context.Background(), path, "s1", now); err != nil {
		t.Fatal(err)
	}
	got, err := readContextRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || !got.Sessions[0].LastSeen.Equal(now) {
		t.Fatalf("heartbeat = %+v, want %s", got.Sessions, now)
	}
}
