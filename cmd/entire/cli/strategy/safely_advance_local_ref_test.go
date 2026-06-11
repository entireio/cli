package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const safelyAdvanceTestRef plumbing.ReferenceName = "refs/heads/safely-advance-test"

// newSafelyAdvanceTestRepo opens an empty git repository for ref-manipulation
// tests. It uses testutil.InitRepo so author/GPG config matches the rest of
// the suite, but the tests themselves operate purely via plumbing — no
// worktree state needed.
func newSafelyAdvanceTestRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	return repo
}

// makeEmptyTreeCommit writes a commit with an empty tree and the given parents
// and message. Different messages produce different hashes, so callers can
// create distinct commits (including divergent siblings) without touching
// files.
func makeEmptyTreeCommit(t *testing.T, repo *git.Repository, parents []plumbing.Hash, msg string) plumbing.Hash {
	t.Helper()
	emptyTree := object.Tree{}
	emptyTreeObj := repo.Storer.NewEncodedObject()
	if err := emptyTree.Encode(emptyTreeObj); err != nil {
		t.Fatalf("encode empty tree: %v", err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(emptyTreeObj)
	if err != nil {
		t.Fatalf("store empty tree: %v", err)
	}
	sig := object.Signature{Name: "T", Email: "t@example.com", When: time.Unix(0, 0).UTC()}
	commit := &object.Commit{
		TreeHash:     treeHash,
		Message:      msg,
		Author:       sig,
		Committer:    sig,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("encode commit %q: %v", msg, err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit %q: %v", msg, err)
	}
	return hash
}

// forceSetTestRef points safelyAdvanceTestRef at hash unconditionally —
// tests deliberately rewind/diverge the ref to set up the scenarios under
// test.
func forceSetTestRef(t *testing.T, repo *git.Repository, hash plumbing.Hash) {
	t.Helper()
	if err := repo.Storer.SetReference(plumbing.NewHashReference(safelyAdvanceTestRef, hash)); err != nil {
		t.Fatalf("SetReference %s: %v", safelyAdvanceTestRef, err)
	}
}

// readTestRef reads safelyAdvanceTestRef and fatals if it is missing. Used
// after a call to SafelyAdvanceLocalRef to inspect the resulting ref state.
func readTestRef(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(safelyAdvanceTestRef, true)
	if err != nil {
		t.Fatalf("read %s: %v", safelyAdvanceTestRef, err)
	}
	return ref.Hash()
}

func makeTreeCommit(t *testing.T, repo *git.Repository, parents []plumbing.Hash, msg string, files map[string]string) plumbing.Hash {
	t.Helper()
	entries := make(map[string]object.TreeEntry, len(files))
	for path, contents := range files {
		blobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(contents))
		if err != nil {
			t.Fatalf("create blob %s: %v", path, err)
		}
		entries[path] = object.TreeEntry{Name: path, Mode: 0o100644, Hash: blobHash}
	}
	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, entries)
	if err != nil {
		t.Fatalf("build tree for %q: %v", msg, err)
	}
	sig := object.Signature{Name: "T", Email: "t@example.com", When: time.Unix(0, 0).UTC()}
	commit := &object.Commit{
		TreeHash:     treeHash,
		Message:      msg,
		Author:       sig,
		Committer:    sig,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("encode commit %q: %v", msg, err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit %q: %v", msg, err)
	}
	return hash
}

func assertCommitFile(t *testing.T, repo *git.Repository, commitHash plumbing.Hash, path, want string) {
	t.Helper()
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		t.Fatalf("commit %s: %v", commitHash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree for %s: %v", commitHash, err)
	}
	file, err := tree.File(path)
	if err != nil {
		t.Fatalf("file %s in %s: %v", path, commitHash, err)
	}
	got, err := file.Contents()
	if err != nil {
		t.Fatalf("contents for %s in %s: %v", path, commitHash, err)
	}
	if got != want {
		t.Fatalf("contents for %s in %s = %q, want %q", path, commitHash, got, want)
	}
}

func TestSafelyAdvanceLocalRef_LocalMissing_SetsToTarget(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, a); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != a {
		t.Errorf("local ref = %s, want %s", got, a)
	}
}

func TestSafelyAdvanceLocalRef_LocalEqualsTarget_NoOp(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")
	forceSetTestRef(t, repo, a)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, a); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != a {
		t.Errorf("local ref = %s, want %s (unchanged)", got, a)
	}
}

