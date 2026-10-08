package strategy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
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

	// Record turn-end hashes with same file content as session created
	sessionContent := []byte("session modified content")
	hashes := recordedHashes(map[string][]byte{
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
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"test.txt"})
	assert.True(t, result, "Modified file should count as overlap (user edited session's work)")
}

// TestFilesOverlapWithContent_NewFile_ContentMatch tests that a new file with
// matching content counts as overlap.
func TestFilesOverlapWithContent_NewFile_ContentMatch(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Record turn-end hashes with a new file
	originalContent := []byte("session created this content")
	hashes := recordedHashes(map[string][]byte{
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
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"newfile.txt"})
	assert.True(t, result, "New file with matching content should count as overlap")
}

// TestFilesOverlapWithContent_NewFile_UserRewriteStillLinks tests that a new file
// committed with different content than the agent left still links: linking is
// by name, because editing an agent-created file before committing it is far
// more common than replacing it.
func TestFilesOverlapWithContent_NewFile_UserRewriteStillLinks(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Record turn-end hashes with a file
	sessionContent := []byte("session created this")
	hashes := recordedHashes(map[string][]byte{
		"replaced.txt": sessionContent,
	})

	// Commit the file with different content than the agent left (the user
	// edited, or even rewrote, the agent's new file before committing).
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

	// Linking is by name: the commit carries a path the session created.
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"replaced.txt"})
	assert.True(t, result, "a new file in FilesTouched links by name, whatever its committed content")
}

