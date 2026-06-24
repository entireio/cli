package checkpoint

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestBlobResolver_HasBlob_Present(t *testing.T) {
	t.Parallel()

	repo, store, cpID := setupRepoForUpdate(t)

	// Get the metadata branch tree
	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}

	// Navigate to the transcript blob via tree entries
	refs, err := CollectTranscriptBlobHashes(tree, cpID)
	if err != nil {
		t.Fatalf("CollectTranscriptBlobHashes() error = %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("expected at least one transcript blob ref")
	}

	resolver := NewBlobResolver(repo.Storer)

	// Blob should exist — it was written by WriteCommitted
	if !resolver.HasBlob(refs[0].Hash) {
		t.Errorf("HasBlob(%s) = false, want true (blob was written locally)", refs[0].Hash)
	}
}

func TestBlobResolver_HasBlob_Missing(t *testing.T) {
	t.Parallel()

	repo, _, _ := setupRepoForUpdate(t)
	resolver := NewBlobResolver(repo.Storer)

	// Random hash that doesn't exist
	fakeHash := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if resolver.HasBlob(fakeHash) {
		t.Error("HasBlob(fake) = true, want false")
	}
}

func TestBlobResolver_ReadBlob(t *testing.T) {
	t.Parallel()

	repo, store, cpID := setupRepoForUpdate(t)

	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}

	refs, err := CollectTranscriptBlobHashes(tree, cpID)
	if err != nil {
		t.Fatalf("CollectTranscriptBlobHashes() error = %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("expected at least one transcript blob ref")
	}

	resolver := NewBlobResolver(repo.Storer)

	data, err := resolver.ReadBlob(refs[0].Hash)
	if err != nil {
		t.Fatalf("ReadBlob() error = %v", err)
	}
	if len(data) == 0 {
		t.Error("ReadBlob() returned empty data")
	}
	// The transcript content from setupRepoForUpdate
	if string(data) != "provisional transcript line 1\n" {
		t.Errorf("ReadBlob() = %q, want %q", string(data), "provisional transcript line 1\n")
	}
}

func TestBlobResolver_ReadBlob_Missing(t *testing.T) {
	t.Parallel()

	repo, _, _ := setupRepoForUpdate(t)
	resolver := NewBlobResolver(repo.Storer)

	fakeHash := plumbing.NewHash("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	_, err := resolver.ReadBlob(fakeHash)
	if err == nil {
		t.Error("ReadBlob(fake) should return error")
	}
}

func TestCollectTranscriptBlobHashes_SingleSession(t *testing.T) {
	t.Parallel()

	_, store, cpID := setupRepoForUpdate(t)

	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}

	refs, err := CollectTranscriptBlobHashes(tree, cpID)
	if err != nil {
		t.Fatalf("CollectTranscriptBlobHashes() error = %v", err)
	}

	if len(refs) != 1 {
		t.Fatalf("expected 1 transcript ref, got %d", len(refs))
	}

	ref := refs[0]
	if ref.SessionIndex != 0 {
		t.Errorf("SessionIndex = %d, want 0", ref.SessionIndex)
	}
	if ref.Hash.IsZero() {
		t.Error("Hash should not be zero")
	}
	if ref.Path != "0/full.jsonl" {
		t.Errorf("Path = %q, want %q", ref.Path, "0/full.jsonl")
	}
}

func TestCollectTranscriptBlobHashes_MultiSession(t *testing.T) {
	t.Parallel()

	repo, store, cpID := setupRepoForUpdate(t)

	// Write a second session to the same checkpoint
	err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "session-002",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("second session transcript\n")),
		Prompts:      []string{"second prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	if err != nil {
		t.Fatalf("WriteCommitted() for second session error = %v", err)
	}

	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}

	refs, err := CollectTranscriptBlobHashes(tree, cpID)
	if err != nil {
		t.Fatalf("CollectTranscriptBlobHashes() error = %v", err)
	}

	if len(refs) != 2 {
		t.Fatalf("expected 2 transcript refs, got %d", len(refs))
	}

	// Verify session indices
	if refs[0].SessionIndex != 0 {
		t.Errorf("refs[0].SessionIndex = %d, want 0", refs[0].SessionIndex)
	}
	if refs[1].SessionIndex != 1 {
		t.Errorf("refs[1].SessionIndex = %d, want 1", refs[1].SessionIndex)
	}

	// Verify they have different hashes (different transcript content)
	if refs[0].Hash == refs[1].Hash {
		t.Error("multi-session refs should have different blob hashes")
	}

	// Verify all blobs exist locally
	resolver := NewBlobResolver(repo.Storer)
	for i, ref := range refs {
		if !resolver.HasBlob(ref.Hash) {
			t.Errorf("session %d blob %s should be present locally", i, ref.Hash)
		}
	}
}

func TestCollectTranscriptBlobHashes_NonexistentCheckpoint(t *testing.T) {
	t.Parallel()

	_, store, _ := setupRepoForUpdate(t)

	tree, err := store.getSessionsBranchTree()
	if err != nil {
		t.Fatalf("getSessionsBranchTree() error = %v", err)
	}

	fakeID := id.MustCheckpointID("ffffffffffff")
	_, err = CollectTranscriptBlobHashes(tree, fakeID)
	if err == nil {
		t.Error("expected error for nonexistent checkpoint")
	}
}
