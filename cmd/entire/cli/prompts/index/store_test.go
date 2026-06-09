package index

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStore_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &Store{
		repoRoot:  dir,
		indexPath: filepath.Join(dir, "test.ndjson"),
		lockPath:  filepath.Join(dir, "test.lock"),
	}

	if err := store.InitIndex(); err != nil {
		t.Fatalf("failed to init index: %v", err)
	}

	var wg sync.WaitGroup
	successCount := 0
	var mu sync.Mutex

	writerCount := 3
	entriesPerWriter := 10

	for range writerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries := make([]Entry, entriesPerWriter)
			for i := range entriesPerWriter {
				entries[i] = Entry{
					CheckpointID: "test",
					PromptText:   "test prompt",
					Agent:        "test-agent",
					Branch:       "main",
					CreatedAt:    time.Now(),
				}
			}
			err := store.AppendEntries(entries)
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	entries, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to load index: %v", err)
	}

	expectedEntries := successCount * entriesPerWriter
	if len(entries) != expectedEntries {
		t.Errorf("expected %d entries, got %d", expectedEntries, len(entries))
	}

	if successCount == 0 {
		t.Fatal("at least one write should succeed")
	}

	expectedEntries = successCount * entriesPerWriter
	if len(entries) != expectedEntries {
		t.Errorf("expected %d entries, got %d", expectedEntries, len(entries))
	}

	fileData, err := os.ReadFile(store.indexPath)
	if err != nil {
		t.Fatalf("failed to read index file: %v", err)
	}

	lineCount := 0
	for _, b := range fileData {
		if b == '\n' {
			lineCount++
		}
	}

	if lineCount != expectedEntries+1 { // +1 for header
		t.Errorf("expected %d lines in NDJSON, got %d", expectedEntries+1, lineCount)
	}
}

func TestStore_AppendEntries_EmptySlice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &Store{
		repoRoot:  dir,
		indexPath: filepath.Join(dir, "test.ndjson"),
		lockPath:  filepath.Join(dir, "test.lock"),
	}

	if err := store.InitIndex(); err != nil {
		t.Fatalf("failed to init index: %v", err)
	}

	err := store.AppendEntries([]Entry{})
	if err != nil {
		t.Errorf("AppendEntries with empty slice should not error: %v", err)
	}

	entries, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to load index: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestStore_AppendEntries_SingleEntry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &Store{
		repoRoot:  dir,
		indexPath: filepath.Join(dir, "test.ndjson"),
		lockPath:  filepath.Join(dir, "test.lock"),
	}

	if err := store.InitIndex(); err != nil {
		t.Fatalf("failed to init index: %v", err)
	}

	entry := Entry{
		CheckpointID:    "abc123def456",
		SessionIndex:    0,
		TurnIndex:       0,
		Kind:            "session",
		PromptText:      "Fix the login bug",
		PromptTruncated: false,
		CommitHash:      "abc1234",
		CommitMessage:   "feat: add login",
		Branch:          "main",
		Agent:           "Claude Code",
		Model:           "haiku",
		FilesTouched:    []string{"main.go"},
		CreatedAt:       time.Now(),
	}

	if err := store.AppendEntries([]Entry{entry}); err != nil {
		t.Fatalf("failed to append entry: %v", err)
	}

	entries, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("failed to load index: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].CheckpointID != "abc123def456" {
		t.Errorf("expected checkpoint ID 'abc123def456', got '%s'", entries[0].CheckpointID)
	}

	if entries[0].PromptText != "Fix the login bug" {
		t.Errorf("expected prompt 'Fix the login bug', got '%s'", entries[0].PromptText)
	}
}

func TestStore_LockFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := &Store{
		repoRoot:  dir,
		indexPath: filepath.Join(dir, "test.ndjson"),
		lockPath:  filepath.Join(dir, "test.lock"),
	}

	if err := store.InitIndex(); err != nil {
		t.Fatalf("failed to init index: %v", err)
	}

	lock1, err := newLockFile(store.lockPath)
	if err != nil {
		t.Fatalf("failed to create lock1: %v", err)
	}

	if err := lock1.TryLock(); err != nil {
		t.Fatalf("failed to acquire lock1: %v", err)
	}

	lock2, err := newLockFile(store.lockPath)
	if err != nil {
		t.Fatalf("failed to create lock2: %v", err)
	}

	err = lock2.TryLock()
	if err == nil {
		t.Error("expected second lock to fail, but it succeeded")
	}
}

func BenchmarkIndexLoad1K(b *testing.B) {
	dir := b.TempDir()
	store := &Store{
		repoRoot:  dir,
		indexPath: filepath.Join(dir, "test.ndjson"),
		lockPath:  filepath.Join(dir, "test.lock"),
	}

	if err := store.InitIndex(); err != nil {
		b.Fatalf("failed to init index: %v", err)
	}

	entries := make([]Entry, 1000)
	for i := range entries {
		entries[i] = Entry{
			CheckpointID:    "abc123def456",
			SessionIndex:    i % 5,
			TurnIndex:       i % 3,
			Kind:            "session",
			PromptText:      "test prompt with some words here for testing search functionality",
			PromptTruncated: false,
			CommitHash:      "abc1234",
			CommitMessage:   "test commit",
			Branch:          "main",
			Agent:           "Claude Code",
			Model:           "haiku",
			FilesTouched:    []string{"main.go", "util.go"},
			CreatedAt:       time.Now().Add(-time.Duration(i) * time.Hour),
		}
	}

	if err := store.AppendEntries(entries); err != nil {
		b.Fatalf("failed to populate index: %v", err)
	}

	b.ResetTimer()
	for range b.N {
		_, err := store.Load(context.Background())
		if err != nil {
			b.Fatalf("failed to load: %v", err)
		}
	}
}
