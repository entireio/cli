package contenthash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HashFileContent computes SHA256 hash of file content only (not path or metadata)
func HashFileContent(filePath string) (string, error) {
	file, err := os.Open(filePath) //nolint:gosec // filePath comes from git status, not user input
	if err != nil {
		return "", fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("failed to hash file %s: %w", filePath, err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// HashFileWithMetadata includes file path in hash for move/rename detection
func HashFileWithMetadata(relPath, content string, mode os.FileMode) string {
	hasher := sha256.New()

	// Include relative path and executable bit in hash
	fmt.Fprintf(hasher, "path:%s|exec:%t|", relPath, mode&0o111 != 0)
	hasher.Write([]byte(content))

	return hex.EncodeToString(hasher.Sum(nil))
}

// HashFileWithPath computes hash including the relative path for better change tracking
func HashFileWithPath(absPath, relPath string) (string, error) {
	file, err := os.Open(absPath) //nolint:gosec // absPath comes from git status, not user input
	if err != nil {
		return "", fmt.Errorf("failed to open file %s: %w", absPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file %s: %w", absPath, err)
	}

	hasher := sha256.New()

	// Include relative path and executable bit in hash
	fmt.Fprintf(hasher, "path:%s|exec:%t|", relPath, info.Mode()&0o111 != 0)

	// Copy file content
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("failed to hash file %s: %w", absPath, err)
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// IsLargeFile checks if a file exceeds the size threshold for hashing
func IsLargeFile(filePath string, thresholdMB int) (bool, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return false, fmt.Errorf("failed to stat file %s: %w", filePath, err)
	}

	thresholdBytes := int64(thresholdMB * 1024 * 1024)
	return info.Size() > thresholdBytes, nil
}

// ShouldHashFile determines if a file should be included in content hashing
func ShouldHashFile(relPath string) bool {
	// Skip hidden files and directories (except .gitignore, .gitattributes, etc)
	base := filepath.Base(relPath)
	if base != ".gitignore" && base != ".gitattributes" && len(base) > 0 && base[0] == '.' {
		return false // Skip hidden files
	}

	// Check if file is in a hidden directory
	for dir := filepath.Dir(relPath); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
		if filepath.Base(dir)[0] == '.' {
			return false // Skip files in hidden directories
		}
	}

	// Skip common build artifacts
	switch filepath.Ext(relPath) {
	case ".pyc", ".pyo", ".so", ".o", ".a", ".exe", ".dll":
		return false
	}

	// Skip node_modules and similar
	for _, part := range strings.Split(relPath, string(filepath.Separator)) {
		switch part {
		case "node_modules", "venv", ".venv", "__pycache__", "target", "dist", "build":
			return false
		}
	}

	return true
}
