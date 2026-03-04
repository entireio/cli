package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFilesOverlapWithContent_ModifiedFile tests that a modified file (exists in parent)
// counts as overlap regardless of content changes.
func TestFilesOverlapWithContent_ModifiedFile(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create initial file and commit
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("original content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Create shadow branch with same file content as session created
	sessionContent := []byte("session modified content")
	createShadowBranchWithContent(t, repo, "abc1234", "e3b0c4", map[string][]byte{
		"test.txt": sessionContent,
	})

	// Modify the file with DIFFERENT content (user edited session's work)
	require.NoError(t, os.WriteFile(testFile, []byte("user modified further"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Modify file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Get HEAD commit
	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	// Test: Modified file should count as overlap even with different content
	shadowBranch := checkpoint.ShadowBranchNameForCommit("abc1234", "e3b0c4")
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"test.txt"})
	assert.True(t, result, "Modified file should count as overlap (user edited session's work)")
}

// TestFilesOverlapWithContent_NewFile_ContentMatch tests that a new file with
// matching content counts as overlap.
func TestFilesOverlapWithContent_NewFile_ContentMatch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create shadow branch with a new file
	originalContent := []byte("session created this content")
	createShadowBranchWithContent(t, repo, "def5678", "e3b0c4", map[string][]byte{
		"newfile.txt": originalContent,
	})

	// Commit the same file with SAME content (user commits session's work unchanged)
	testFile := filepath.Join(dir, "newfile.txt")
	require.NoError(t, os.WriteFile(testFile, originalContent, 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("newfile.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Add new file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	// Test: New file with matching content should count as overlap
	shadowBranch := checkpoint.ShadowBranchNameForCommit("def5678", "e3b0c4")
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"newfile.txt"})
	assert.True(t, result, "New file with matching content should count as overlap")
}

// TestFilesOverlapWithContent_NewFile_ContentMismatch tests that a new file with
// completely different content does NOT count as overlap (reverted & replaced scenario).
func TestFilesOverlapWithContent_NewFile_ContentMismatch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create shadow branch with a file
	sessionContent := []byte("session created this")
	createShadowBranchWithContent(t, repo, "ghi9012", "e3b0c4", map[string][]byte{
		"replaced.txt": sessionContent,
	})

	// Commit a file with COMPLETELY DIFFERENT content (user reverted & replaced)
	testFile := filepath.Join(dir, "replaced.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("user wrote something totally unrelated"), 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("replaced.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Add replaced file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	// Test: New file with different content should NOT count as overlap
	shadowBranch := checkpoint.ShadowBranchNameForCommit("ghi9012", "e3b0c4")
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"replaced.txt"})
	assert.False(t, result, "New file with different content should NOT count as overlap (reverted & replaced)")
}

// TestFilesOverlapWithContent_FileNotInCommit tests that a file in filesTouched
// but not in the commit doesn't count as overlap.
func TestFilesOverlapWithContent_FileNotInCommit(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create shadow branch with files
	fileAContent := []byte("file A content")
	fileBContent := []byte("file B content")
	createShadowBranchWithContent(t, repo, "jkl3456", "e3b0c4", map[string][]byte{
		"fileA.txt": fileAContent,
		"fileB.txt": fileBContent,
	})

	// Only commit fileA (not fileB)
	fileA := filepath.Join(dir, "fileA.txt")
	require.NoError(t, os.WriteFile(fileA, fileAContent, 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("fileA.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Add only file A", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	// Test: Only fileB in filesTouched, which is not in commit
	shadowBranch := checkpoint.ShadowBranchNameForCommit("jkl3456", "e3b0c4")
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"fileB.txt"})
	assert.False(t, result, "File not in commit should not count as overlap")

	// Test: fileA in filesTouched and in commit - should overlap (new file with matching content)
	result = filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"fileA.txt"})
	assert.True(t, result, "File in commit with matching content should count as overlap")
}