// TestFilesOverlapWithContent_NewFile_RecordedDeletionDoesNotLink pins the one
// exception to name matching: the agent's last action on the path was deleting
// it, so a commit that adds the path as a new file re-creates someone else's
// file and does not link.
func TestFilesOverlapWithContent_NewFile_RecordedDeletionDoesNotLink(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "recreated.txt"), []byte("re-created by the user"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("recreated.txt")
	require.NoError(t, err)
	headCommit, err := wt.Commit("Re-create file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	commit, err := repo.CommitObject(headCommit)
	require.NoError(t, err)

	hashes := map[string]string{"recreated.txt": touchedFileDeleted}
	assert.False(t, filesOverlapWithContent(context.Background(), hashes, commit, []string{"recreated.txt"}))
}

// TestFilesOverlapWithContent_FileNotInCommit tests that a file in filesTouched
// but not in the commit doesn't count as overlap.
func TestFilesOverlapWithContent_FileNotInCommit(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Record turn-end hashes with files
	fileAContent := []byte("file A content")
	fileBContent := []byte("file B content")
	hashes := recordedHashes(map[string][]byte{
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
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"fileB.txt"})
	assert.False(t, result, "File not in commit should not count as overlap")

	// Test: fileA in filesTouched and in commit - should overlap (new file with matching content)
	result = filesOverlapWithContent(context.Background(), hashes, commit, []string{"fileA.txt"})
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

	// Record turn-end hashes (simulating agent work that includes the deletion)
	hashes := recordedHashes(map[string][]byte{
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
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"to_delete.txt"})
	assert.True(t, result, "Deleted file should count as overlap (agent's deletion being committed)")
}

// TestFilesOverlapWithContent_NoRecordedHash tests the name-match fallback for
// a new file with no recorded hash (e.g. it reached FilesTouched via a task
// record, not a turn-end step).
func TestFilesOverlapWithContent_NoRecordedHash(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a commit adding a file nothing recorded a hash for
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

	// Test: a path without a recorded hash falls back to name matching
	result := filesOverlapWithContent(context.Background(), nil, commit, []string{"test.txt"})
	assert.True(t, result, "A committed path without a recorded hash should match by name")

	// ...but a path absent from the commit still does not overlap.
	result = filesOverlapWithContent(context.Background(), nil, commit, []string{"elsewhere.txt"})
	assert.False(t, result, "Name matching must still require the path to be committed")
}

// TestFilesWithRemainingAgentChanges_FileNotCommitted tests that files not in the commit
// are kept in the remaining list.
func TestFilesWithRemainingAgentChanges_FileNotCommitted(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Record turn-end hashes with two files
	hashes := recordedHashes(map[string][]byte{
		"fileA.txt": []byte("content A"),
		"fileB.txt": []byte("content B"),
	})

	// The agent wrote both files; only fileA is committed.
	fileA := filepath.Join(dir, "fileA.txt")
	require.NoError(t, os.WriteFile(fileA, []byte("content A"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileB.txt"), []byte("content B"), 0o644))
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

	committedFiles := map[string]struct{}{"fileA.txt": {}}

	// fileB was not committed - should be in remaining
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit, []string{"fileA.txt", "fileB.txt"}, committedFiles)
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

	// Record turn-end hashes with file
	hashes := recordedHashes(map[string][]byte{
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

	committedFiles := map[string]struct{}{"test.txt": {}}

	// File was fully committed - should NOT be in remaining
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit, []string{"test.txt"}, committedFiles)
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

	// The recorded hash is of the full agent content
	fullContent := []byte("line 1\nline 2\nline 3\nline 4\n")
	hashes := recordedHashes(map[string][]byte{
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

	committedFiles := map[string]struct{}{"test.txt": {}}

	// Content doesn't match and working tree is dirty - file should be in remaining
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit, []string{"test.txt"}, committedFiles)
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

	// The recorded hash is of the agent's content
	agentContent := []byte("func GetPort() int { return 8080 }\n")
	hashes := recordedHashes(map[string][]byte{
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

	committedFiles := map[string]struct{}{"config.go": {}}

	// Content differs from the recorded hash but working tree is clean — no carry-forward
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit, []string{"config.go"}, committedFiles)
	assert.Empty(t, remaining, "Replaced content with clean working tree should not be in remaining")
}

// TestFilesWithRemainingAgentChanges_AutocrlfNormalizedWorkingTree verifies that
// line-ending normalization does not create phantom carry-forward files.
func TestFilesWithRemainingAgentChanges_AutocrlfNormalizedWorkingTree(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()

	testutil.RunGit(t, dir, "config", "core.autocrlf", "true")

	agentContent := []byte("package main\r\n\r\nimport \"fmt\"\r\n\r\nfunc main() {\r\n\tfmt.Println(\"hello world\")\n\tfmt.Println(\"goodbye world\")\n}\n")
	hashes := recordedHashes(map[string][]byte{
		"src/main.go": agentContent,
	})

	workingTreeContent := "package main\r\n\r\nimport \"fmt\"\r\n\r\nfunc main() {\r\n\tfmt.Println(\"hello world\")\r\n\tfmt.Println(\"goodbye world\")\r\n}\r\n"
	testutil.WriteFile(t, dir, "src/main.go", workingTreeContent)
	testutil.RunGit(t, dir, "add", "--", "src/main.go")
	testutil.RunGit(t, dir, "commit", "-m", "Commit normalized content")

	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	committedFile, err := commit.File("src/main.go")
	require.NoError(t, err)
	committedContent, err := committedFile.Contents()
	require.NoError(t, err)
	assert.NotContains(t, committedContent, "\r\n", "native git add must normalize the committed blob to LF")
	diskContent, err := os.ReadFile(filepath.Join(dir, "src", "main.go"))
	require.NoError(t, err)
	assert.Equal(t, workingTreeContent, string(diskContent), "the working tree must retain CRLF bytes")

	committedFiles := map[string]struct{}{"src/main.go": {}}

	// Git reports no diff here even though the on-disk bytes are CRLF and the
	// committed blob is LF-normalized under core.autocrlf=true.
	testutil.RunGit(t, dir, "diff", "--exit-code", "--", "src/main.go")

	remaining := filesWithRemainingAgentChanges(t.Context(), repo, hashes, commit, []string{"src/main.go"}, committedFiles)
	assert.Empty(t, remaining, "autocrlf-only working tree differences should not be carried forward")
}

// TestFilesWithRemainingAgentChanges_ComparesWorktreeToCommitNotIndex is a
// design pin: it passes with the raw-hash fallback too, but fails if the native
// Git check is simplified to an index-relative bare `git diff`.
func TestFilesWithRemainingAgentChanges_ComparesWorktreeToCommitNotIndex(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()

	hashes := recordedHashes(map[string][]byte{
		"config.go": []byte("agent content\n"),
	})

	testutil.WriteFile(t, dir, "config.go", "committed replacement\n")
	testutil.GitAdd(t, dir, "config.go")
	testutil.GitCommit(t, dir, "Replace config")

	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	// Move both the index and working tree past the commit. A plain `git diff`
	// reports clean because it compares these two, but carry-forward must compare
	// the working tree with the commit that was just created.
	testutil.WriteFile(t, dir, "config.go", "next staged change\n")
	testutil.GitAdd(t, dir, "config.go")
	testutil.RunGit(t, dir, "diff", "--exit-code", "--", "config.go")

	committedFiles := map[string]struct{}{"config.go": {}}
	remaining := filesWithRemainingAgentChanges(t.Context(), repo, hashes, commit, []string{"config.go"}, committedFiles)
	assert.Equal(t, []string{"config.go"}, remaining)
}

func TestWorkingTreeMatchesBlobSymlinkHashesTheTargetPath(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	t.Parallel()

	dir := t.TempDir()
	const target = "real.txt"
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "link.txt")))
	h := plumbing.NewHasher(config.SHA1, plumbing.BlobObject, int64(len(target)))
	_, err := h.Write([]byte(target))
	require.NoError(t, err)

	// Withholding a worktree symlink from hash-object is gitrepo's rule now
	// (TestHashableWorktreeEntry_WorktreeSymlinkIsWithheld); what stays here is
	// what this fallback must then answer for one.
	assert.True(t, workingTreeMatchesBlob(dir, "link.txt", filemode.Symlink, h.Sum()))
	assert.False(t, workingTreeMatchesBlob(dir, "link.txt", filemode.Regular, h.Sum()),
		"a symlink must not compare clean against a regular-file commit")
}

// TestFilesWithRemainingAgentChanges_NoRecordedHash tests the fallback to
// file-level subtraction for paths without a recorded hash, including the
// phantom-path guard for uncommitted paths missing from the worktree.
func TestFilesWithRemainingAgentChanges_NoRecordedHash(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a commit; nothing recorded hashes for these paths
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

	// other.txt exists but is uncommitted; phantom.txt was never created.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("uncommitted"), 0o644))
	committedFiles := map[string]struct{}{"test.txt": {}}
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, nil, commit, []string{"test.txt", "other.txt", "phantom.txt"}, committedFiles)

	// test.txt is committed and the worktree matches the commit, other.txt is
	// not committed; phantom.txt is neither committed nor in the worktree.
	assert.Equal(t, []string{"other.txt"}, remaining, "unhashed paths: committed-and-clean drops, uncommitted stays, phantom drops")
}

// TestFilesWithRemainingAgentChanges_NoRecordedHash_PartialCommitKept: a
// committed path with no recorded hash (an upgraded session, or one whose hash
// MergeUnhashedFilesTouched cleared) stays while the worktree still holds more
// than was committed, as a hashed path would.
func TestFilesWithRemainingAgentChanges_NoRecordedHash_PartialCommitKept(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// The user commits the first half of the file...
	require.NoError(t, os.WriteFile(filepath.Join(dir, "half.txt"), []byte("first half\n"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("half.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("Commit half", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)
	// ...while the rest is still in the worktree.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "half.txt"), []byte("first half\nsecond half\n"), 0o644))

	remaining := filesWithRemainingAgentChanges(context.Background(), repo, nil, commit,
		[]string{"half.txt"}, map[string]struct{}{"half.txt": {}})
	assert.Equal(t, []string{"half.txt"}, remaining)
}

// resolveCommitTrees is a test helper that resolves the HEAD tree and parent
// tree of a commit. Used to test cache equivalence.
func resolveCommitTrees(t *testing.T, commit *object.Commit) (headTree, parentTree *object.Tree) {
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

	return headTree, parentTree
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

	// Record turn-end hashes
	hashes := recordedHashes(map[string][]byte{
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

	headTree, parentTree := resolveCommitTrees(t, commit)

	// Cache miss (no opts)
	resultWithout := filesOverlapWithContent(context.Background(), hashes, commit, []string{"test.txt"})

	// Cache hit (all trees pre-resolved)
	resultWith := filesOverlapWithContent(context.Background(), hashes, commit, []string{"test.txt"}, overlapOpts{
		headTree:      headTree,
		parentTree:    parentTree,
		hasParentTree: true,
	})

	assert.Equal(t, resultWithout, resultWith, "Cache hit and cache miss should produce the same result")
	assert.True(t, resultWith, "Modified file should count as overlap")
}

// TestFilesOverlapWithContent_PartialCache verifies correct behavior when the
// trees are pre-resolved for a new file whose content matches the recorded hash.
func TestFilesOverlapWithContent_PartialCache(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Record turn-end hashes with new file
	content := []byte("session content")
	hashes := recordedHashes(map[string][]byte{
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

	headTree, parentTree := resolveCommitTrees(t, commit)

	// Pre-resolved headTree and parentTree
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"newfile.txt"}, overlapOpts{
		headTree:      headTree,
		parentTree:    parentTree,
		hasParentTree: true,
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

	// Record turn-end hashes with content matching the initial commit's file
	hashes := recordedHashes(map[string][]byte{
		"test.txt": []byte("initial content"),
	})

	headTree, err := commit.Tree()
	require.NoError(t, err)

	// Cache with hasParentTree=true and parentTree=nil (initial commit has no parent)
	result := filesOverlapWithContent(context.Background(), hashes, commit, []string{"test.txt"}, overlapOpts{
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

	// Record turn-end hashes with two files
	hashes := recordedHashes(map[string][]byte{
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

	headTree, _ := resolveCommitTrees(t, commit)

	committedFiles := map[string]struct{}{"fileA.txt": {}}
	filesTouched := []string{"fileA.txt", "fileB.txt"}

	// Cache miss
	resultWithout := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit, filesTouched, committedFiles)

	// Cache hit
	resultWith := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit, filesTouched, committedFiles, overlapOpts{
		headTree: headTree,
	})

	assert.Equal(t, resultWithout, resultWith, "Cache hit and cache miss should produce the same result")
	// fileB.txt was not committed, so it should be in remaining
	assert.Contains(t, resultWith, "fileB.txt")
	// fileA.txt was committed with matching content, so it should NOT be in remaining
	assert.NotContains(t, resultWith, "fileA.txt")
}

// TestFilesWithRemainingAgentChanges_PhantomFile tests that files tracked in
// filesTouched with no recorded hash and missing from the worktree are skipped. This
// happens when an agent's transcript references a file path (e.g. via a
// write_file tool call) that was never actually created on disk — for example
// when an agent tries to write src/types.go but creates src/types/types.go
// instead. Without this check, phantom files cause infinite carry-forward.
func TestFilesWithRemainingAgentChanges_PhantomFile(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Only the REAL file has a recorded hash (the phantom path was never
	// created, so there was nothing to hash).
	hashes := recordedHashes(map[string][]byte{
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

	committedFiles := map[string]struct{}{"src/types/types.go": {}}

	// filesTouched includes both the real path and a phantom path.
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit,
		[]string{"src/types.go", "src/types/types.go"}, committedFiles)

	// src/types.go is not committed, has no recorded hash, and is missing → skip.
	// src/types/types.go is committed with matching content → skip.
	assert.Empty(t, remaining, "Phantom files should not be carried forward")
}

// TestFilesWithRemainingAgentChanges_UncommittedDeletion verifies that an
// agent deletion the user did not commit is carried forward: the commit tree
// still has the path and the worktree still lacks it, so the later commit that
// deletes it must still link the session.
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
	_, err = wt.Commit("Add file that agent will delete", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// The agent's turn-end step recorded the deletion.
	hashes := map[string]string{"to_delete.txt": touchedFileDeleted}

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
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, userCommit,
		[]string{"to_delete.txt", "other.txt"}, committedFiles)

	// to_delete.txt is kept: the deletion is still pending. other.txt has no
	// recorded hash, was committed, and the worktree matches the commit, so it
	// drops.
	assert.Equal(t, []string{"to_delete.txt"}, remaining, "a pending recorded deletion is carried forward")
}

// recordedDeletionRepo commits to_delete.txt and removes it from the worktree,
// as an agent's deletion would, returning the repo, its worktree, and the
// recorded hashes for that deletion.
func recordedDeletionRepo(t *testing.T) (string, *git.Repository, *git.Worktree, map[string]string) {
	t.Helper()
	dir := setupGitRepo(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "to_delete.txt"), []byte("will be deleted"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("to_delete.txt")
	require.NoError(t, err)
	_, err = wt.Commit("Add file that agent will delete", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "to_delete.txt")))
	return dir, repo, wt, map[string]string{"to_delete.txt": touchedFileDeleted}
}

// TestFilesWithRemainingAgentChanges_CommittedDeletion: once a commit carries
// the agent's deletion, nothing of it is left to carry forward.
func TestFilesWithRemainingAgentChanges_CommittedDeletion(t *testing.T) {
	t.Parallel()
	_, repo, wt, hashes := recordedDeletionRepo(t)

	_, err := wt.Remove("to_delete.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("Commit the deletion", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit,
		[]string{"to_delete.txt"}, map[string]struct{}{"to_delete.txt": {}})
	assert.Empty(t, remaining)
}

// TestFilesWithRemainingAgentChanges_RecreatedDeletion: a recorded deletion
// whose path is back in the worktree was re-created by someone else; it is no
// longer the agent's pending work.
func TestFilesWithRemainingAgentChanges_RecreatedDeletion(t *testing.T) {
	t.Parallel()
	dir, repo, wt, hashes := recordedDeletionRepo(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other"), 0o644))
	_, err := wt.Add("other.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("Unrelated commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "to_delete.txt"), []byte("re-created"), 0o644))

	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit,
		[]string{"to_delete.txt"}, map[string]struct{}{"other.txt": {}})
	assert.Empty(t, remaining)
}

// TestFilesWithRemainingAgentChanges_StagedEditCommittedDeletionPending: the
// user staged an edit to a file, the agent then deleted it from the worktree,
// and the user committed the staged edit. The commit modified the path — it is
// in the commit's changed set — but the commit tree still has it and the
// worktree still lacks it, so the agent's deletion is still pending.
func TestFilesWithRemainingAgentChanges_StagedEditCommittedDeletionPending(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)

	target := filepath.Join(dir, "edited.txt")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0o644))
	_, err = wt.Add("edited.txt")
	require.NoError(t, err)
	_, err = wt.Commit("Add file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// The user stages an edit...
	require.NoError(t, os.WriteFile(target, []byte("original\nuser edit\n"), 0o644))
	_, err = wt.Add("edited.txt")
	require.NoError(t, err)
	// ...the agent deletes the file from the worktree...
	require.NoError(t, os.Remove(target))
	// ...and the user commits what was staged.
	testutil.GitCommit(t, dir, "Commit the staged edit")
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	_, err = commit.File("edited.txt")
	require.NoError(t, err, "fixture: the commit carries the staged edit, not the deletion")

	hashes := map[string]string{"edited.txt": touchedFileDeleted}
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit,
		[]string{"edited.txt"}, map[string]struct{}{"edited.txt": {}})
	assert.Equal(t, []string{"edited.txt"}, remaining)
}

// TestFilesWithRemainingAgentChanges_NoRecordedHash_PendingTrackedDeletion: a
// path with no recorded hash that the commit still has but the worktree lacks
// is a pending deletion of a tracked file (an older CLI's state, or a route
// that records no hash), not a phantom. Only a path absent from both the
// commit and the worktree is a phantom.
func TestFilesWithRemainingAgentChanges_NoRecordedHash_PendingTrackedDeletion(t *testing.T) {
	t.Parallel()
	_, repo, wt, _ := recordedDeletionRepo(t)

	require.NoError(t, os.WriteFile(filepath.Join(wt.Filesystem().Root(), "other.txt"), []byte("other"), 0o644))
	_, err := wt.Add("other.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("Commit another file", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	remaining := filesWithRemainingAgentChanges(context.Background(), repo, nil, commit,
		[]string{"to_delete.txt", "other.txt", "never_existed.txt"}, map[string]struct{}{"other.txt": {}})
	assert.Equal(t, []string{"to_delete.txt"}, remaining,
		"the pending tracked deletion stays; the committed file and the phantom drop")
}

// TestFilesWithRemainingAgentChanges_HashedFileDeletedByCommit: the agent
// edited a tracked file (so a hash is recorded), and the human deleted it and
// committed the deletion. The recorded hash is history, not remaining work:
// with the file gone from both the commit and the worktree, nothing is left to
// carry forward.
func TestFilesWithRemainingAgentChanges_HashedFileDeletedByCommit(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	testutil.WriteFile(t, dir, "edited.txt", "original\n")
	testutil.GitAdd(t, dir, "edited.txt")
	testutil.GitCommit(t, dir, "Add file")
	testutil.WriteFile(t, dir, "edited.txt", "agent edit\n")
	hashes := map[string]string{"edited.txt": gitHashObject(t, dir, "edited.txt")}

	testutil.RunGit(t, dir, "rm", "-q", "-f", "--", "edited.txt")
	testutil.GitCommit(t, dir, "Delete the file")
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit,
		[]string{"edited.txt"}, map[string]struct{}{"edited.txt": {}})
	assert.Empty(t, remaining)
}

// TestFilesWithRemainingAgentChanges_UnreadableParentKeepsDeletion: when the
// worktree cannot be inspected (here the parent directory is unreadable, so
// Lstat fails with a permission error rather than "does not exist"), a pending
// recorded deletion is kept rather than dropped as re-created.
func TestFilesWithRemainingAgentChanges_UnreadableParentKeepsDeletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not block Lstat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	dir := setupGitRepo(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	testutil.WriteFile(t, dir, "locked/gone.txt", "tracked\n")
	testutil.WriteFile(t, dir, "other.txt", "other\n")
	testutil.GitAdd(t, dir, "locked/gone.txt")
	testutil.GitCommit(t, dir, "Add file")
	require.NoError(t, os.Remove(filepath.Join(dir, "locked", "gone.txt")))
	testutil.GitAdd(t, dir, "other.txt")
	testutil.GitCommit(t, dir, "Commit another file")
	head, err := repo.Head()
	require.NoError(t, err)
	commit, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)

	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) //nolint:errcheck // best-effort so t.TempDir can clean up

	hashes := map[string]string{"locked/gone.txt": touchedFileDeleted}
	remaining := filesWithRemainingAgentChanges(context.Background(), repo, hashes, commit,
		[]string{"locked/gone.txt"}, map[string]struct{}{"other.txt": {}})
	assert.Equal(t, []string{"locked/gone.txt"}, remaining)
}

// TestFilesOverlapWithContent_CarriedForwardDeletionLinks: the later commit
// that finally deletes a carried-forward recorded deletion (the parent has the
// path, HEAD does not) links the session.
func TestFilesOverlapWithContent_CarriedForwardDeletionLinks(t *testing.T) {
	t.Parallel()
	_, repo, wt, hashes := recordedDeletionRepo(t)

	_, err := wt.Remove("to_delete.txt")
	require.NoError(t, err)
	commitHash, err := wt.Commit("Commit the deletion", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)
	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	assert.True(t, filesOverlapWithContent(context.Background(), hashes, commit, []string{"to_delete.txt"}))
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

	// Record turn-end hashes (content doesn't matter for modified files)
	hashes := recordedHashes(map[string][]byte{
		"test.txt": []byte("agent content"),
	})

	// Modified file should count as overlap regardless of content
	result := stagedFilesOverlapWithContent(context.Background(), repo, hashes, []string{"test.txt"}, []string{"test.txt"})
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

	// Record turn-end hashes with SAME content
	hashes := recordedHashes(map[string][]byte{
		"newfile.txt": content,
	})

	// New file with matching content should count as overlap
	result := stagedFilesOverlapWithContent(context.Background(), repo, hashes, []string{"newfile.txt"}, []string{"newfile.txt"})
	assert.True(t, result, "New file with matching content should count as overlap")
}

// TestStagedFilesOverlapWithContent_NewFile_UserRewriteStillLinks: the
// prepare-commit-msg side of TestFilesOverlapWithContent_NewFile_UserRewriteStillLinks.
func TestStagedFilesOverlapWithContent_NewFile_UserRewriteStillLinks(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// Create a NEW file with different content than the agent left
	newFile := filepath.Join(dir, "newfile.txt")
	require.NoError(t, os.WriteFile(newFile, []byte("user replaced content"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("newfile.txt")
	require.NoError(t, err)

	// Record turn-end hashes with DIFFERENT content (agent's original)
	hashes := recordedHashes(map[string][]byte{
		"newfile.txt": []byte("agent original content"),
	})

	// Linking is by name: the staged path is one the session created.
	result := stagedFilesOverlapWithContent(context.Background(), repo, hashes, []string{"newfile.txt"}, []string{"newfile.txt"})
	assert.True(t, result, "a staged new file in FilesTouched links by name, whatever its content")
}

// TestStagedFilesOverlapWithContent_NewFile_RecordedDeletionDoesNotLink: see
// TestFilesOverlapWithContent_NewFile_RecordedDeletionDoesNotLink.
func TestStagedFilesOverlapWithContent_NewFile_RecordedDeletionDoesNotLink(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "recreated.txt"), []byte("re-created by the user"), 0o644))
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("recreated.txt")
	require.NoError(t, err)

	hashes := map[string]string{"recreated.txt": touchedFileDeleted}
	assert.False(t, stagedFilesOverlapWithContent(context.Background(), repo, hashes, []string{"recreated.txt"}, []string{"recreated.txt"}))
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

	// Record turn-end hashes
	hashes := recordedHashes(map[string][]byte{
		"session.txt": []byte("session content"),
	})

	// Staged file "other.txt" is not in filesTouched "session.txt"
	result := stagedFilesOverlapWithContent(context.Background(), repo, hashes, []string{"other.txt"}, []string{"session.txt"})
	assert.False(t, result, "Non-overlapping files should return false")
}

// TestStagedFilesOverlapWithContent_DeletedFile tests that a deleted file
// (exists in HEAD but staged for deletion) DOES count as overlap.
// The agent's action of deleting the file is being committed, so the session
// context should be linked to this commit.
func TestStagedFilesOverlapWithContent_DeletedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
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

	// Record turn-end hashes (simulating agent work on the file)
	hashes := recordedHashes(map[string][]byte{
		"to_delete.txt": []byte("agent modified content"),
	})

	// Stage the file for deletion (git rm)
	_, err = worktree.Remove("to_delete.txt")
	require.NoError(t, err)

	// Deleted file SHOULD count as overlap - the agent's deletion is being committed
	result := stagedFilesOverlapWithContent(context.Background(), repo, hashes, []string{"to_delete.txt"}, []string{"to_delete.txt"})
	assert.True(t, result, "Deleted file should count as overlap (agent's deletion being committed)")
}

// recordedHashes returns the TouchedFileHashes a turn-end step would record
// for files with the given contents. Use touchedFileDeleted directly for a
// recorded deletion.
func recordedHashes(fileContents map[string][]byte) map[string]string {
	hashes := make(map[string]string, len(fileContents))
	for path, content := range fileContents {
		hashes[path] = blobHashOf(content).String()
	}
	return hashes
}

// blobHashOf returns the SHA-1 git blob hash of content.
func blobHashOf(content []byte) plumbing.Hash {
	h := plumbing.NewHasher(config.SHA1, plumbing.BlobObject, int64(len(content)))
	if _, err := h.Write(content); err != nil {
		panic(err)
	}
	return h.Sum()
}
