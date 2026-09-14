// Package worktreeid owns stable Git worktree identity and namespace hashing.
package worktreeid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HashLength is the number of hexadecimal characters returned by Hash.
const HashLength = 6

// Hash returns the stable namespace key for a Git worktree identifier.
func Hash(worktreeID string) string {
	hash := sha256.Sum256([]byte(worktreeID))
	return hex.EncodeToString(hash[:])[:HashLength]
}

// Get returns the internal Git worktree identifier for worktreePath. The main
// worktree has an empty identifier; linked worktrees use their admin-dir name.
func Get(worktreePath string) (string, error) {
	gitPath := filepath.Join(worktreePath, ".git")

	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat .git: %w", err)
	}
	if info.IsDir() {
		return "", nil
	}

	content, err := os.ReadFile(gitPath) //nolint:gosec // gitPath is constructed from worktreePath + ".git"
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("invalid .git file format: %s", line)
	}

	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if id, found := parse(gitdir); found {
		return id, nil
	}

	resolved := resolveGitdirPath(worktreePath, gitdir)
	if isMainGitDir(resolved) {
		return "", nil
	}
	if isLinkedAdminDir(resolved) {
		normalized := strings.TrimSuffix(strings.ReplaceAll(gitdir, "\\", "/"), "/")
		if index := strings.LastIndex(normalized, "/worktrees/"); index >= 0 {
			return normalized[index+len("/worktrees/"):], nil
		}
	}
	return "", fmt.Errorf("unexpected gitdir format (no worktrees): %s", gitdir)
}

func resolveGitdirPath(worktreePath, gitdir string) string {
	gitdir = filepath.FromSlash(strings.ReplaceAll(gitdir, "\\", "/"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(worktreePath, gitdir)
	}
	return filepath.Clean(gitdir)
}

func isMainGitDir(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // path comes from the repository's own .git file
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil { //nolint:gosec // read-only repository probe
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "commondir")); err == nil { //nolint:gosec // read-only repository probe
		return false
	}
	return true
}

func isLinkedAdminDir(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // path comes from the repository's own .git file
	if err != nil || !info.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil { //nolint:gosec // read-only repository probe
		return false
	}
	_, err = os.Stat(filepath.Join(path, "commondir")) //nolint:gosec // read-only repository probe
	return err == nil
}

func parse(gitdir string) (string, bool) {
	gitdir = strings.TrimSuffix(strings.ReplaceAll(gitdir, "\\", "/"), "/")

	// A worktrees segment before the final modules segment belongs to the
	// superproject; only a segment after modules identifies this submodule's
	// linked worktree.
	if modulesIndex := strings.LastIndex(gitdir, "/modules/"); modulesIndex >= 0 {
		afterModules := gitdir[modulesIndex+len("/modules/"):]
		if _, id, found := strings.Cut(afterModules, "/worktrees/"); found {
			return strings.TrimSuffix(id, "/"), true
		}
		return "", true
	}

	for _, marker := range []string{".git/worktrees/", ".bare/worktrees/"} {
		if _, id, found := strings.Cut(gitdir, marker); found {
			return strings.TrimSuffix(id, "/"), true
		}
	}
	return "", false
}