// TestFilesOverlapWithContent_DeletedFile tests that a deleted file
// (existed in parent, not in HEAD) DOES count as overlap.
// The agent's action of deleting the file is being committed.
func TestFilesOverlapWithContent_DeletedFile(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create and commit a file that will be deleted
	toDelete := filepath.Join(dir, "to_delete.txt")
	require.NoError(t, os.WriteFile(toDelete, []byte("content to delete"), 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("to_delete.txt")
	require.NoError(t, err)
	_, err = wt.Commit("Add file to delete", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Create shadow branch (simulating agent work that includes the deletion)
	createShadowBranchWithContent(t, repo, "del1234", "e3b0c4", map[string][]byte{
		"other.txt": []byte("other content"),
	})

	// Delete the file and commit the deletion
	_, err = wt.Remove("to_delete.txt")
	require.NoError(t, err)
	deleteCommit, err := wt.Commit("Delete file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(deleteCommit)
	require.NoError(t, err)

	// Test: deleted file in filesTouched should count as overlap
	shadowBranch := checkpoint.ShadowBranchNameForCommit("del1234", "e3b0c4")
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"to_delete.txt"})
	assert.True(t, result, "Deleted file should count as overlap (agent's deletion being committed)")
}

// TestFilesOverlapWithContent_NoShadowBranch tests fallback when shadow branch doesn't exist.
func TestFilesOverlapWithContent_NoShadowBranch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a commit without any shadow branch
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Test commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	// Test: Non-existent shadow branch should fall back to assuming overlap
	result := filesOverlapWithContent(context.Background(), repo, "entire/nonexistent-e3b0c4", commit, []string{"test.txt"})
	assert.True(t, result, "Missing shadow branch should fall back to assuming overlap")
}

// TestFilesWithRemainingAgentChanges_FileNotCommitted tests that files not in the commit
// are kept in the remaining list.
func TestFilesWithRemainingAgentChanges_FileNotCommitted(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create shadow branch with two files
	createShadowBranchWithContent(t, repo, "abc1234", "e3b0c4", map[string][]byte{
		"fileA.txt": []byte("content A"),
		"fileB.txt": []byte("content B"),
	})

	// Only commit fileA
	fileA := filepath.Join(dir, "fileA.txt")
	require.NoError(t, os.WriteFile(fileA, []byte("content A"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("fileA.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Add file A only", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("abc1234", "e3b0c4")
	committedFiles := map[string]struct{}{"fileA.txt": {}}

	// fileB was not committed - should be in remaining
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit, []string{"fileA.txt", "fileB.txt"}, committedFiles)
	assert.Equal(t, []string{"fileB.txt"}, remaining, "Uncommitted file should be in remaining")
}

// TestFilesWithRemainingAgentChanges_FullyCommitted tests that files committed with
// matching content are NOT in the remaining list.
func TestFilesWithRemainingAgentChanges_FullyCommitted(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	content := []byte("exact same content")

	// Create shadow branch with file
	createShadowBranchWithContent(t, repo, "def5678", "e3b0c4", map[string][]byte{
		"test.txt": content,
	})

	// Commit the file with SAME content
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, content, 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Add file with same content", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("def5678", "e3b0c4")
	committedFiles := map[string]struct{}{"test.txt": {}}

	// File was fully committed - should NOT be in remaining
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit, []string{"test.txt"}, committedFiles)
	assert.Empty(t, remaining, "Fully committed file should not be in remaining")
}

// TestFilesWithRemainingAgentChanges_PartialCommit tests that files committed with
// different content (partial commit via git add -p) ARE in the remaining list
// when the working tree still has the full agent content.
func TestFilesWithRemainingAgentChanges_PartialCommit(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Shadow branch has the full agent content
	fullContent := []byte("line 1\nline 2\nline 3\nline 4\n")
	createShadowBranchWithContent(t, repo, "ghi9012", "e3b0c4", map[string][]byte{
		"test.txt": fullContent,
	})

	// User commits only partial content (simulating git add -p)
	partialContent := []byte("line 1\nline 2\n")
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, partialContent, 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Partial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// After a real git add -p, the working tree still has the full content.
	// Simulate this by writing the full content back to disk after the commit.
	require.NoError(t, os.WriteFile(testFile, fullContent, 0o644))

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("ghi9012", "e3b0c4")
	committedFiles := map[string]struct{}{"test.txt": {}}

	// Content doesn't match and working tree is dirty - file should be in remaining
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit, []string{"test.txt"}, committedFiles)
	assert.Equal(t, []string{"test.txt"}, remaining, "Partially committed file with dirty working tree should be in remaining")
}

// TestFilesWithRemainingAgentChanges_ReplacedContent tests that files committed with
// different content but a CLEAN working tree are NOT in the remaining list.
// This is the scenario where the user intentionally replaced the agent's content.
func TestFilesWithRemainingAgentChanges_ReplacedContent(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Shadow branch has the agent's content
	agentContent := []byte("func GetPort() int { return 8080 }\n")
	createShadowBranchWithContent(t, repo, "rep1234", "e3b0c4", map[string][]byte{
		"config.go": agentContent,
	})

	// User writes completely different content and commits
	userContent := []byte("func GetHost() string { return \"localhost\" }\n")
	testFile := filepath.Join(dir, "config.go")
	require.NoError(t, os.WriteFile(testFile, userContent, 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("config.go")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Replace config", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Working tree is clean — matches the commit (user committed everything)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("rep1234", "e3b0c4")
	committedFiles := map[string]struct{}{"config.go": {}}

	// Content differs from shadow but working tree is clean — no carry-forward
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit, []string{"config.go"}, committedFiles)
	assert.Empty(t, remaining, "Replaced content with clean working tree should not be in remaining")
}

// TestFilesWithRemainingAgentChanges_NoShadowBranch tests fallback to file-level subtraction.
func TestFilesWithRemainingAgentChanges_NoShadowBranch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a commit without any shadow branch
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Test commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	// Non-existent shadow branch should fall back to file-level subtraction
	committedFiles := map[string]struct{}{"test.txt": {}}
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, "entire/nonexistent-e3b0c4", commit, []string{"test.txt", "other.txt"}, committedFiles)

	// With file-level subtraction: test.txt is in committedFiles, other.txt is not
	assert.Equal(t, []string{"other.txt"}, remaining, "Fallback should use file-level subtraction")
}

// resolveCommitTrees is a test helper that resolves HEAD tree, parent tree, and
// shadow tree from a commit and shadow branch. Used to test cache equivalence.
func resolveCommitTrees(t *testing.T, repo *git.Repository, commit *object.Commit, shadowBranchName string) (headTree, parentTree, shadowTree *object.Tree) {
	t.Helper()

	var err error
	headTree, err = commit.Tree()
	require.NoError(t, err)

	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		require.NoError(t, err)
		parentTree, err = parent.Tree()
		require.NoError(t, err)
	}

	refName := plumbing.NewBranchReferenceName(shadowBranchName)
	shadowRef, err := repo.Reference(refName, true)
	if err == nil {
		shadowCommit, err := repo.CommitObject(shadowRef.Hash())
		require.NoError(t, err)
		shadowTree, err = shadowCommit.Tree()
		require.NoError(t, err)
	}

	return headTree, parentTree, shadowTree
}

