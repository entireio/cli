package checkpoint

import (
	"fmt"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// storeBlob is a test helper that stores a blob in the repo and returns its hash.
func storeBlob(t *testing.T, repo *git.Repository, content string) plumbing.Hash {
	t.Helper()
	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("failed to get blob writer: %v", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("failed to write blob content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close blob writer: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store blob: %v", err)
	}
	return hash
}

// mustInitBareRepo creates a bare git repo for testing.
func mustInitBareRepo(t *testing.T) *git.Repository {
	t.Helper()
	repo, err := git.PlainInit(t.TempDir(), true)
	if err != nil {
		t.Fatalf("failed to init bare repo: %v", err)
	}
	return repo
}

// mustStoreTree stores a tree and fatals on error.
func mustStoreTree(t *testing.T, repo *git.Repository, entries []object.TreeEntry) plumbing.Hash {
	t.Helper()
	hash, err := storeTree(repo, entries)
	if err != nil {
		t.Fatalf("storeTree() error = %v", err)
	}
	return hash
}

// mustTreeObject reads a tree object and fatals on error.
func mustTreeObject(t *testing.T, repo *git.Repository, hash plumbing.Hash) *object.Tree {
	t.Helper()
	tree, err := repo.TreeObject(hash)
	if err != nil {
		t.Fatalf("failed to read tree %s: %v", hash, err)
	}
	return tree
}

// flattenTreeHelper reads a tree recursively and returns all entries as path->hash.
func flattenTreeHelper(t *testing.T, repo *git.Repository, treeHash plumbing.Hash, prefix string) map[string]plumbing.Hash {
	t.Helper()
	result := make(map[string]plumbing.Hash)
	tree := mustTreeObject(t, repo, treeHash)
	for _, entry := range tree.Entries {
		fullPath := entry.Name
		if prefix != "" {
			fullPath = prefix + "/" + entry.Name
		}
		if entry.Mode == filemode.Dir {
			sub := flattenTreeHelper(t, repo, entry.Hash, fullPath)
			for k, v := range sub {
				result[k] = v
			}
		} else {
			result[fullPath] = entry.Hash
		}
	}
	return result
}

func TestSplitFirstSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path      string
		wantFirst string
		wantRest  string
	}{
		{"file.txt", "file.txt", ""},
		{"a/b", "a", "b"},
		{"a/b/c", "a", "b/c"},
		{"dir/sub/file.txt", "dir", "sub/file.txt"},
	}

	for _, tt := range tests {
		first, rest := splitFirstSegment(tt.path)
		if first != tt.wantFirst || rest != tt.wantRest {
			t.Errorf("splitFirstSegment(%q) = (%q, %q), want (%q, %q)",
				tt.path, first, rest, tt.wantFirst, tt.wantRest)
		}
	}
}

func TestStoreTree_RoundTrip(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobHash := storeBlob(t, repo, "hello")
	entries := []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blobHash},
	}

	hash, err := storeTree(repo, entries)
	if err != nil {
		t.Fatalf("storeTree() error = %v", err)
	}
	if hash == plumbing.ZeroHash {
		t.Fatal("storeTree() returned zero hash")
	}

	// Read it back
	tree := mustTreeObject(t, repo, hash)
	if len(tree.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(tree.Entries))
	}
	if tree.Entries[0].Name != "file.txt" {
		t.Errorf("expected file.txt, got %s", tree.Entries[0].Name)
	}
	if tree.Entries[0].Hash != blobHash {
		t.Errorf("hash mismatch: got %s, want %s", tree.Entries[0].Hash, blobHash)
	}
}

