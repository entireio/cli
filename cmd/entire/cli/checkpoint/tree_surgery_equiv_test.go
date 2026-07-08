package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestBuildTreeWithChanges_AppliesModificationsDeletionsAndMetadata verifies
// that the ApplyTreeChanges-based buildTreeWithChanges applies file
// modifications, deletions, and metadata-directory additions while leaving
// unrelated tree entries untouched.
func TestBuildTreeWithChanges_AppliesModificationsDeletionsAndMetadata(t *testing.T) { //nolint:paralleltest // t.Chdir requires non-parallel
	repo, dir := setupTestRepo(t)
	store := newEphemeralStore(repo, DefaultV1Refs())

	// Get the base tree hash from HEAD
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	baseTreeHash := commit.TreeHash

	// Create modified and deleted files
	modifiedFiles := []string{"file1.txt", "file2.txt"}
	deletedFiles := []string{"file3.txt"}

	// Write modified files to disk
	for _, f := range modifiedFiles {
		path := filepath.Join(dir, f)
		if err := os.WriteFile(path, []byte("modified content for "+f), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	// Create metadata directory with a file
	metadataDir := ".entire/metadata/test-session"
	metadataDirAbs := filepath.Join(dir, metadataDir)
	if err := os.MkdirAll(metadataDirAbs, 0o750); err != nil {
		t.Fatalf("mkdir metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDirAbs, "full.jsonl"), []byte(`{"type":"test"}`), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Switch to repo dir so paths.WorktreeRoot() resolves correctly
	t.Chdir(dir)

	newHash, err := store.buildTreeWithChanges(context.Background(), baseTreeHash, modifiedFiles, deletedFiles, metadataDir, metadataDirAbs)
	if err != nil {
		t.Fatalf("buildTreeWithChanges: %v", err)
	}

	newTree, err := repo.TreeObject(newHash)
	if err != nil {
		t.Fatalf("read new tree: %v", err)
	}

	// Modified files carry the new on-disk content.
	for _, f := range modifiedFiles {
		file, fileErr := newTree.File(f)
		if fileErr != nil {
			t.Fatalf("modified file %s missing from tree: %v", f, fileErr)
		}
		content, contentErr := file.Contents()
		if contentErr != nil {
			t.Fatalf("read %s: %v", f, contentErr)
		}
		if want := "modified content for " + f; content != want {
			t.Errorf("%s content = %q, want %q", f, content, want)
		}
	}

	// Deleted files are gone.
	for _, f := range deletedFiles {
		if _, fileErr := newTree.File(f); fileErr == nil {
			t.Errorf("deleted file %s still present in tree", f)
		}
	}

	// Metadata directory content was added at the tree-relative path.
	if _, err := newTree.File(metadataDir + "/full.jsonl"); err != nil {
		t.Errorf("metadata file missing from tree: %v", err)
	}

	// Unrelated entries are untouched.
	if _, err := newTree.File("src/main.go"); err != nil {
		t.Errorf("unrelated file src/main.go missing from tree: %v", err)
	}
}

// TestAddTaskMetadataToTree_EquivalenceWithFlattenRebuild verifies that
// the ApplyTreeChanges-based addTaskMetadataToTree produces identical trees.
func TestAddTaskMetadataToTree_EquivalenceWithFlattenRebuild(t *testing.T) {
	t.Parallel()

	repo, _ := setupTestRepo(t)
	store := newEphemeralStore(repo, DefaultV1Refs())

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	baseTreeHash := commit.TreeHash

	// Test without transcripts — the tree structure equivalence is what matters.
	// Transcript processing (chunking, redaction) is covered by integration tests.
	opts := WriteEphemeralTaskOptions{
		SessionID:      "sess-001",
		ToolUseID:      "tool-001",
		AgentID:        "agent-001",
		CheckpointUUID: "uuid-001",
	}

	// New approach (ApplyTreeChanges)
	newHash, err := store.addTaskMetadataToTree(context.Background(), baseTreeHash, opts)
	if err != nil {
		t.Fatalf("addTaskMetadataToTree (new): %v", err)
	}

	// Old approach: manually flatten and rebuild
	oldHash := flattenRebuildTaskMetadata(t, repo, baseTreeHash, opts)

	if newHash != oldHash {
		t.Errorf("tree hash mismatch: new=%s old=%s", newHash, oldHash)
	}
}

// TestAddTaskMetadataToTree_IncrementalPath verifies the incremental checkpoint
// branch of addTaskMetadataToTree produces a valid tree with the expected entry.
// We can't do exact hash comparison because the incremental checkpoint embeds
// time.Now(), so instead we verify the file exists at the correct path in the tree.
func TestAddTaskMetadataToTree_IncrementalPath(t *testing.T) {
	t.Parallel()

	repo, _ := setupTestRepo(t)
	store := newEphemeralStore(repo, DefaultV1Refs())

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	opts := WriteEphemeralTaskOptions{
		SessionID:           "sess-002",
		ToolUseID:           "tool-002",
		IsIncremental:       true,
		IncrementalType:     "todo_update",
		IncrementalSequence: 3,
		IncrementalData:     []byte(`{"items":["task1","task2"]}`),
	}

	newTreeHash, err := store.addTaskMetadataToTree(context.Background(), commit.TreeHash, opts)
	if err != nil {
		t.Fatalf("addTaskMetadataToTree (incremental): %v", err)
	}

	// Verify the checkpoint file exists at the expected path
	newTree, err := repo.TreeObject(newTreeHash)
	if err != nil {
		t.Fatalf("read new tree: %v", err)
	}

	expectedPath := ".entire/metadata/sess-002/tasks/tool-002/checkpoints/003-tool-002.json"
	file, err := newTree.File(expectedPath)
	if err != nil {
		t.Fatalf("file not found at %s: %v", expectedPath, err)
	}

	content, err := file.Contents()
	if err != nil {
		t.Fatalf("read file contents: %v", err)
	}

	// Verify it's valid JSON with expected fields
	if !strings.Contains(content, `"type": "todo_update"`) {
		t.Errorf("expected type field in content, got: %s", content)
	}
	if !strings.Contains(content, `"tool_use_id": "tool-002"`) {
		t.Errorf("expected tool_use_id field in content, got: %s", content)
	}

	// Verify original files are still present (tree surgery didn't destroy them)
	if _, err := newTree.File("file1.txt"); err != nil {
		t.Errorf("original file1.txt missing from tree after incremental update")
	}
}

// setupTestRepo creates a temporary git repo with some initial files.
func setupTestRepo(t *testing.T) (*gogit.Repository, string) {
	t.Helper()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	testutil.InitRepo(t, dir)
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("git open: %v", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}

	// Create initial files
	for _, name := range []string{"file1.txt", "file2.txt", "file3.txt", "src/main.go"} {
		abs := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte("initial content of "+name), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	// Create .gitignore
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".entire/\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if _, err := wt.Add(".gitignore"); err != nil {
		t.Fatalf("add .gitignore: %v", err)
	}

	_, err = wt.Commit("Initial commit", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	return repo, dir
}

// flattenRebuildTaskMetadata is the old FlattenTree+BuildTreeFromEntries approach
// for addTaskMetadataToTree comparison.
func flattenRebuildTaskMetadata(
	t *testing.T, repo *gogit.Repository,
	baseTreeHash plumbing.Hash,
	opts WriteEphemeralTaskOptions,
) plumbing.Hash {
	t.Helper()

	baseTree, err := repo.TreeObject(baseTreeHash)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	entries := make(map[string]object.TreeEntry)
	if err := FlattenTree(repo, baseTree, "", entries); err != nil {
		t.Fatalf("flatten: %v", err)
	}

	sessionMetadataDir := ".entire/metadata/" + opts.SessionID
	taskMetadataDir := sessionMetadataDir + "/tasks/" + opts.ToolUseID

	// Checkpoint.json
	checkpointJSON := []byte(`{
  "session_id": "` + opts.SessionID + `",
  "tool_use_id": "` + opts.ToolUseID + `",
  "checkpoint_uuid": "` + opts.CheckpointUUID + `",
  "agent_id": "` + opts.AgentID + `"
}`)
	blobHash, err := CreateBlobFromContent(repo, checkpointJSON)
	if err != nil {
		t.Fatalf("create blob: %v", err)
	}
	cpPath := taskMetadataDir + "/checkpoint.json"
	entries[cpPath] = object.TreeEntry{
		Name: cpPath,
		Mode: filemode.Regular,
		Hash: blobHash,
	}

	hash, err := BuildTreeFromEntries(context.Background(), repo, entries)
	if err != nil {
		t.Fatalf("build tree: %v", err)
	}
	return hash
}