// TestFilesOverlapWithContent_CacheEquivalence verifies that calling
// filesOverlapWithContent with pre-resolved trees (cache hit) produces
// the same result as calling without opts (cache miss / fallback).
func TestFilesOverlapWithContent_CacheEquivalence(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create initial file and commit
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("original content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	_, err = wt.Commit("parent commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Create shadow branch
	createShadowBranchWithContent(t, repo, "abc1234", "e3b0c4", map[string][]byte{
		"test.txt": []byte("session modified"),
	})

	// Modify file and commit
	require.NoError(t, os.WriteFile(testFile, []byte("user modified"), 0o644))
	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	headHash, err := wt.Commit("user commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headHash)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("abc1234", "e3b0c4")
	headTree, parentTree, shadowTree := resolveCommitTrees(t, repo, commit, shadowBranch)

	// Cache miss (no opts)
	resultWithout := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"test.txt"})

	// Cache hit (all trees pre-resolved)
	resultWith := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"test.txt"}, overlapOpts{
		headTree:      headTree,
		shadowTree:    shadowTree,
		parentTree:    parentTree,
		hasParentTree: true,
	})

	assert.Equal(t, resultWithout, resultWith, "Cache hit and cache miss should produce the same result")
	assert.True(t, resultWith, "Modified file should count as overlap")
}

