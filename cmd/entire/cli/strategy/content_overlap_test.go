package strategy

import (
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
	result := filesOverlapWithContent(repo, shadowBranch, commit, []string{"test.txt"})
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
	result := filesOverlapWithContent(repo, shadowBranch, commit, []string{"newfile.txt"})
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
	result := filesOverlapWithContent(repo, shadowBranch, commit, []string{"replaced.txt"})
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
	result := filesOverlapWithContent(repo, shadowBranch, commit, []string{"fileB.txt"})
	assert.False(t, result, "File not in commit should not count as overlap")

	// Test: fileA in filesTouched and in commit - should overlap (new file with matching content)
	result = filesOverlapWithContent(repo, shadowBranch, commit, []string{"fileA.txt"})
	assert.True(t, result, "File in commit with matching content should count as overlap")
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
	result := filesOverlapWithContent(repo, "entire/nonexistent-e3b0c4", commit, []string{"test.txt"})
	assert.True(t, result, "Missing shadow branch should fall back to assuming overlap")
}

// createShadowBranchWithContent creates a shadow branch with the given file contents.
// This helper directly uses go-git APIs to avoid paths.RepoRoot() dependency.
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
