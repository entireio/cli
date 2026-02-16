package contenthash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashFileContent(t *testing.T) {
	t.Parallel()

	// Create temporary directory
	tmpDir := t.TempDir()

	// Create test file
	testFile := filepath.Join(tmpDir, "test.txt")
	content := []byte("Hello, World!\n")
	require.NoError(t, os.WriteFile(testFile, content, 0644))

	// Hash the file
	hash, err := HashFileContent(testFile)
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	// Hash should be consistent
	hash2, err := HashFileContent(testFile)
	require.NoError(t, err)
	assert.Equal(t, hash, hash2, "Hash should be consistent for same content")

	// Different content should produce different hash
	testFile2 := filepath.Join(tmpDir, "test2.txt")
	require.NoError(t, os.WriteFile(testFile2, []byte("Different content\n"), 0644))
	hash3, err := HashFileContent(testFile2)
	require.NoError(t, err)
	assert.NotEqual(t, hash, hash3, "Different content should produce different hash")
}

func TestHashFileContent_NonExistentFile(t *testing.T) {
	t.Parallel()

	hash, err := HashFileContent("/non/existent/file.txt")
	require.Error(t, err)
	assert.Empty(t, hash)
	assert.Contains(t, err.Error(), "failed to open file")
}

func TestHashFileWithPath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create two files with same content but different paths
	file1 := filepath.Join(tmpDir, "dir1", "file.txt")
	file2 := filepath.Join(tmpDir, "dir2", "file.txt")

	require.NoError(t, os.MkdirAll(filepath.Dir(file1), 0755))
	require.NoError(t, os.MkdirAll(filepath.Dir(file2), 0755))

	content := []byte("Same content\n")
	require.NoError(t, os.WriteFile(file1, content, 0644))
	require.NoError(t, os.WriteFile(file2, content, 0644))

	// Hash with different relative paths
	hash1, err := HashFileWithPath(file1, "dir1/file.txt")
	require.NoError(t, err)

	hash2, err := HashFileWithPath(file2, "dir2/file.txt")
	require.NoError(t, err)

	// Hashes should differ because paths are different
	assert.NotEqual(t, hash1, hash2, "Same content with different paths should produce different hashes")
}

func TestHashFileWithPath_ExecutableBit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "script.sh")

	content := []byte("#!/bin/bash\necho 'Hello'\n")
	require.NoError(t, os.WriteFile(testFile, content, 0644))

	// Hash without executable bit
	hash1, err := HashFileWithPath(testFile, "script.sh")
	require.NoError(t, err)

	// Make file executable
	require.NoError(t, os.Chmod(testFile, 0755))

	// Hash with executable bit
	hash2, err := HashFileWithPath(testFile, "script.sh")
	require.NoError(t, err)

	// Hashes should differ due to executable bit
	assert.NotEqual(t, hash1, hash2, "Executable bit should affect hash")
}

func TestIsLargeFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create small file
	smallFile := filepath.Join(tmpDir, "small.txt")
	require.NoError(t, os.WriteFile(smallFile, []byte("Small content"), 0644))

	isLarge, err := IsLargeFile(smallFile, 1) // 1MB threshold
	require.NoError(t, err)
	assert.False(t, isLarge, "Small file should not be large")

	// Create "large" file (for testing, just slightly over threshold)
	largeFile := filepath.Join(tmpDir, "large.bin")
	largeContent := make([]byte, 2*1024) // 2KB
	require.NoError(t, os.WriteFile(largeFile, largeContent, 0644))

	// Use tiny threshold for testing
	isLarge, err = IsLargeFile(largeFile, 0) // 0MB threshold (essentially 1 byte)
	require.NoError(t, err)
	assert.True(t, isLarge, "File over threshold should be large")
}

func TestShouldHashFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{"Regular file", "src/main.go", true},
		{"Gitignore", ".gitignore", true},
		{"Gitattributes", ".gitattributes", true},
		{"Hidden file", ".hidden", false},
		{"Hidden directory file", ".config/file.txt", false},
		{"Python cache", "file.pyc", false},
		{"Object file", "main.o", false},
		{"Shared library", "lib.so", false},
		{"Executable", "app.exe", false},
		{"DLL", "lib.dll", false},
		{"Node modules", "node_modules/package/index.js", false},
		{"Python venv", "venv/lib/python3.9/site.py", false},
		{"Python cache dir", "__pycache__/module.pyc", false},
		{"Build directory", "build/output.js", false},
		{"Dist directory", "dist/bundle.js", false},
		{"Target directory", "target/classes/Main.class", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ShouldHashFile(tt.path)
			assert.Equal(t, tt.expected, result, "ShouldHashFile(%s) = %v, want %v", tt.path, result, tt.expected)
		})
	}
}

func TestHashFileWithMetadata(t *testing.T) {
	t.Parallel()

	// Test with regular file
	hash1 := HashFileWithMetadata("test.txt", "content", 0644)
	assert.NotEmpty(t, hash1)

	// Test with executable file
	hash2 := HashFileWithMetadata("test.sh", "content", 0755)
	assert.NotEmpty(t, hash2)

	// Different permissions should produce different hash
	assert.NotEqual(t, hash1, hash2, "Different permissions should produce different hash")

	// Different path should produce different hash
	hash3 := HashFileWithMetadata("other.txt", "content", 0644)
	assert.NotEqual(t, hash1, hash3, "Different path should produce different hash")

	// Different content should produce different hash
	hash4 := HashFileWithMetadata("test.txt", "different", 0644)
	assert.NotEqual(t, hash1, hash4, "Different content should produce different hash")
}
