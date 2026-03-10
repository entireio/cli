package benchutil

import (
	"fmt"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// --- Helpers ---

// benchInitBareRepo creates a bare git repo for benchmarks.
func benchInitBareRepo(b *testing.B) *gogit.Repository {
	b.Helper()
	repo, err := gogit.PlainInit(b.TempDir(), true)
	if err != nil {
		b.Fatalf("git init: %v", err)
	}
	return repo
}

// benchCreateBlob stores a blob in the repo.
func benchCreateBlob(b *testing.B, repo *gogit.Repository, content string) plumbing.Hash {
	b.Helper()
	hash, err := checkpoint.CreateBlobFromContent(repo, []byte(content))
	if err != nil {
		b.Fatalf("create blob: %v", err)
	}
	return hash
}

// buildShardedMetadataTree builds a realistic metadata branch tree with N checkpoints
// distributed across shards. Each checkpoint has 4 files (metadata.json, full.jsonl,
// prompt.txt, content_hash.txt) matching the real storage format.
//
// Returns the root tree hash.
func buildShardedMetadataTree(b *testing.B, repo *gogit.Repository, checkpointCount int) plumbing.Hash {
	b.Helper()

	entries := make(map[string]object.TreeEntry, checkpointCount*4)
	for i := range checkpointCount {
		// Generate a fake checkpoint ID (12 hex chars) spread across shards
		cpID := fmt.Sprintf("%02x%010d", i%256, i)
		shard := cpID[:2]
		suffix := cpID[2:]
		base := shard + "/" + suffix + "/"

		for _, file := range []struct {
			name    string
			content string
		}{
			{"metadata.json", fmt.Sprintf(`{"checkpoint_id":"%s","session_count":1}`, cpID)},
			{"0/metadata.json", fmt.Sprintf(`{"checkpoint_id":"%s","session_id":"s-%d"}`, cpID, i)},
			{"0/full.jsonl", fmt.Sprintf(`{"type":"assistant","content":"checkpoint %d data padding %0200d"}`, i, i)},
			{"0/prompt.txt", fmt.Sprintf("Implement feature %d", i)},
			{"0/content_hash.txt", fmt.Sprintf("sha256:%064x", i)},
		} {
			blob, err := checkpoint.CreateBlobFromContent(repo, []byte(file.content))
			if err != nil {
				b.Fatalf("create blob: %v", err)
			}
			path := base + file.name
			entries[path] = object.TreeEntry{
				Name: file.name,
				Mode: filemode.Regular,
				Hash: blob,
			}
		}
	}

	hash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		b.Fatalf("build tree: %v", err)
	}
	return hash
}

// buildFlatFileTree builds a tree with N files in a directory structure
// resembling a real working tree (files under src/, pkg/, etc.).
func buildFlatFileTree(b *testing.B, repo *gogit.Repository, fileCount int) plumbing.Hash {
	b.Helper()

	entries := make(map[string]object.TreeEntry, fileCount)
	dirs := []string{"src", "pkg", "internal", "cmd", "api"}
	for i := range fileCount {
		dir := dirs[i%len(dirs)]
		name := fmt.Sprintf("%s/file_%04d.go", dir, i)
		blob := benchCreateBlob(b, repo, fmt.Sprintf("package main\n// file %d\nfunc f%d() {}\n", i, i))
		entries[name] = object.TreeEntry{
			Name: fmt.Sprintf("file_%04d.go", i),
			Mode: filemode.Regular,
			Hash: blob,
		}
	}

	hash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	if err != nil {
		b.Fatalf("build tree: %v", err)
	}
	return hash
}

// --- UpdateSubtree benchmarks (metadata branch path) ---
// The key dimension is number of prior checkpoints, since UpdateSubtree is O(depth)
// while FlattenTree+BuildTreeFromEntries is O(total checkpoints).

func BenchmarkUpdateSubtree(b *testing.B) {
	for _, count := range []int{1, 10, 50, 200, 500} {
		b.Run(fmt.Sprintf("Checkpoints_%d/TreeSurgery", count), benchUpdateSubtreeTreeSurgery(count))
		b.Run(fmt.Sprintf("Checkpoints_%d/FlattenRebuild", count), benchUpdateSubtreeFlattenRebuild(count))
	}
}

