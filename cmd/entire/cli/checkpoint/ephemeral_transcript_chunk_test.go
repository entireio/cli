package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// TestAddTaskMetadataToTree_PartialTranscriptChunkSetFails pins the invariant
// that a shadow checkpoint is never written with a hole in its transcript
// chunk set.
//
// Readers reassemble a chunked transcript from whatever chunk files are
// present, in index order, with no gap marker (agent.SortChunkFiles +
// agent.ReassembleTranscript on the CLI side; the same base-plus-".%03d"
// convention on every other consumer). A chunk that fails to write and is
// skipped therefore produces a transcript that parses cleanly and is silently
// short by up to agent.MaxChunkSize — and because condensation reads the
// session transcript out of the shadow tree, that truncation propagates into
// the committed checkpoint. Both transcript-chunk loops in the committed store
// return on a blob failure; this one must too.
func TestAddTaskMetadataToTree_PartialTranscriptChunkSetFails(t *testing.T) {
	repo, dir := setupTestRepo(t)
	store := newEphemeralStore(repo, DefaultV1Refs())

	transcriptPath := filepath.Join(dir, "full.jsonl")
	// Force chunking: content must exceed agent.MaxChunkSize but still be splittable
 	// by the JSONL chunker (no single line may exceed MaxChunkSize).
 	content := []byte(strings.Repeat("a", agent.MaxChunkSize-1) + "\n" + "b\n")
 	if err := os.WriteFile(transcriptPath, content, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Fail exactly one transcript chunk write (after at least one succeeds).
	original := createBlobFromContent
	t.Cleanup(func() { createBlobFromContent = original })
	calls := 0
 	createBlobFromContent = func(r *git.Repository, b []byte) (plumbing.Hash, error) {
 		calls++
 		if calls == 2 {
 			return plumbing.ZeroHash, os.ErrPermission
 		}
 		return original(r, b)
	}

	baseTreeHash := headTreeHash(t, repo)
	opts := WriteEphemeralTaskOptions{
		SessionID:      "sess-chunk",
		ToolUseID:      "tool-chunk",
		AgentID:        "agent-chunk",
		CheckpointUUID: "uuid-chunk",
		TranscriptPath: transcriptPath,
	}

	_, err := store.addTaskMetadataToTree(context.Background(), baseTreeHash, opts)
	if err == nil {
		t.Fatal("addTaskMetadataToTree succeeded after a transcript chunk blob write failed; " +
			"the checkpoint would be stored with a silently missing transcript chunk")
	}
	if !strings.Contains(err.Error(), "transcript chunk") {
		t.Errorf("error does not name the failing transcript chunk: %v", err)
	}
}

// TestAddTaskMetadataToTree_WritesEveryTranscriptChunk is the positive half:
// with blob writes working, every chunk index the chunker produced is present
// in the resulting tree.
func TestAddTaskMetadataToTree_WritesEveryTranscriptChunk(t *testing.T) {
	t.Parallel()

	repo, dir := setupTestRepo(t)
	store := newEphemeralStore(repo, DefaultV1Refs())

	transcriptPath := filepath.Join(dir, "full.jsonl")
	content := []byte(`{"type":"user"}` + "\n" + `{"type":"assistant"}` + "\n")
	if err := os.WriteFile(transcriptPath, content, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	chunks, err := agent.ChunkTranscript(context.Background(), content, agent.DetectAgentTypeFromContent(content))
	if err != nil {
		t.Fatalf("chunk transcript: %v", err)
	}

	baseTreeHash := headTreeHash(t, repo)
	opts := WriteEphemeralTaskOptions{
		SessionID:      "sess-chunk-ok",
		ToolUseID:      "tool-chunk-ok",
		AgentID:        "agent-chunk-ok",
		CheckpointUUID: "uuid-chunk-ok",
		TranscriptPath: transcriptPath,
	}

	treeHash, err := store.addTaskMetadataToTree(context.Background(), baseTreeHash, opts)
	if err != nil {
		t.Fatalf("addTaskMetadataToTree: %v", err)
	}
	tree, err := repo.TreeObject(treeHash)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	for i := range chunks {
		want := paths.EntireMetadataDir + "/" + opts.SessionID + "/" + agent.ChunkFileName(paths.TranscriptFileName, i)
		if _, err := tree.File(want); err != nil {
			t.Errorf("transcript chunk %d missing from tree at %s: %v", i, want, err)
		}
	}
}

func headTreeHash(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return commit.TreeHash
}