func TestUpdateSubtree_CreateFromEmpty(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blob1 := storeBlob(t, repo, "content1")
	blob2 := storeBlob(t, repo, "content2")

	newEntries := []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: blob1},
		{Name: "full.jsonl", Mode: filemode.Regular, Hash: blob2},
	}

	// Create tree at path a3/b2c4d5e6f7/ from empty root
	hash, err := UpdateSubtree(repo, plumbing.ZeroHash, []string{"a3", "b2c4d5e6f7"}, newEntries, UpdateSubtreeOptions{})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	// Verify structure: root -> a3/ -> b2c4d5e6f7/ -> {metadata.json, full.jsonl}
	files := flattenTreeHelper(t, repo, hash, "")
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files["a3/b2c4d5e6f7/metadata.json"] != blob1 {
		t.Error("metadata.json not found or wrong hash")
	}
	if files["a3/b2c4d5e6f7/full.jsonl"] != blob2 {
		t.Error("full.jsonl not found or wrong hash")
	}
}

func TestUpdateSubtree_PreservesSiblings(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobA := storeBlob(t, repo, "existing-a")
	blobB := storeBlob(t, repo, "existing-b")
	blobNew := storeBlob(t, repo, "new-content")

	// Build an initial tree:
	// a3/
	//   existing1/ -> {file.txt: blobA}
	//   existing2/ -> {file.txt: blobB}
	innerTree1 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blobA},
	})
	innerTree2 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blobB},
	})
	shardTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "existing1", Mode: filemode.Dir, Hash: innerTree1},
		{Name: "existing2", Mode: filemode.Dir, Hash: innerTree2},
	})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "a3", Mode: filemode.Dir, Hash: shardTree},
	})

	// Now add a new subtree at a3/newcheckpoint/
	newEntries := []object.TreeEntry{
		{Name: "data.json", Mode: filemode.Regular, Hash: blobNew},
	}
	newRoot, err := UpdateSubtree(repo, rootTree, []string{"a3", "newcheckpoint"}, newEntries, UpdateSubtreeOptions{})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, newRoot, "")

	// Should have 3 files now: 2 existing + 1 new
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), files)
	}
	if files["a3/existing1/file.txt"] != blobA {
		t.Error("existing1/file.txt lost or wrong hash")
	}
	if files["a3/existing2/file.txt"] != blobB {
		t.Error("existing2/file.txt lost or wrong hash")
	}
	if files["a3/newcheckpoint/data.json"] != blobNew {
		t.Error("newcheckpoint/data.json not found")
	}

	// Verify sibling subtrees weren't re-created (same hash)
	root := mustTreeObject(t, repo, newRoot)
	for _, e := range root.Entries {
		if e.Name == "a3" {
			a3Tree := mustTreeObject(t, repo, e.Hash)
			for _, sub := range a3Tree.Entries {
				if sub.Name == "existing1" && sub.Hash != innerTree1 {
					t.Error("existing1 subtree hash changed — should be preserved")
				}
				if sub.Name == "existing2" && sub.Hash != innerTree2 {
					t.Error("existing2 subtree hash changed — should be preserved")
				}
			}
		}
	}
}

func TestUpdateSubtree_ReplaceExisting(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobOld := storeBlob(t, repo, "old")
	blobNew := storeBlob(t, repo, "new")

	// Build initial tree: a3/ckpt1/ -> {metadata.json: blobOld}
	innerTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: blobOld},
	})
	shardTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "ckpt1", Mode: filemode.Dir, Hash: innerTree},
	})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "a3", Mode: filemode.Dir, Hash: shardTree},
	})

	// Replace entries at a3/ckpt1/ with ReplaceAll (default)
	newEntries := []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: blobNew},
		{Name: "extra.txt", Mode: filemode.Regular, Hash: blobNew},
	}
	newRoot, err := UpdateSubtree(repo, rootTree, []string{"a3", "ckpt1"}, newEntries, UpdateSubtreeOptions{
		MergeMode: ReplaceAll,
	})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, newRoot, "")
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
	if files["a3/ckpt1/metadata.json"] != blobNew {
		t.Error("metadata.json should have new hash")
	}
	if files["a3/ckpt1/extra.txt"] != blobNew {
		t.Error("extra.txt should exist")
	}
}