// benchUpdateSubtreeTreeSurgery benchmarks adding a checkpoint to a metadata tree
// using UpdateSubtree (O(depth) tree surgery).
func benchUpdateSubtreeTreeSurgery(priorCheckpoints int) func(*testing.B) {
	return func(b *testing.B) {
		repo := benchInitBareRepo(b)
		rootTree := buildShardedMetadataTree(b, repo, priorCheckpoints)

		// Prepare the new checkpoint data (5 files)
		newBlobs := make([]plumbing.Hash, 5)
		for i := range newBlobs {
			newBlobs[i] = benchCreateBlob(b, repo, fmt.Sprintf("new checkpoint file %d content", i))
		}

		// Build the checkpoint subtree once
		cpEntries := make(map[string]object.TreeEntry, 5)
		for i, name := range []string{"metadata.json", "0/metadata.json", "0/full.jsonl", "0/prompt.txt", "0/content_hash.txt"} {
			cpEntries[name] = object.TreeEntry{
				Name: name,
				Mode: filemode.Regular,
				Hash: newBlobs[i],
			}
		}
		cpTreeHash, err := checkpoint.BuildTreeFromEntries(repo, cpEntries)
		if err != nil {
			b.Fatalf("build checkpoint tree: %v", err)
		}

		shardPrefix := "ff" // Use a shard that likely doesn't exist yet
		shardSuffix := "newckpt1234"

		b.ResetTimer()
		for range b.N {
			_, err := checkpoint.UpdateSubtree(repo, rootTree, []string{shardPrefix}, []object.TreeEntry{
				{Name: shardSuffix, Mode: filemode.Dir, Hash: cpTreeHash},
			}, checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting})
			if err != nil {
				b.Fatalf("UpdateSubtree: %v", err)
			}
		}
	}
}

// benchUpdateSubtreeFlattenRebuild benchmarks the old approach: flatten entire tree,
// add new entries, rebuild from scratch. O(total checkpoints).
func benchUpdateSubtreeFlattenRebuild(priorCheckpoints int) func(*testing.B) {
	return func(b *testing.B) {
		repo := benchInitBareRepo(b)
		rootTree := buildShardedMetadataTree(b, repo, priorCheckpoints)

		// Prepare the new checkpoint entries
		newBlobs := make([]plumbing.Hash, 5)
		for i := range newBlobs {
			newBlobs[i] = benchCreateBlob(b, repo, fmt.Sprintf("new checkpoint file %d content", i))
		}
		newFiles := []struct {
			path string
			hash plumbing.Hash
		}{
			{"ff/newckpt1234/metadata.json", newBlobs[0]},
			{"ff/newckpt1234/0/metadata.json", newBlobs[1]},
			{"ff/newckpt1234/0/full.jsonl", newBlobs[2]},
			{"ff/newckpt1234/0/prompt.txt", newBlobs[3]},
			{"ff/newckpt1234/0/content_hash.txt", newBlobs[4]},
		}

		b.ResetTimer()
		for range b.N {
			// Flatten entire tree
			tree, err := repo.TreeObject(rootTree)
			if err != nil {
				b.Fatalf("read tree: %v", err)
			}
			entries := make(map[string]object.TreeEntry)
			if err := checkpoint.FlattenTree(repo, tree, "", entries); err != nil {
				b.Fatalf("FlattenTree: %v", err)
			}

			// Add new entries
			for _, f := range newFiles {
				entries[f.path] = object.TreeEntry{
					Name: f.path,
					Mode: filemode.Regular,
					Hash: f.hash,
				}
			}

			// Rebuild entire tree
			_, err = checkpoint.BuildTreeFromEntries(repo, entries)
			if err != nil {
				b.Fatalf("BuildTreeFromEntries: %v", err)
			}
		}
	}
}

// --- ApplyTreeChanges benchmarks (working tree / shadow branch path) ---
// Two dimensions: base tree size (total files) and number of changes.

