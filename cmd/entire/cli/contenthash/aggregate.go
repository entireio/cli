package contenthash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ComputeAggregateContentHash creates deterministic hash from file changes
// Critical: Uses sorted file paths for rebase resilience
func ComputeAggregateContentHash(fileHashes map[string]string) string {
	if len(fileHashes) == 0 {
		return "" // Empty changeset
	}

	// Sort files by path for deterministic ordering across rebases
	var sortedPairs []string
	for relPath, hash := range fileHashes {
		sortedPairs = append(sortedPairs, fmt.Sprintf("%s:%s", relPath, hash))
	}
	sort.Strings(sortedPairs)

	// Create aggregate hash: SHA256(path1:hash1|path2:hash2|...)
	hasher := sha256.New()
	for _, pair := range sortedPairs {
		hasher.Write([]byte(pair + "|"))
	}

	return hex.EncodeToString(hasher.Sum(nil))
}

// ComputeAggregateFromLists creates deterministic hash from separate file lists
// Useful when you have modified/new/deleted files tracked separately
func ComputeAggregateFromLists(modified, added, deleted []string, hashFunc func(string) (string, error)) (string, error) {
	if len(modified)+len(added)+len(deleted) == 0 {
		return "", nil // No changes
	}

	fileHashes := make(map[string]string)

	// Hash modified files
	for _, path := range modified {
		hash, err := hashFunc(path)
		if err != nil {
			return "", fmt.Errorf("failed to hash modified file %s: %w", path, err)
		}
		fileHashes[path] = hash
	}

	// Hash added files
	for _, path := range added {
		hash, err := hashFunc(path)
		if err != nil {
			return "", fmt.Errorf("failed to hash new file %s: %w", path, err)
		}
		fileHashes[path] = hash
	}

	// For deleted files, use a special marker
	for _, path := range deleted {
		fileHashes[path] = "DELETED"
	}

	return ComputeAggregateContentHash(fileHashes), nil
}

// NormalizeContentHash ensures consistent formatting of content hashes
func NormalizeContentHash(hash string) string {
	// Remove any sha256: prefix if present
	hash = strings.TrimPrefix(hash, "sha256:")

	// Lowercase for consistency
	return strings.ToLower(hash)
}

// FormatContentHash formats a content hash for display/storage
func FormatContentHash(hash string) string {
	if hash == "" {
		return ""
	}

	// Ensure it's normalized
	hash = NormalizeContentHash(hash)

	// Add sha256: prefix for clarity
	return "sha256:" + hash
}

// ValidateContentHash checks if a content hash is valid SHA256
func ValidateContentHash(hash string) bool {
	// Remove prefix if present
	hash = NormalizeContentHash(hash)

	// Check length (SHA256 is 64 hex characters)
	if len(hash) != 64 {
		return false
	}

	// Check if all characters are valid hex
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return true
}