func TestUpdateSubtree_MergeKeepExisting(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobExisting := storeBlob(t, repo, "existing")
	blobNew := storeBlob(t, repo, "new")
	blobReplacement := storeBlob(t, repo, "replacement")

	// Build initial: a3/ckpt1/ -> {keep.txt: blobExisting, replace.txt: blobExisting, delete.txt: blobExisting}
	innerTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "delete.txt", Mode: filemode.Regular, Hash: blobExisting},
		{Name: "keep.txt", Mode: filemode.Regular, Hash: blobExisting},
		{Name: "replace.txt", Mode: filemode.Regular, Hash: blobExisting},
	})
	shardTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "ckpt1", Mode: filemode.Dir, Hash: innerTree},
	})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "a3", Mode: filemode.Dir, Hash: shardTree},
	})

	// Merge: add new.txt, replace replace.txt, delete delete.txt, keep keep.txt
	newEntries := []object.TreeEntry{
		{Name: "new.txt", Mode: filemode.Regular, Hash: blobNew},
		{Name: "replace.txt", Mode: filemode.Regular, Hash: blobReplacement},
	}
	newRoot, err := UpdateSubtree(repo, rootTree, []string{"a3", "ckpt1"}, newEntries, UpdateSubtreeOptions{
		MergeMode:   MergeKeepExisting,
		DeleteNames: []string{"delete.txt"},
	})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, newRoot, "")
	if len(files) != 3 {
		t.Fatalf("expected 3 files (keep, replace, new), got %d: %v", len(files), files)
	}
	if files["a3/ckpt1/keep.txt"] != blobExisting {
		t.Error("keep.txt should be preserved with original hash")
	}
	if files["a3/ckpt1/replace.txt"] != blobReplacement {
		t.Error("replace.txt should have replacement hash")
	}
	if files["a3/ckpt1/new.txt"] != blobNew {
		t.Error("new.txt should be added")
	}
	if _, ok := files["a3/ckpt1/delete.txt"]; ok {
		t.Error("delete.txt should have been deleted")
	}
}

func TestUpdateSubtree_PreservesTopLevelSiblings(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobA := storeBlob(t, repo, "shard-a")
	blobB := storeBlob(t, repo, "shard-b")
	blobNew := storeBlob(t, repo, "new")

	// Build initial tree with two top-level shard directories
	// a3/ -> ckpt1/ -> {file.txt}
	// b7/ -> ckpt2/ -> {file.txt}
	innerA := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blobA},
	})
	shardA := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "ckpt1", Mode: filemode.Dir, Hash: innerA},
	})
	innerB := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blobB},
	})
	shardB := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "ckpt2", Mode: filemode.Dir, Hash: innerB},
	})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "a3", Mode: filemode.Dir, Hash: shardA},
		{Name: "b7", Mode: filemode.Dir, Hash: shardB},
	})

	// Modify only a3/ckpt1
	newRoot, err := UpdateSubtree(repo, rootTree, []string{"a3", "ckpt1"}, []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blobNew},
	}, UpdateSubtreeOptions{})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	// b7 subtree hash should be preserved exactly
	root := mustTreeObject(t, repo, newRoot)
	for _, e := range root.Entries {
		if e.Name == "b7" && e.Hash != shardB {
			t.Errorf("b7 shard tree hash changed: got %s, want %s", e.Hash, shardB)
		}
	}

	files := flattenTreeHelper(t, repo, newRoot, "")
	if files["a3/ckpt1/file.txt"] != blobNew {
		t.Error("a3/ckpt1/file.txt should have new hash")
	}
	if files["b7/ckpt2/file.txt"] != blobB {
		t.Error("b7/ckpt2/file.txt should be preserved")
	}
}

func TestUpdateSubtree_EmptyPathSegments(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blob := storeBlob(t, repo, "root-level")
	entries := []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blob},
	}

	// Empty path means we're at the root — should just build the leaf tree
	hash, err := UpdateSubtree(repo, plumbing.ZeroHash, nil, entries, UpdateSubtreeOptions{})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	tree := mustTreeObject(t, repo, hash)
	if len(tree.Entries) != 1 || tree.Entries[0].Name != "file.txt" {
		t.Errorf("unexpected entries: %v", tree.Entries)
	}
}

