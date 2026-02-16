package contenthash

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeAggregateContentHash(t *testing.T) {
	t.Parallel()

	// Test empty map
	hash := ComputeAggregateContentHash(nil)
	assert.Empty(t, hash, "Empty map should produce empty hash")

	hash = ComputeAggregateContentHash(map[string]string{})
	assert.Empty(t, hash, "Empty map should produce empty hash")

	// Test single file
	fileHashes := map[string]string{
		"file1.txt": "abc123def456",
	}
	hash1 := ComputeAggregateContentHash(fileHashes)
	assert.NotEmpty(t, hash1, "Single file should produce non-empty hash")

	// Test multiple files - order shouldn't matter
	fileHashes2 := map[string]string{
		"file2.txt": "789ghi012jkl",
		"file1.txt": "abc123def456",
		"file3.txt": "mno345pqr678",
	}
	hash2 := ComputeAggregateContentHash(fileHashes2)
	assert.NotEmpty(t, hash2, "Multiple files should produce non-empty hash")

	// Test deterministic ordering
	fileHashes3 := map[string]string{
		"file3.txt": "mno345pqr678",
		"file1.txt": "abc123def456",
		"file2.txt": "789ghi012jkl",
	}
	hash3 := ComputeAggregateContentHash(fileHashes3)
	assert.Equal(t, hash2, hash3, "Hash should be deterministic regardless of input order")

	// Different content should produce different hash
	fileHashes4 := map[string]string{
		"file1.txt": "different_hash",
		"file2.txt": "789ghi012jkl",
		"file3.txt": "mno345pqr678",
	}
	hash4 := ComputeAggregateContentHash(fileHashes4)
	assert.NotEqual(t, hash2, hash4, "Different content should produce different hash")
}

func TestComputeAggregateFromLists(t *testing.T) {
	t.Parallel()

	// Mock hash function for testing
	mockHashFunc := func(path string) (string, error) {
		// Return a predictable hash based on the filename
		return "hash_of_" + path, nil
	}

	// Test with empty lists
	hash, err := ComputeAggregateFromLists(nil, nil, nil, mockHashFunc)
	require.NoError(t, err)
	assert.Empty(t, hash, "Empty lists should produce empty hash")

	// Test with modified files only
	modified := []string{"file1.txt", "file2.txt"}
	hash1, err := ComputeAggregateFromLists(modified, nil, nil, mockHashFunc)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1, "Modified files should produce non-empty hash")

	// Test with new files only
	newFiles := []string{"new1.txt", "new2.txt"}
	hash2, err := ComputeAggregateFromLists(nil, newFiles, nil, mockHashFunc)
	require.NoError(t, err)
	assert.NotEmpty(t, hash2, "New files should produce non-empty hash")
	assert.NotEqual(t, hash1, hash2, "Different file sets should produce different hashes")

	// Test with deleted files
	deleted := []string{"deleted1.txt", "deleted2.txt"}
	hash3, err := ComputeAggregateFromLists(nil, nil, deleted, mockHashFunc)
	require.NoError(t, err)
	assert.NotEmpty(t, hash3, "Deleted files should produce non-empty hash")

	// Test with all types
	hash4, err := ComputeAggregateFromLists(modified, newFiles, deleted, mockHashFunc)
	require.NoError(t, err)
	assert.NotEmpty(t, hash4, "Mixed file types should produce non-empty hash")
}

func TestComputeAggregateFromLists_ErrorHandling(t *testing.T) {
	t.Parallel()

	// Mock hash function that returns error
	errorHashFunc := func(path string) (string, error) {
		if path == "bad_file.txt" {
			return "", assert.AnError
		}
		return "hash_of_" + path, nil
	}

	// Test error in modified files
	modified := []string{"good.txt", "bad_file.txt"}
	_, err := ComputeAggregateFromLists(modified, nil, nil, errorHashFunc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to hash modified file")

	// Test error in new files
	newFiles := []string{"bad_file.txt"}
	_, err = ComputeAggregateFromLists(nil, newFiles, nil, errorHashFunc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to hash new file")
}

func TestNormalizeContentHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Already normalized", "abc123def456", "abc123def456"},
		{"With sha256 prefix", "sha256:abc123def456", "abc123def456"},
		{"Uppercase", "ABC123DEF456", "abc123def456"},
		{"Mixed case with prefix", "sha256:AbC123DeF456", "abc123def456"},
		{"Empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := NormalizeContentHash(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatContentHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Raw hash", "abc123def456", "sha256:abc123def456"},
		{"Already formatted", "sha256:abc123def456", "sha256:abc123def456"},
		{"Uppercase", "ABC123DEF456", "sha256:abc123def456"},
		{"Empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := FormatContentHash(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidateContentHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"Valid SHA256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"Valid with prefix", "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"Valid uppercase", "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", true},
		{"Too short", "abc123", false},
		{"Too long", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8551234", false},
		{"Invalid characters", "g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		{"Empty string", "", false},
		{"Wrong length", "abc", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := ValidateContentHash(tt.input)
			assert.Equal(t, tt.expected, result, "ValidateContentHash(%s) = %v, want %v", tt.input, result, tt.expected)
		})
	}
}