func TestSafelyAdvanceLocalRef_LocalAhead_NoOp(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")
	b := makeEmptyTreeCommit(t, repo, []plumbing.Hash{a}, "B")
	forceSetTestRef(t, repo, b)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, a); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != b {
		t.Errorf("locally-ahead ref must not rewind: got %s, want %s", got, b)
	}
}

func TestSafelyAdvanceLocalRef_LocalBehind_FastForwards(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	a := makeEmptyTreeCommit(t, repo, nil, "A")
	b := makeEmptyTreeCommit(t, repo, []plumbing.Hash{a}, "B")
	forceSetTestRef(t, repo, a)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, b); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	if got := readTestRef(t, repo); got != b {
		t.Errorf("local ref should have fast-forwarded: got %s, want %s", got, b)
	}
}

func TestSafelyAdvanceLocalRef_Diverged_ReplaysLocalOntoTarget(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	base := makeTreeCommit(t, repo, nil, "base", map[string]string{"base.txt": "base"})
	localTip := makeTreeCommit(t, repo, []plumbing.Hash{base}, "local-only-work", map[string]string{
		"base.txt":  "base",
		"local.txt": "local",
	})
	targetTip := makeTreeCommit(t, repo, []plumbing.Hash{base}, "remote-only-work", map[string]string{
		"base.txt":   "base",
		"remote.txt": "remote",
	})
	forceSetTestRef(t, repo, localTip)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, targetTip); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	got := readTestRef(t, repo)
	if got == localTip || got == targetTip {
		t.Fatalf("diverged ref should be replayed onto target: got %s, local %s, target %s", got, localTip, targetTip)
	}
	replayedCommit, err := repo.CommitObject(got)
	if err != nil {
		t.Fatalf("replayed commit %s: %v", got, err)
	}
	if len(replayedCommit.ParentHashes) != 1 || replayedCommit.ParentHashes[0] != targetTip {
		t.Fatalf("replayed commit parents = %v, want [%s]", replayedCommit.ParentHashes, targetTip)
	}
	assertCommitFile(t, repo, got, "base.txt", "base")
	assertCommitFile(t, repo, got, "local.txt", "local")
	assertCommitFile(t, repo, got, "remote.txt", "remote")
}

func TestSafelyAdvanceLocalRef_UnrelatedHistory_ReplaysLocalOntoTarget(t *testing.T) {
	t.Parallel()
	repo := newSafelyAdvanceTestRepo(t)
	localOnly := makeTreeCommit(t, repo, nil, "local-orphan", map[string]string{"local.txt": "local"})
	targetOnly := makeTreeCommit(t, repo, nil, "target-orphan", map[string]string{"remote.txt": "remote"})
	forceSetTestRef(t, repo, localOnly)

	if err := SafelyAdvanceLocalRef(context.Background(), repo, safelyAdvanceTestRef, targetOnly); err != nil {
		t.Fatalf("SafelyAdvanceLocalRef: %v", err)
	}
	got := readTestRef(t, repo)
	if got == localOnly || got == targetOnly {
		t.Fatalf("unrelated-history ref should be replayed onto target: got %s, local %s, target %s", got, localOnly, targetOnly)
	}
	replayedCommit, err := repo.CommitObject(got)
	if err != nil {
		t.Fatalf("replayed commit %s: %v", got, err)
	}
	if len(replayedCommit.ParentHashes) != 1 || replayedCommit.ParentHashes[0] != targetOnly {
		t.Fatalf("replayed commit parents = %v, want [%s]", replayedCommit.ParentHashes, targetOnly)
	}
	assertCommitFile(t, repo, got, "local.txt", "local")
	assertCommitFile(t, repo, got, "remote.txt", "remote")
}
