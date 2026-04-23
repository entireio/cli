package recap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// newIsolatedRepo sets up a git repo with an entire-sessions dir.
// Must NOT be called from tests that use t.Parallel() — it calls
// t.Chdir(). Clears package-level caches that would otherwise leak
// from earlier tests in the same binary.
//
// See sessions_test.go:27 for the existing pattern this mirrors.
func newIsolatedRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "README.md", "init")
	testutil.GitAdd(t, tmp, "README.md")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)
	// Critical: clear both caches AFTER chdir, otherwise earlier
	// tests' cached values leak into this repo. Without this, tests
	// are flaky depending on run order.
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	return tmp
}

// writeSessionState writes a session.State JSON into .git/entire-sessions/.
func writeSessionState(t *testing.T, repoRoot string, s *session.State) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".git", "entire-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, s.SessionID+".json")
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustTime returns a time or fails the test.
//
//nolint:unused // shared fixture used by upcoming Chunk 1 tests (loader/enrich)
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ctx returns a test context.
func ctx() context.Context { return context.Background() }

type committedFixture struct {
	ID        string // 12-hex-char checkpoint ID
	SessionID string
	Agent     string // e.g. "Claude Code"; optional
	CreatedAt time.Time
	Files     []string
}

// writeCommittedCheckpoint writes a valid two-file metadata pair onto the
// entire/checkpoints/v1 branch: root metadata.json (CheckpointSummary shape)
// and 0/metadata.json (sessionMetadataLite shape). This is what
// strategy.ListCheckpoints reads; any deviation from this shape will cause
// the loader to silently skip the checkpoint.
//
// When called multiple times on the same repo, this merges the new checkpoint
// into the existing entire/checkpoints/v1 tree so both checkpoints are visible
// from the single branch tip that ListCheckpoints reads.
func writeCommittedCheckpoint(t *testing.T, repoRoot string, f committedFixture) {
	t.Helper()
	repo, err := gogit.PlainOpen(repoRoot)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}

	// --- 1. Build session-level metadata blob (0/metadata.json). ---
	// Fields mirror sessionMetadataLite in strategy/common.go.
	sessionMeta := map[string]any{
		"session_id": f.SessionID,
		"created_at": f.CreatedAt.Format(time.RFC3339Nano),
	}
	if f.Agent != "" {
		sessionMeta["agent"] = f.Agent
	}
	sessionBody, err := json.MarshalIndent(sessionMeta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sessionBlobHash := writeBlob(t, repo, sessionBody)

	// --- 2. Build root metadata blob (metadata.json). ---
	// Fields mirror checkpointSummaryLite in strategy/common.go.
	// Session paths are absolute (leading "/"): "/<id[:2]>/<id[2:]>/0/metadata.json".
	summary := checkpoint.CheckpointSummary{
		CheckpointID:     id.CheckpointID(f.ID),
		Strategy:         "manual-commit",
		CheckpointsCount: 1,
		FilesTouched:     f.Files,
		Sessions: []checkpoint.SessionFilePaths{
			{Metadata: "/" + f.ID[:2] + "/" + f.ID[2:] + "/0/metadata.json"},
		},
	}
	summaryBody, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	summaryBlobHash := writeBlob(t, repo, summaryBody)

	// --- 3. Build tree for 0/ (contains session metadata.json). ---
	sessionDirHash := writeTree(t, repo, []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: sessionBlobHash},
	})

	// --- 4. Build tree for <id[2:]>/ (contains root metadata.json + 0/). ---
	cpDirHash := writeTree(t, repo, []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: summaryBlobHash},
		{Name: "0", Mode: filemode.Dir, Hash: sessionDirHash},
	})

	// --- 5. Load existing branch tree (if any) and merge in new shard/cp. ---
	// This ensures successive calls add checkpoints to a single tree,
	// so ListCheckpoints sees all of them from the branch tip.
	rootEntries := loadExistingRootEntries(t, repo)
	shard := f.ID[:2]
	cpName := f.ID[2:]

	// Find or create the shard entry.
	var shardEntries []object.TreeEntry
	if existingShard, ok := findEntry(rootEntries, shard); ok {
		shardTree, err := repo.TreeObject(existingShard.Hash)
		if err != nil {
			t.Fatalf("load shard tree: %v", err)
		}
		shardEntries = append(shardEntries, shardTree.Entries...)
	}
	// Upsert the cp dir in the shard.
	shardEntries = upsertEntry(shardEntries, object.TreeEntry{
		Name: cpName, Mode: filemode.Dir, Hash: cpDirHash,
	})
	shardDirHash := writeTree(t, repo, shardEntries)

	// Upsert the shard dir in the root.
	rootEntries = upsertEntry(rootEntries, object.TreeEntry{
		Name: shard, Mode: filemode.Dir, Hash: shardDirHash,
	})
	rootHash := writeTree(t, repo, rootEntries)

	// --- 6. Commit on entire/checkpoints/v1. ---
	commitHash := writeCommit(t, repo, rootHash, f.CreatedAt, "Checkpoint: "+f.ID)

	// --- 7. Point the branch ref at the new commit. ---
	cmd := exec.CommandContext(context.Background(), "git", "update-ref", "refs/heads/entire/checkpoints/v1", commitHash.String())
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
}

// loadExistingRootEntries returns the current tree entries of the
// entire/checkpoints/v1 branch tip, or an empty slice if the branch does not
// exist. Used by writeCommittedCheckpoint to merge successive checkpoints into
// a single tree.
func loadExistingRootEntries(t *testing.T, repo *gogit.Repository) []object.TreeEntry {
	t.Helper()
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		t.Fatalf("read metadata branch ref: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("get metadata branch commit: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("get metadata branch tree: %v", err)
	}
	out := make([]object.TreeEntry, len(tree.Entries))
	copy(out, tree.Entries)
	return out
}

// findEntry returns the entry with the given name and true if it exists.
func findEntry(entries []object.TreeEntry, name string) (object.TreeEntry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return object.TreeEntry{}, false
}

// upsertEntry returns a new slice with the given entry inserted or replaced by
// name. go-git's tree encoder sorts entries internally, so ordering here does
// not matter for correctness.
func upsertEntry(entries []object.TreeEntry, entry object.TreeEntry) []object.TreeEntry {
	for i, e := range entries {
		if e.Name == entry.Name {
			out := make([]object.TreeEntry, len(entries))
			copy(out, entries)
			out[i] = entry
			return out
		}
	}
	return append(entries, entry)
}

// writeBlob encodes bytes into a git blob and returns its hash.
// Uses repo.Storer.NewEncodedObject() — same pattern as production in
// checkpoint/committed.go.
func writeBlob(t *testing.T, repo *gogit.Repository, data []byte) plumbing.Hash {
	t.Helper()
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// writeTree builds an object.Tree from entries and stores it, returning its
// hash. Entries are sorted per go-git's TreeEntrySorter before encoding (the
// encoder rejects unsorted entries).
func writeTree(t *testing.T, repo *gogit.Repository, entries []object.TreeEntry) plumbing.Hash {
	t.Helper()
	sorted := make([]object.TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Sort(object.TreeEntrySorter(sorted))
	tree := object.Tree{Entries: sorted}
	obj := repo.Storer.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// writeCommit builds an orphan-style commit (no parent) at the given tree.
func writeCommit(t *testing.T, repo *gogit.Repository, tree plumbing.Hash, when time.Time, msg string) plumbing.Hash {
	t.Helper()
	sig := object.Signature{Name: "test", Email: "test@example.com", When: when}
	commit := object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   msg,
		TreeHash:  tree,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