func TestUpdateSubtree_FileToDirectoryCollision(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobFile := storeBlob(t, repo, "i-am-a-file")
	blobNew := storeBlob(t, repo, "new-content")

	// Build a tree where "a3" is a regular file (not a directory)
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "a3", Mode: filemode.Regular, Hash: blobFile},
	})

	// UpdateSubtree should replace the file "a3" with a directory "a3/ckpt/"
	newRoot, err := UpdateSubtree(repo, rootTree, []string{"a3", "ckpt"}, []object.TreeEntry{
		{Name: "data.json", Mode: filemode.Regular, Hash: blobNew},
	}, UpdateSubtreeOptions{})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	// "a3" should now be a directory containing ckpt/data.json
	files := flattenTreeHelper(t, repo, newRoot, "")
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if files["a3/ckpt/data.json"] != blobNew {
		t.Error("a3/ckpt/data.json not found or wrong hash")
	}

	// Verify "a3" is now a directory, not a file
	root := mustTreeObject(t, repo, newRoot)
	for _, e := range root.Entries {
		if e.Name == "a3" && e.Mode != filemode.Dir {
			t.Errorf("a3 should be a directory, got mode %v", e.Mode)
		}
	}
}

func TestApplyTreeChanges_Empty(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blob := storeBlob(t, repo, "content")
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "file.txt", Mode: filemode.Regular, Hash: blob},
	})

	// No changes should return the same hash
	result, err := ApplyTreeChanges(repo, rootTree, nil)
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}
	if result != rootTree {
		t.Errorf("expected same hash for no changes, got %s want %s", result, rootTree)
	}
}

func TestApplyTreeChanges_AddFile(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blob1 := storeBlob(t, repo, "existing")
	blob2 := storeBlob(t, repo, "new-file")
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "existing.txt", Mode: filemode.Regular, Hash: blob1},
	})

	result, err := ApplyTreeChanges(repo, rootTree, []TreeChange{
		{Path: "new.txt", Entry: &object.TreeEntry{
			Name: "new.txt", Mode: filemode.Regular, Hash: blob2,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, result, "")
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files["existing.txt"] != blob1 {
		t.Error("existing.txt should be preserved")
	}
	if files["new.txt"] != blob2 {
		t.Error("new.txt should be added")
	}
}

func TestApplyTreeChanges_DeleteFile(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blob1 := storeBlob(t, repo, "keep")
	blob2 := storeBlob(t, repo, "delete-me")
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "delete.txt", Mode: filemode.Regular, Hash: blob2},
		{Name: "keep.txt", Mode: filemode.Regular, Hash: blob1},
	})

	result, err := ApplyTreeChanges(repo, rootTree, []TreeChange{
		{Path: "delete.txt", Entry: nil}, // nil Entry means delete
	})
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, result, "")
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if files["keep.txt"] != blob1 {
		t.Error("keep.txt should be preserved")
	}
}

