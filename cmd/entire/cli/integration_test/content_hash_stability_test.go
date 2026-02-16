//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/contenthash"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentHash_StableAcrossRebase(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// 1. Create initial files and commit on the feature branch
		testFile1 := filepath.Join(env.RepoDir, "test1.txt")
		testFile2 := filepath.Join(env.RepoDir, "test2.txt")

		require.NoError(t, os.WriteFile(testFile1, []byte("Initial content 1\n"), 0644))
		require.NoError(t, os.WriteFile(testFile2, []byte("Initial content 2\n"), 0644))

		env.GitAdd("test1.txt")
		env.GitAdd("test2.txt")
		env.GitCommit("Add initial test files")

		// 2. Modify files on the feature branch
		require.NoError(t, os.WriteFile(testFile1, []byte("Modified content 1\n"), 0644))
		require.NoError(t, os.WriteFile(testFile2, []byte("Modified content 2\n"), 0644))

		// Compute the content hash for these file changes
		files := &contenthash.FileChanges{
			Modified: []string{"test1.txt", "test2.txt"},
		}
		contentHash1, err := contenthash.ComputeContentHashForChanges(files, env.RepoDir)
		require.NoError(t, err)
		assert.NotEmpty(t, contentHash1, "Content hash should not be empty")

		// 3. Commit the modified files
		env.GitAdd("test1.txt")
		env.GitAdd("test2.txt")
		env.GitCommit("Modify test files")

		// 4. Find default branch name and create a new base commit to force a rebase
		defaultBranch := env.GetDefaultBranch()

		cmd := exec.Command("git", "checkout", defaultBranch)
		cmd.Dir = env.RepoDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git checkout %s failed: %s", defaultBranch, string(out))

		env.WriteFile("base.txt", "Base change\n")
		env.GitAdd("base.txt")
		env.GitCommit("Base change on default branch")

		// Rebase feature branch onto new default branch
		cmd = exec.Command("git", "rebase", defaultBranch, "feature/test-branch")
		cmd.Dir = env.RepoDir
		out, err = cmd.CombinedOutput()
		require.NoError(t, err, "git rebase failed: %s", string(out))

		cmd = exec.Command("git", "checkout", "feature/test-branch")
		cmd.Dir = env.RepoDir
		out, err = cmd.CombinedOutput()
		require.NoError(t, err, "git checkout feature/test-branch failed: %s", string(out))

		// 5. Verify content hash should be the same after rebase
		// (same file contents, same relative paths)
		contentHash2, err := contenthash.ComputeContentHashForChanges(files, env.RepoDir)
		require.NoError(t, err)

		assert.Equal(t, contentHash1, contentHash2, "Content hash should remain stable after rebase")
	})
}

func TestContentHash_ChangesWithFileModifications(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create test file
		testFile := filepath.Join(env.RepoDir, "test.txt")

		// Initial content
		require.NoError(t, os.WriteFile(testFile, []byte("Initial content\n"), 0644))

		// Compute first hash
		files := &contenthash.FileChanges{
			New: []string{"test.txt"},
		}
		hash1, err := contenthash.ComputeContentHashForChanges(files, env.RepoDir)
		require.NoError(t, err)

		// Modify file content
		require.NoError(t, os.WriteFile(testFile, []byte("Modified content\n"), 0644))

		// Compute second hash
		files2 := &contenthash.FileChanges{
			Modified: []string{"test.txt"},
		}
		hash2, err := contenthash.ComputeContentHashForChanges(files2, env.RepoDir)
		require.NoError(t, err)

		// Verify hashes are different when content changes
		assert.NotEqual(t, hash1, hash2, "Content hash should change when file is modified")
		assert.NotEmpty(t, hash1, "First hash should not be empty")
		assert.NotEmpty(t, hash2, "Second hash should not be empty")
	})
}

func TestContentHash_DeterministicAcrossRuns(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create multiple test files
		testFiles := map[string]string{
			"file1.txt": "Content 1\n",
			"file2.txt": "Content 2\n",
			"file3.txt": "Content 3\n",
		}

		for name, content := range testFiles {
			path := filepath.Join(env.RepoDir, name)
			require.NoError(t, os.WriteFile(path, []byte(content), 0644))
		}

		// Compute hash multiple times - should be deterministic
		var hashes []string
		for i := 0; i < 3; i++ {
			files := &contenthash.FileChanges{
				New: []string{"file1.txt", "file2.txt", "file3.txt"},
			}
			hash, err := contenthash.ComputeContentHashForChanges(files, env.RepoDir)
			require.NoError(t, err)
			hashes = append(hashes, hash)
		}

		// All hashes should be identical
		for i := 1; i < len(hashes); i++ {
			assert.Equal(t, hashes[0], hashes[i], "Hash should be deterministic across runs")
		}
		assert.NotEmpty(t, hashes[0], "Hash should not be empty")
	})
}

func TestContentHash_HandlesDeletions(t *testing.T) {
	t.Parallel()

	RunForAllStrategies(t, func(t *testing.T, env *TestEnv, strategyName string) {
		// Create test files
		file1 := filepath.Join(env.RepoDir, "keep.txt")
		file2 := filepath.Join(env.RepoDir, "delete.txt")

		require.NoError(t, os.WriteFile(file1, []byte("Keep this\n"), 0644))
		require.NoError(t, os.WriteFile(file2, []byte("Delete this\n"), 0644))

		// Hash with both files
		files := &contenthash.FileChanges{
			New: []string{"keep.txt", "delete.txt"},
		}
		hash1, err := contenthash.ComputeContentHashForChanges(files, env.RepoDir)
		require.NoError(t, err)

		// Delete one file
		require.NoError(t, os.Remove(file2))

		// Hash with only one file
		files2 := &contenthash.FileChanges{
			Modified: []string{"keep.txt"},
			Deleted:  []string{"delete.txt"},
		}
		hash2, err := contenthash.ComputeContentHashForChanges(files2, env.RepoDir)
		require.NoError(t, err)

		// Hashes should differ when files are deleted
		assert.NotEqual(t, hash1, hash2, "Content hash should change when files are deleted")
		assert.NotEmpty(t, hash1, "First hash should not be empty")
		assert.NotEmpty(t, hash2, "Second hash should not be empty")
	})
}