func BenchmarkApplyTreeChanges(b *testing.B) {
	// Dimension 1: Varying base tree size with fixed number of changes
	for _, fileCount := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("Files_%d_Changes_3/TreeSurgery", fileCount), benchApplyTreeChangesTreeSurgery(fileCount, 3))
		b.Run(fmt.Sprintf("Files_%d_Changes_3/FlattenRebuild", fileCount), benchApplyTreeChangesFlattenRebuild(fileCount, 3))
	}

	// Dimension 2: Varying number of changes with fixed base tree size
	for _, changeCount := range []int{1, 5, 20, 50} {
		b.Run(fmt.Sprintf("Files_200_Changes_%d/TreeSurgery", changeCount), benchApplyTreeChangesTreeSurgery(200, changeCount))
		b.Run(fmt.Sprintf("Files_200_Changes_%d/FlattenRebuild", changeCount), benchApplyTreeChangesFlattenRebuild(200, changeCount))
	}
}

// benchApplyTreeChangesTreeSurgery benchmarks modifying files in a tree
// using ApplyTreeChanges (only touches affected subtrees).
func benchApplyTreeChangesTreeSurgery(fileCount, changeCount int) func(*testing.B) {
	return func(b *testing.B) {
		repo := benchInitBareRepo(b)
		rootTree := buildFlatFileTree(b, repo, fileCount)

		// Prepare changes: modify files spread across directories
		changes := make([]checkpoint.TreeChange, 0, changeCount)
		dirs := []string{"src", "pkg", "internal", "cmd", "api"}
		for i := range changeCount {
			dir := dirs[i%len(dirs)]
			fileIdx := i % fileCount
			path := fmt.Sprintf("%s/file_%04d.go", dir, fileIdx)
			newBlob := benchCreateBlob(b, repo, fmt.Sprintf("modified content %d\n", i))
			changes = append(changes, checkpoint.TreeChange{
				Path: path,
				Entry: &object.TreeEntry{
					Name: fmt.Sprintf("file_%04d.go", fileIdx),
					Mode: filemode.Regular,
					Hash: newBlob,
				},
			})
		}

		b.ResetTimer()
		for range b.N {
			_, err := checkpoint.ApplyTreeChanges(repo, rootTree, changes)
			if err != nil {
				b.Fatalf("ApplyTreeChanges: %v", err)
			}
		}
	}
}

// benchApplyTreeChangesFlattenRebuild benchmarks the old approach for working tree
// modifications: flatten, modify, rebuild.
func benchApplyTreeChangesFlattenRebuild(fileCount, changeCount int) func(*testing.B) {
	return func(b *testing.B) {
		repo := benchInitBareRepo(b)
		rootTree := buildFlatFileTree(b, repo, fileCount)

		// Prepare changes
		type change struct {
			path string
			hash plumbing.Hash
		}
		changes := make([]change, 0, changeCount)
		dirs := []string{"src", "pkg", "internal", "cmd", "api"}
		for i := range changeCount {
			dir := dirs[i%len(dirs)]
			fileIdx := i % fileCount
			path := fmt.Sprintf("%s/file_%04d.go", dir, fileIdx)
			newBlob := benchCreateBlob(b, repo, fmt.Sprintf("modified content %d\n", i))
			changes = append(changes, change{path: path, hash: newBlob})
		}

		b.ResetTimer()
		for range b.N {
			// Flatten entire tree
			tree, err := repo.TreeObject(rootTree)
			if err != nil {
				b.Fatalf("read tree: %v", err)
			}
			entries := make(map[string]object.TreeEntry)
			if err := checkpoint.FlattenTree(repo, tree, "", entries); err != nil {
				b.Fatalf("FlattenTree: %v", err)
			}

			// Apply changes
			for _, c := range changes {
				entries[c.path] = object.TreeEntry{
					Name: c.path,
					Mode: filemode.Regular,
					Hash: c.hash,
				}
			}

			// Rebuild
			_, err = checkpoint.BuildTreeFromEntries(repo, entries)
			if err != nil {
				b.Fatalf("BuildTreeFromEntries: %v", err)
			}
		}
	}
}