func TestApplyTreeChanges_ModifyNestedFile(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobOld := storeBlob(t, repo, "old-content")
	blobNew := storeBlob(t, repo, "new-content")
	blobSibling := storeBlob(t, repo, "sibling")

	// Build: src/ -> {handler.go: blobOld}, README.md: blobSibling
	srcTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "handler.go", Mode: filemode.Regular, Hash: blobOld},
	})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "README.md", Mode: filemode.Regular, Hash: blobSibling},
		{Name: "src", Mode: filemode.Dir, Hash: srcTree},
	})

	// Modify src/handler.go
	result, err := ApplyTreeChanges(repo, rootTree, []TreeChange{
		{Path: "src/handler.go", Entry: &object.TreeEntry{
			Name: "handler.go", Mode: filemode.Regular, Hash: blobNew,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, result, "")
	if files["src/handler.go"] != blobNew {
		t.Error("src/handler.go should have new content")
	}
	if files["README.md"] != blobSibling {
		t.Error("README.md should be preserved")
	}
}

func TestApplyTreeChanges_MultipleDirectories(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobA := storeBlob(t, repo, "file-a")
	blobB := storeBlob(t, repo, "file-b")
	blobC := storeBlob(t, repo, "file-c")
	blobNew := storeBlob(t, repo, "new")

	// Build: dir1/a.txt, dir2/b.txt, dir3/c.txt
	t1 := mustStoreTree(t, repo, []object.TreeEntry{{Name: "a.txt", Mode: filemode.Regular, Hash: blobA}})
	t2 := mustStoreTree(t, repo, []object.TreeEntry{{Name: "b.txt", Mode: filemode.Regular, Hash: blobB}})
	t3 := mustStoreTree(t, repo, []object.TreeEntry{{Name: "c.txt", Mode: filemode.Regular, Hash: blobC}})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "dir1", Mode: filemode.Dir, Hash: t1},
		{Name: "dir2", Mode: filemode.Dir, Hash: t2},
		{Name: "dir3", Mode: filemode.Dir, Hash: t3},
	})

	// Modify dir1/a.txt and dir3/c.txt, leave dir2 untouched
	result, err := ApplyTreeChanges(repo, rootTree, []TreeChange{
		{Path: "dir1/a.txt", Entry: &object.TreeEntry{
			Name: "a.txt", Mode: filemode.Regular, Hash: blobNew,
		}},
		{Path: "dir3/c.txt", Entry: &object.TreeEntry{
			Name: "c.txt", Mode: filemode.Regular, Hash: blobNew,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}

	// Verify dir2's tree hash is preserved
	root := mustTreeObject(t, repo, result)
	for _, e := range root.Entries {
		if e.Name == "dir2" && e.Hash != t2 {
			t.Error("dir2 subtree hash changed — should be preserved since no changes")
		}
	}

	files := flattenTreeHelper(t, repo, result, "")
	if files["dir1/a.txt"] != blobNew {
		t.Error("dir1/a.txt should have new content")
	}
	if files["dir2/b.txt"] != blobB {
		t.Error("dir2/b.txt should be preserved")
	}
	if files["dir3/c.txt"] != blobNew {
		t.Error("dir3/c.txt should have new content")
	}
}

func TestApplyTreeChanges_CreateNestedFromEmpty(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blob := storeBlob(t, repo, "deep-content")

	// Start from empty tree
	result, err := ApplyTreeChanges(repo, plumbing.ZeroHash, []TreeChange{
		{Path: "a/b/c/file.txt", Entry: &object.TreeEntry{
			Name: "file.txt", Mode: filemode.Regular, Hash: blob,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, result, "")
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if files["a/b/c/file.txt"] != blob {
		t.Error("deeply nested file not found")
	}
}

func TestApplyTreeChanges_MixedOperations(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	blobKeep := storeBlob(t, repo, "keep")
	blobDelete := storeBlob(t, repo, "delete")
	blobOld := storeBlob(t, repo, "old")
	blobNew := storeBlob(t, repo, "new")
	blobAdd := storeBlob(t, repo, "added")

	// Build: keep.txt, delete.txt, modify.txt
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "delete.txt", Mode: filemode.Regular, Hash: blobDelete},
		{Name: "keep.txt", Mode: filemode.Regular, Hash: blobKeep},
		{Name: "modify.txt", Mode: filemode.Regular, Hash: blobOld},
	})

	result, err := ApplyTreeChanges(repo, rootTree, []TreeChange{
		// Delete
		{Path: "delete.txt", Entry: nil},
		// Modify
		{Path: "modify.txt", Entry: &object.TreeEntry{
			Name: "modify.txt", Mode: filemode.Regular, Hash: blobNew,
		}},
		// Add
		{Path: "added.txt", Entry: &object.TreeEntry{
			Name: "added.txt", Mode: filemode.Regular, Hash: blobAdd,
		}},
	})
	if err != nil {
		t.Fatalf("ApplyTreeChanges() error = %v", err)
	}

	files := flattenTreeHelper(t, repo, result, "")
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(files), files)
	}
	if files["keep.txt"] != blobKeep {
		t.Error("keep.txt wrong")
	}
	if files["modify.txt"] != blobNew {
		t.Error("modify.txt should be updated")
	}
	if files["added.txt"] != blobAdd {
		t.Error("added.txt should be added")
	}
	if _, ok := files["delete.txt"]; ok {
		t.Error("delete.txt should be deleted")
	}
}

