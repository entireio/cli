package contenthash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeContentHashForChanges(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create test files
	file1 := filepath.Join(tmpDir, "file1.go")
	file2 := filepath.Join(tmpDir, "file2.txt")
	file3 := filepath.Join(tmpDir, "new.md")

	require.NoError(t, os.WriteFile(file1, []byte("package main\n"), 0644))
	require.NoError(t, os.WriteFile(file2, []byte("Hello World\n"), 0644))
	require.NoError(t, os.WriteFile(file3, []byte("# New File\n"), 0644))

	// Test with nil changes
	hash, err := ComputeContentHashForChanges(nil, tmpDir)
	require.NoError(t, err)
	assert.Empty(t, hash, "Nil changes should produce empty hash")

	// Test with empty changes
	changes := &FileChanges{}
	hash, err = ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.Empty(t, hash, "Empty changes should produce empty hash")

	// Test with modified files
	changes = &FileChanges{
		Modified: []string{"file1.go", "file2.txt"},
	}
	hash1, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1, "Modified files should produce non-empty hash")

	// Test with new files
	changes = &FileChanges{
		New: []string{"new.md"},
	}
	hash2, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash2, "New files should produce non-empty hash")
	assert.NotEqual(t, hash1, hash2, "Different file sets should produce different hashes")

	// Test with both modified and new files
	changes = &FileChanges{
		Modified: []string{"file1.go"},
		New:      []string{"file2.txt", "new.md"},
	}
	hash3, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash3, "Mixed changes should produce non-empty hash")

	// Test with deleted files (should not affect hash)
	changes = &FileChanges{
		Modified: []string{"file1.go"},
		Deleted:  []string{"deleted.txt"},
	}
	hash4, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash4, "Changes with deletions should produce non-empty hash")
}

func TestComputeContentHashForChanges_SkipsLargeFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create small file
	smallFile := filepath.Join(tmpDir, "small.txt")
	require.NoError(t, os.WriteFile(smallFile, []byte("Small content"), 0644))

	// Create "large" file (11MB to exceed 10MB threshold)
	largeFile := filepath.Join(tmpDir, "large.bin")
	largeContent := make([]byte, 11*1024*1024) // 11MB
	require.NoError(t, os.WriteFile(largeFile, largeContent, 0644))

	// Hash with both files
	changes := &FileChanges{
		New: []string{"small.txt", "large.bin"},
	}
	hash, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash, "Should produce hash even when some files are skipped")

	// Hash with only small file
	changes = &FileChanges{
		New: []string{"small.txt"},
	}
	hash2, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)

	// Hashes should be the same since large file was skipped
	assert.Equal(t, hash, hash2, "Large file should be skipped")
}

func TestComputeContentHashForChanges_SkipsHiddenFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create regular file
	regularFile := filepath.Join(tmpDir, "regular.txt")
	require.NoError(t, os.WriteFile(regularFile, []byte("Regular content"), 0644))

	// Create hidden file
	hiddenFile := filepath.Join(tmpDir, ".hidden")
	require.NoError(t, os.WriteFile(hiddenFile, []byte("Hidden content"), 0644))

	// Create gitignore (should not be skipped)
	gitignoreFile := filepath.Join(tmpDir, ".gitignore")
	require.NoError(t, os.WriteFile(gitignoreFile, []byte("*.tmp\n"), 0644))

	// Hash with all files
	changes := &FileChanges{
		New: []string{"regular.txt", ".hidden", ".gitignore"},
	}
	hash1, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1)

	// Hash without hidden file
	changes = &FileChanges{
		New: []string{"regular.txt", ".gitignore"},
	}
	hash2, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)

	// Hashes should be the same since hidden file was skipped
	assert.Equal(t, hash1, hash2, "Hidden file should be skipped automatically")
}

func TestComputeContentHashForChanges_HandlesNonExistentFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create one valid file
	validFile := filepath.Join(tmpDir, "valid.txt")
	require.NoError(t, os.WriteFile(validFile, []byte("Valid content"), 0644))

	// Include non-existent file in changes
	changes := &FileChanges{
		Modified: []string{"valid.txt", "nonexistent.txt"},
	}

	// Non-existent files are gracefully skipped (IsLargeFile stat fails → continue)
	// so only the valid file contributes to the hash
	hash, err := ComputeContentHashForChanges(changes, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	// Should match hash of only the valid file
	changesValid := &FileChanges{
		Modified: []string{"valid.txt"},
	}
	hashValid, err := ComputeContentHashForChanges(changesValid, tmpDir)
	require.NoError(t, err)
	assert.Equal(t, hashValid, hash, "Non-existent file should be skipped, producing same hash as valid-only")
}

func TestComputeContentHashFromPaths(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create test files
	file1 := filepath.Join(tmpDir, "file1.txt")
	file2 := filepath.Join(tmpDir, "file2.txt")

	require.NoError(t, os.WriteFile(file1, []byte("Content 1"), 0644))
	require.NoError(t, os.WriteFile(file2, []byte("Content 2"), 0644))

	// Test with empty paths
	hash, err := ComputeContentHashFromPaths(nil, nil, tmpDir)
	require.NoError(t, err)
	assert.Empty(t, hash, "Empty paths should produce empty hash")

	// Test with modified paths
	hash1, err := ComputeContentHashFromPaths([]string{file1, file2}, nil, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1, "Modified paths should produce non-empty hash")

	// Test with new paths
	hash2, err := ComputeContentHashFromPaths(nil, []string{file1}, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash2, "New paths should produce non-empty hash")
	assert.NotEqual(t, hash1, hash2, "Different file sets should produce different hashes")

	// Test with both modified and new
	hash3, err := ComputeContentHashFromPaths([]string{file1}, []string{file2}, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash3, "Mixed paths should produce non-empty hash")

	// Test determinism
	hash4, err := ComputeContentHashFromPaths([]string{file2, file1}, nil, tmpDir)
	require.NoError(t, err)
	assert.Equal(t, hash1, hash4, "Hash should be deterministic regardless of path order")
}

func TestComputeContentHashFromPaths_SkipsFilesOutsideRepo(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	outsideDir := t.TempDir() // Different temp directory

	// Create file inside repo
	insideFile := filepath.Join(tmpDir, "inside.txt")
	require.NoError(t, os.WriteFile(insideFile, []byte("Inside repo"), 0644))

	// Create file outside repo
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("Outside repo"), 0644))

	// Hash with both files - outside file should be skipped
	hash1, err := ComputeContentHashFromPaths([]string{insideFile, outsideFile}, nil, tmpDir)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1)

	// Hash with only inside file
	hash2, err := ComputeContentHashFromPaths([]string{insideFile}, nil, tmpDir)
	require.NoError(t, err)

	// Hashes should be the same since outside file was skipped
	assert.Equal(t, hash1, hash2, "Files outside repo should be skipped")
}
