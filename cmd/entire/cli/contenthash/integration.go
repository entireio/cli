package contenthash

import (
	"fmt"
	"path/filepath"
)

// FileChanges represents categorized file changes for content hashing.
// This mirrors cli.FileChanges to avoid import cycles.
type FileChanges struct {
	Modified []string // Modified or staged files
	New      []string // Untracked files
	Deleted  []string // Deleted files (not used in content hashing)
}

// ComputeContentHashForChanges integrates with DetectFileChanges from state.go
func ComputeContentHashForChanges(changes *FileChanges, repoRoot string) (string, error) {
	if changes == nil || (len(changes.Modified)+len(changes.New)) == 0 {
		return "", nil // No content to hash
	}

	fileHashes := make(map[string]string)

	// Hash modified files
	for _, relPath := range changes.Modified {
		// Skip files we shouldn't hash
		if !ShouldHashFile(relPath) {
			continue
		}

		absPath := filepath.Join(repoRoot, relPath)

		// Check if file is too large (skip files > 10MB by default)
		isLarge, err := IsLargeFile(absPath, 10)
		if err != nil {
			// File might have been deleted between detection and hashing
			continue
		}
		if isLarge {
			// Skip large files to maintain performance
			continue
		}

		hash, err := HashFileWithPath(absPath, relPath)
		if err != nil {
			return "", fmt.Errorf("failed to hash modified file %s: %w", relPath, err)
		}
		fileHashes[relPath] = hash
	}

	// Hash new files
	for _, relPath := range changes.New {
		// Skip files we shouldn't hash
		if !ShouldHashFile(relPath) {
			continue
		}

		absPath := filepath.Join(repoRoot, relPath)

		// Check if file is too large
		isLarge, err := IsLargeFile(absPath, 10)
		if err != nil {
			continue
		}
		if isLarge {
			continue
		}

		hash, err := HashFileWithPath(absPath, relPath)
		if err != nil {
			return "", fmt.Errorf("failed to hash new file %s: %w", relPath, err)
		}
		fileHashes[relPath] = hash
	}

	// Deleted files excluded - don't contribute to content hash
	// This is intentional: if a file is deleted, the content hash should change
	// based on what remains, not what was removed

	return ComputeAggregateContentHash(fileHashes), nil
}

// ComputeContentHashFromPaths computes content hash from file paths directly
// This is useful when you have absolute paths instead of FileChanges
func ComputeContentHashFromPaths(modifiedPaths, newPaths []string, repoRoot string) (string, error) {
	if len(modifiedPaths)+len(newPaths) == 0 {
		return "", nil // No content to hash
	}

	fileHashes := make(map[string]string)

	// Process modified files
	for _, absPath := range modifiedPaths {
		relPath, err := filepath.Rel(repoRoot, absPath)
		if err != nil {
			continue // Skip files outside repo
		}

		if !ShouldHashFile(relPath) {
			continue
		}

		// Check if file is too large
		isLarge, err := IsLargeFile(absPath, 10)
		if err != nil {
			continue
		}
		if isLarge {
			continue
		}

		hash, err := HashFileWithPath(absPath, relPath)
		if err != nil {
			return "", fmt.Errorf("failed to hash modified file %s: %w", relPath, err)
		}
		fileHashes[relPath] = hash
	}

	// Process new files
	for _, absPath := range newPaths {
		relPath, err := filepath.Rel(repoRoot, absPath)
		if err != nil {
			continue
		}

		if !ShouldHashFile(relPath) {
			continue
		}

		isLarge, err := IsLargeFile(absPath, 10)
		if err != nil {
			continue
		}
		if isLarge {
			continue
		}

		hash, err := HashFileWithPath(absPath, relPath)
		if err != nil {
			return "", fmt.Errorf("failed to hash new file %s: %w", relPath, err)
		}
		fileHashes[relPath] = hash
	}

	return ComputeAggregateContentHash(fileHashes), nil
}