// TestUpdateSubtree_EquivalenceWithFlattenRebuild verifies that UpdateSubtree
// produces the same result as the old FlattenTree + modify + BuildTreeFromEntries approach.
func TestUpdateSubtree_EquivalenceWithFlattenRebuild(t *testing.T) {
	t.Parallel()
	repo := mustInitBareRepo(t)

	// Build a realistic sharded tree with multiple checkpoints
	blobs := make([]plumbing.Hash, 5)
	for i := range blobs {
		blobs[i] = storeBlob(t, repo, fmt.Sprintf("content-%d", i))
	}

	// Build: a3/ckpt1/{meta.json, full.jsonl}, a3/ckpt2/{meta.json}, b7/ckpt3/{meta.json}
	ckpt1 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "full.jsonl", Mode: filemode.Regular, Hash: blobs[1]},
		{Name: "meta.json", Mode: filemode.Regular, Hash: blobs[0]},
	})
	ckpt2 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "meta.json", Mode: filemode.Regular, Hash: blobs[2]},
	})
	ckpt3 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "meta.json", Mode: filemode.Regular, Hash: blobs[3]},
	})
	shardA3 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "ckpt1", Mode: filemode.Dir, Hash: ckpt1},
		{Name: "ckpt2", Mode: filemode.Dir, Hash: ckpt2},
	})
	shardB7 := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "ckpt3", Mode: filemode.Dir, Hash: ckpt3},
	})
	rootTree := mustStoreTree(t, repo, []object.TreeEntry{
		{Name: "a3", Mode: filemode.Dir, Hash: shardA3},
		{Name: "b7", Mode: filemode.Dir, Hash: shardB7},
	})

	// Now add a new checkpoint at a3/newckpt/ using tree surgery
	newBlob := storeBlob(t, repo, "new-checkpoint")
	surgeryResult, err := UpdateSubtree(repo, rootTree, []string{"a3", "newckpt"}, []object.TreeEntry{
		{Name: "meta.json", Mode: filemode.Regular, Hash: newBlob},
	}, UpdateSubtreeOptions{})
	if err != nil {
		t.Fatalf("UpdateSubtree() error = %v", err)
	}

	// Do the same with FlattenTree + BuildTreeFromEntries
	tree := mustTreeObject(t, repo, rootTree)
	flatEntries := make(map[string]object.TreeEntry)
	if err := FlattenTree(repo, tree, "", flatEntries); err != nil {
		t.Fatalf("FlattenTree() error = %v", err)
	}
	flatEntries["a3/newckpt/meta.json"] = object.TreeEntry{
		Name: "meta.json",
		Mode: filemode.Regular,
		Hash: newBlob,
	}
	flatResult, err := BuildTreeFromEntries(repo, flatEntries)
	if err != nil {
		t.Fatalf("BuildTreeFromEntries() error = %v", err)
	}

	// Both approaches should produce the exact same tree hash
	if surgeryResult != flatResult {
		t.Errorf("tree surgery result (%s) != flatten+rebuild result (%s)", surgeryResult, flatResult)

		// Debug: show both trees
		surgeryFiles := flattenTreeHelper(t, repo, surgeryResult, "")
		flatFiles := flattenTreeHelper(t, repo, flatResult, "")
		t.Logf("Surgery files: %v", surgeryFiles)
		t.Logf("Flat files: %v", flatFiles)
	}
}