// TestFilesOverlapWithContent_PartialCache verifies correct behavior when only
// some trees are pre-resolved (e.g., headTree cached but shadowTree nil).
func TestFilesOverlapWithContent_PartialCache(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create shadow branch with new file
	content := []byte("session content")
	createShadowBranchWithContent(t, repo, "part1234", "e3b0c4", map[string][]byte{
		"newfile.txt": content,
	})

	// Commit same file with same content
	testFile := filepath.Join(dir, "newfile.txt")
	require.NoError(t, os.WriteFile(testFile, content, 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("newfile.txt")
	require.NoError(t, err)
	headHash, err := wt.Commit("add new file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headHash)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("part1234", "e3b0c4")
	headTree, parentTree, _ := resolveCommitTrees(t, repo, commit, shadowBranch)

	// Partial cache: headTree and parentTree provided, shadowTree nil (will be resolved from repo)
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"newfile.txt"}, overlapOpts{
		headTree:      headTree,
		parentTree:    parentTree,
		hasParentTree: true,
		// shadowTree intentionally nil — triggers fallback resolution
	})

	assert.True(t, result, "Partial cache (headTree only) should still detect overlap")
}

// TestFilesOverlapWithContent_CacheWithInitialCommit verifies cache behavior
// when parentTree is nil (initial commit / no parent).
func TestFilesOverlapWithContent_CacheWithInitialCommit(t *testing.T) {
	t.Parallel()
	// setupGitRepo creates one initial commit (no parent), so HEAD has NumParents() == 0
	dir := setupGitRepo(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	require.Equal(t, 0, commit.NumParents(), "setupGitRepo should create an initial commit")

	// Create shadow branch with content matching the initial commit's file
	createShadowBranchWithContent(t, repo, "init123", "e3b0c4", map[string][]byte{
		"test.txt": []byte("initial content"),
	})

	shadowBranch := checkpoint.ShadowBranchNameForCommit("init123", "e3b0c4")
	headTree, err := commit.Tree()
	require.NoError(t, err)

	// Cache with hasParentTree=true and parentTree=nil (initial commit has no parent)
	result := filesOverlapWithContent(context.Background(), repo, shadowBranch, commit, []string{"test.txt"}, overlapOpts{
		headTree:      headTree,
		parentTree:    nil,
		hasParentTree: true, // Explicitly resolved as nil (initial commit)
	})

	assert.True(t, result, "Initial commit with matching content should count as overlap")
}

// TestFilesWithRemainingAgentChanges_CacheEquivalence verifies that calling
// with pre-resolved trees produces the same result as without.
func TestFilesWithRemainingAgentChanges_CacheEquivalence(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create shadow branch with two files
	createShadowBranchWithContent(t, repo, "rem1234", "e3b0c4", map[string][]byte{
		"fileA.txt": []byte("agent content A"),
		"fileB.txt": []byte("agent content B"),
	})

	// Commit only fileA with matching content
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileA.txt"), []byte("agent content A"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileB.txt"), []byte("agent content B"), 0o644))
	_, err = wt.Add("fileA.txt")
	require.NoError(t, err)
	_, err = wt.Add("fileB.txt")
	require.NoError(t, err)
	headHash, err := wt.Commit("commit both files", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headHash)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("rem1234", "e3b0c4")
	headTree, _, shadowTree := resolveCommitTrees(t, repo, commit, shadowBranch)

	committedFiles := map[string]struct{}{"fileA.txt": {}}
	filesTouched := []string{"fileA.txt", "fileB.txt"}

	// Cache miss
	resultWithout := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit, filesTouched, committedFiles)

	// Cache hit
	resultWith := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit, filesTouched, committedFiles, overlapOpts{
		headTree:   headTree,
		shadowTree: shadowTree,
	})

	assert.Equal(t, resultWithout, resultWith, "Cache hit and cache miss should produce the same result")
	// fileB.txt was not committed, so it should be in remaining
	assert.Contains(t, resultWith, "fileB.txt")
	// fileA.txt was committed with matching content, so it should NOT be in remaining
	assert.NotContains(t, resultWith, "fileA.txt")
}

// TestFilesWithRemainingAgentChanges_PhantomFile tests that files tracked in
// filesTouched but not present in the shadow branch tree are skipped. This
// happens when an agent's transcript references a file path (e.g. via a
// write_file tool call) that was never actually created on disk — for example
// when Gemini tries to write src/types.go but creates src/types/types.go
// instead. Without this check, phantom files cause infinite carry-forward.
func TestFilesWithRemainingAgentChanges_PhantomFile(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Shadow branch only contains the REAL file (buildTreeWithChanges skips
	// non-existent files, so the phantom path is never in the tree).
	createShadowBranchWithContent(t, repo, "phn1234", "e3b0c4", map[string][]byte{
		"src/types/types.go": []byte("package types\n\ntype User struct{}\n"),
	})

	// Create the real file on disk and commit it.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src", "types"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "types", "types.go"),
		[]byte("package types\n\ntype User struct{}\n"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("src/types/types.go")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Add types.go", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	shadowBranch := checkpoint.ShadowBranchNameForCommit("phn1234", "e3b0c4")
	committedFiles := map[string]struct{}{"src/types/types.go": {}}

	// filesTouched includes both the real path and a phantom path.
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranch, commit,
		[]string{"src/types.go", "src/types/types.go"}, committedFiles)

	// src/types.go is not committed AND not in shadow tree → skip.
	// src/types/types.go is committed with matching content → skip.
	assert.Empty(t, remaining, "Phantom files not in shadow tree should not be carried forward")
}

// TestFilesWithRemainingAgentChanges_UncommittedDeletion verifies that an
// agent-deleted file that the user didn't commit is correctly skipped.
// The file won't be in the shadow tree (buildTreeWithChanges excludes files
// missing from disk), so the "not in shadow tree" guard handles it.
// Carrying it forward would be a no-op — buildTreeWithChanges would just
// record another deletion since there's nothing on disk to snapshot.
func TestFilesWithRemainingAgentChanges_UncommittedDeletion(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a file that the agent will "delete"
	targetFile := filepath.Join(dir, "to_delete.txt")
	require.NoError(t, os.WriteFile(targetFile, []byte("will be deleted"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("to_delete.txt")
	require.NoError(t, err)
	baseCommitHash, err := wt.Commit("Add file that agent will delete", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Build shadow branch WITHOUT to_delete.txt (agent deleted it on disk,
	// so buildTreeWithChanges excluded it from the shadow tree).
	shadowBranchName := checkpoint.ShadowBranchNameForCommit("del1234", "e3b0c4")
	refName := plumbing.NewBranchReferenceName(shadowBranchName)

	baseCommit, err := repo.CommitObject(baseCommitHash)
	require.NoError(t, err)
	baseTree, err := baseCommit.Tree()
	require.NoError(t, err)

	entries := make(map[string]object.TreeEntry)
	err = checkpoint.FlattenTree(repo, baseTree, "", entries)
	require.NoError(t, err)
	delete(entries, "to_delete.txt")

	treeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	require.NoError(t, err)

	shadowCommitObj := &object.Commit{
		Author:    object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
		Committer: object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
		Message:   "Shadow checkpoint (agent deleted to_delete.txt)",
		TreeHash:  treeHash,
	}
	encodedObj := repo.Storer.NewEncodedObject()
	err = shadowCommitObj.Encode(encodedObj)
	require.NoError(t, err)
	shadowHash, err := repo.Storer.SetEncodedObject(encodedObj)
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, shadowHash)))

	// Delete file on disk (agent did this) but user doesn't commit the deletion
	require.NoError(t, os.Remove(targetFile))

	// User commits something else
	otherFile := filepath.Join(dir, "other.txt")
	require.NoError(t, os.WriteFile(otherFile, []byte("other changes"), 0o644))
	_, err = wt.Add("other.txt")
	require.NoError(t, err)
	userCommitHash, err := wt.Commit("User commit (not including deletion)", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	userCommit, err := repo.CommitObject(userCommitHash)
	require.NoError(t, err)

	committedFiles := map[string]struct{}{"other.txt": {}}
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, shadowBranchName, userCommit,
		[]string{"to_delete.txt", "other.txt"}, committedFiles)

	// to_delete.txt is correctly skipped: it's not in the shadow tree because
	// the agent deleted it from disk. Carrying it forward would be pointless —
	// buildTreeWithChanges would just see the file is missing and record a no-op.
	assert.Empty(t, remaining, "Deleted file not in shadow tree should not be carried forward")
}

// TestStagedFilesOverlapWithContent_ModifiedFile tests that a modified file
// (exists in HEAD) always counts as overlap.
func TestStagedFilesOverlapWithContent_ModifiedFile(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Initial file is created by setupGitRepo
	// Modify it and stage
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("modified content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("test.txt")
	require.NoError(t, err)

	// Create shadow branch (content doesn't matter for modified files)
	createShadowBranchWithContent(t, repo, "abc1234", "e3b0c4", map[string][]byte{
		"test.txt": []byte("shadow content"),
	})

	// Get shadow tree
	shadowBranch := checkpoint.ShadowBranchNameForCommit("abc1234", "e3b0c4")
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	require.NoError(t, err)
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	require.NoError(t, err)
	shadowTree, err := shadowCommit.Tree()
	require.NoError(t, err)

	// Modified file should count as overlap regardless of content
	result := stagedFilesOverlapWithContent(context.Background(), repo, shadowTree, []string{"test.txt"}, []string{"test.txt"})
	assert.True(t, result, "Modified file should always count as overlap")
}

// TestStagedFilesOverlapWithContent_NewFile_ContentMatch tests that a new file
// with matching content counts as overlap.
func TestStagedFilesOverlapWithContent_NewFile_ContentMatch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a NEW file (doesn't exist in HEAD)
	content := []byte("new file content")
	newFile := filepath.Join(dir, "newfile.txt")
	require.NoError(t, os.WriteFile(newFile, content, 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("newfile.txt")
	require.NoError(t, err)

	// Create shadow branch with SAME content
	createShadowBranchWithContent(t, repo, "def5678", "e3b0c4", map[string][]byte{
		"newfile.txt": content,
	})

	// Get shadow tree
	shadowBranch := checkpoint.ShadowBranchNameForCommit("def5678", "e3b0c4")
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	require.NoError(t, err)
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	require.NoError(t, err)
	shadowTree, err := shadowCommit.Tree()
	require.NoError(t, err)

	// New file with matching content should count as overlap
	result := stagedFilesOverlapWithContent(context.Background(), repo, shadowTree, []string{"newfile.txt"}, []string{"newfile.txt"})
	assert.True(t, result, "New file with matching content should count as overlap")
}

// TestStagedFilesOverlapWithContent_NewFile_ContentMismatch tests that a new file
// with different content does NOT count as overlap (reverted & replaced scenario).
func TestStagedFilesOverlapWithContent_NewFile_ContentMismatch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a NEW file with different content than shadow branch
	newFile := filepath.Join(dir, "newfile.txt")
	require.NoError(t, os.WriteFile(newFile, []byte("user replaced content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("newfile.txt")
	require.NoError(t, err)

	// Create shadow branch with DIFFERENT content (agent's original)
	createShadowBranchWithContent(t, repo, "ghi9012", "e3b0c4", map[string][]byte{
		"newfile.txt": []byte("agent original content"),
	})

	// Get shadow tree
	shadowBranch := checkpoint.ShadowBranchNameForCommit("ghi9012", "e3b0c4")
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	require.NoError(t, err)
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	require.NoError(t, err)
	shadowTree, err := shadowCommit.Tree()
	require.NoError(t, err)

	// New file with different content should NOT count as overlap
	result := stagedFilesOverlapWithContent(context.Background(), repo, shadowTree, []string{"newfile.txt"}, []string{"newfile.txt"})
	assert.False(t, result, "New file with mismatched content should not count as overlap")
}

// TestStagedFilesOverlapWithContent_NoOverlap tests that non-overlapping files
// return false.
func TestStagedFilesOverlapWithContent_NoOverlap(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Stage a file NOT in filesTouched
	otherFile := filepath.Join(dir, "other.txt")
	require.NoError(t, os.WriteFile(otherFile, []byte("other content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("other.txt")
	require.NoError(t, err)

	// Create shadow branch
	createShadowBranchWithContent(t, repo, "jkl3456", "e3b0c4", map[string][]byte{
		"session.txt": []byte("session content"),
	})

	// Get shadow tree
	shadowBranch := checkpoint.ShadowBranchNameForCommit("jkl3456", "e3b0c4")
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	require.NoError(t, err)
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	require.NoError(t, err)
	shadowTree, err := shadowCommit.Tree()
	require.NoError(t, err)

	// Staged file "other.txt" is not in filesTouched "session.txt"
	result := stagedFilesOverlapWithContent(context.Background(), repo, shadowTree, []string{"other.txt"}, []string{"session.txt"})
	assert.False(t, result, "Non-overlapping files should return false")
}

// TestStagedFilesOverlapWithContent_DeletedFile tests that a deleted file
// (exists in HEAD but staged for deletion) DOES count as overlap.
// The agent's action of deleting the file is being committed, so the session
// context should be linked to this commit.
func TestStagedFilesOverlapWithContent_DeletedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	worktree, err := repo.Worktree()
	require.NoError(t, err)

	// Create and commit a file that will be deleted
	filePath := filepath.Join(dir, "to_delete.txt")
	err = os.WriteFile(filePath, []byte("original content"), 0644)
	require.NoError(t, err)
	_, err = worktree.Add("to_delete.txt")
	require.NoError(t, err)
	_, err = worktree.Commit("Add to_delete.txt", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)

	// Create shadow branch (simulating agent work on the file)
	createShadowBranchWithContent(t, repo, "mno7890", "e3b0c4", map[string][]byte{
		"to_delete.txt": []byte("agent modified content"),
	})

	// Stage the file for deletion (git rm)
	_, err = worktree.Remove("to_delete.txt")
	require.NoError(t, err)

	// Get shadow tree
	shadowBranch := checkpoint.ShadowBranchNameForCommit("mno7890", "e3b0c4")
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	require.NoError(t, err)
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	require.NoError(t, err)
	shadowTree, err := shadowCommit.Tree()
	require.NoError(t, err)

	// Deleted file SHOULD count as overlap - the agent's deletion is being committed
	result := stagedFilesOverlapWithContent(context.Background(), repo, shadowTree, []string{"to_delete.txt"}, []string{"to_delete.txt"})
	assert.True(t, result, "Deleted file should count as overlap (agent's deletion being committed)")
}

// createShadowBranchWithContent creates a shadow branch with the given file contents.
// This helper directly uses go-git APIs to avoid paths.WorktreeRoot() dependency.
//
//nolint:unparam // worktreeID is kept as a parameter for flexibility even if tests currently use same value
func createShadowBranchWithContent(t *testing.T, repo *git.Repository, baseCommit, worktreeID string, fileContents map[string][]byte) {
	t.Helper()

	shadowBranchName := checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranchName)

	// Get HEAD for base tree
	head, err := repo.Head()
	require.NoError(t, err)

	headCommit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	baseTree, err := headCommit.Tree()
	require.NoError(t, err)

	// Flatten existing tree into map
	entries := make(map[string]object.TreeEntry)
	err = checkpoint.FlattenTree(repo, baseTree, "", entries)
	require.NoError(t, err)

	// Add/update files with provided content
	for filePath, content := range fileContents {
		// Create blob with content
		blob := repo.Storer.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		blob.SetSize(int64(len(content)))
		writer, err := blob.Writer()
		require.NoError(t, err)
		_, err = writer.Write(content)
		require.NoError(t, err)
		err = writer.Close()
		require.NoError(t, err)

		blobHash, err := repo.Storer.SetEncodedObject(blob)
		require.NoError(t, err)

		entries[filePath] = object.TreeEntry{
			Name: filePath,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Build tree from entries
	treeHash, err := checkpoint.BuildTreeFromEntries(repo, entries)
	require.NoError(t, err)

	// Create commit
	commit := &object.Commit{
		TreeHash: treeHash,
		Message:  "Test checkpoint",
		Author: object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
		Committer: object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	}

	commitObj := repo.Storer.NewEncodedObject()
	err = commit.Encode(commitObj)
	require.NoError(t, err)

	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	require.NoError(t, err)

	// Create branch reference
	newRef := plumbing.NewHashReference(refName, commitHash)
	err = repo.Storer.SetReference(newRef)
	require.NoError(t, err)
}

// TestExtractSignificantLines tests the line extraction with length-based filtering.
// Lines must be >= 10 characters after trimming whitespace.
func TestExtractSignificantLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  string
		wantKeys []string // lines that should be in the result
		wantNot  []string // lines that should NOT be in the result
	}{
		{
			name: "go function",
			content: `package main

func hello() {
	fmt.Println("hello world")
	return
}`,
			wantKeys: []string{
				"package main",               // 12 chars
				"func hello() {",             // 14 chars
				`fmt.Println("hello world")`, // 26 chars
			},
			wantNot: []string{
				"}",      // 1 char
				"return", // 6 chars
			},
		},
		{
			name: "python function",
			content: `def calculate(x, y):
    result = x + y
    print(f"Result: {result}")
    return result`,
			wantKeys: []string{
				"def calculate(x, y):",       // 20 chars
				"result = x + y",             // 14 chars
				`print(f"Result: {result}")`, // 25 chars
				"return result",              // 13 chars
			},
			wantNot: []string{},
		},
		{
			name: "javascript",
			content: `const handler = async (req) => {
  const data = await fetch(url);
  return data.json();
};`,
			wantKeys: []string{
				"const handler = async (req) => {", // 32 chars
				"const data = await fetch(url);",   // 30 chars
				"return data.json();",              // 19 chars
			},
			wantNot: []string{
				"};", // 2 chars
			},
		},
		{
			name: "short lines filtered",
			content: `a = 1
b = 2
longVariableName = 42`,
			wantKeys: []string{
				"longVariableName = 42", // 21 chars
			},
			wantNot: []string{
				"a = 1", // 5 chars
				"b = 2", // 5 chars
			},
		},
		{
			name: "structural lines filtered by length",
			content: `{
  });
  ]);
  },
}`,
			wantKeys: []string{},
			wantNot: []string{
				"{",   // 1 char
				"});", // 3 chars
				"]);", // 3 chars
				"},",  // 2 chars
				"}",   // 1 char
			},
		},
		{
			name: "regex and special chars kept if long enough",
			content: `short
/^[a-z0-9]+@[a-z]+\.[a-z]{2,}$/
x`,
			wantKeys: []string{
				"/^[a-z0-9]+@[a-z]+\\.[a-z]{2,}$/", // 32 chars - kept even though mostly non-alpha
			},
			wantNot: []string{
				"short", // 5 chars
				"x",     // 1 char
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := extractSignificantLines(tt.content)

			for _, want := range tt.wantKeys {
				if !result[want] {
					t.Errorf("extractSignificantLines() missing expected line: %q", want)
				}
			}

			for _, notWant := range tt.wantNot {
				if result[notWant] {
					t.Errorf("extractSignificantLines() should not contain: %q", notWant)
				}
			}
		})
	}
}

// TestHasSignificantContentOverlap tests the content overlap detection logic.
// We require at least 2 matching significant lines to count as overlap.
func TestHasSignificantContentOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		stagedContent string
		shadowContent string
		wantOverlap   bool
	}{
		{
			name:          "two matching significant lines - overlap",
			stagedContent: "this is a significant line\nanother matching line here\nshort",
			shadowContent: "this is a significant line\nanother matching line here\nother",
			wantOverlap:   true,
		},
		{
			name:          "only one matching significant line - no overlap",
			stagedContent: "this is a significant line\ncompletely different staged",
			shadowContent: "this is a significant line\ncompletely different shadow",
			wantOverlap:   false,
		},
		{
			name:          "no matching significant lines",
			stagedContent: "completely different content here",
			shadowContent: "this is the shadow content now",
			wantOverlap:   false,
		},
		{
			name:          "both have only short lines - no significant content",
			stagedContent: "a = 1\nb = 2\nc = 3",
			shadowContent: "x = 1\ny = 2\nz = 3",
			wantOverlap:   false,
		},
		{
			name:          "shadow has significant lines but staged has none",
			stagedContent: "a = 1\nb = 2",
			shadowContent: "this is significant content from shadow",
			wantOverlap:   false,
		},
		{
			name:          "staged has significant lines but shadow has none",
			stagedContent: "this is significant content from staged",
			shadowContent: "x = 1\ny = 2",
			wantOverlap:   false,
		},
		{
			name:          "empty strings",
			stagedContent: "",
			shadowContent: "",
			wantOverlap:   false,
		},
		{
			name:          "single shared line like package main - no overlap (boilerplate)",
			stagedContent: "package main\nfunc NewImplementation() {}",
			shadowContent: "package main\nfunc OriginalCode() {}",
			wantOverlap:   false,
		},
		{
			name:          "multiple shared lines - overlap (user kept agent work)",
			stagedContent: "package main\nfunc SharedFunction() {\nreturn nil",
			shadowContent: "package main\nfunc SharedFunction() {\nreturn nil",
			wantOverlap:   true,
		},
		{
			name:          "very small file with single match - overlap (small file exception)",
			stagedContent: "this is a unique line here\nshort",
			shadowContent: "this is a unique line here\nshort",
			wantOverlap:   true, // Shadow has only 1 significant line, so 1 match counts
		},
		{
			name:          "very small file no match - no overlap",
			stagedContent: "completely different staged content",
			shadowContent: "short",
			wantOverlap:   false, // Shadow is very small but no matching lines
		},
		{
			name:          "large staged vs very small shadow with single match - overlap",
			stagedContent: "line one here\nline two here\nline three here\nshared content line",
			shadowContent: "shared content line\nshort",
			wantOverlap:   true, // Shadow has only 1 significant line, so 1 match counts
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hasSignificantContentOverlap(tt.stagedContent, tt.shadowContent)
			if got != tt.wantOverlap {
				t.Errorf("hasSignificantContentOverlap() = %v, want %v", got, tt.wantOverlap)
			}
		})
	}
}

// TestTrimLine tests whitespace trimming from lines.
func TestTrimLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{"no whitespace", "hello", "hello"},
		{"leading spaces", "   hello", "hello"},
		{"trailing spaces", "hello   ", "hello"},
		{"both leading and trailing spaces", "   hello   ", "hello"},
		{"leading tabs", "\t\thello", "hello"},
		{"trailing tabs", "hello\t\t", "hello"},
		{"mixed whitespace", " \t hello \t ", "hello"},
		{"only spaces", "     ", ""},
		{"only tabs", "\t\t\t", ""},
		{"empty string", "", ""},
		{"spaces in middle preserved", "hello world", "hello world"},
		{"tabs in middle preserved", "hello\tworld", "hello\tworld"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := trimLine(tt.line)
			if got != tt.want {
				t.Errorf("trimLine(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}
