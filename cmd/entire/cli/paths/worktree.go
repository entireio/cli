package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GetWorktreeID returns the internal git worktree identifier for the given path.
// For the main worktree (where .git is a directory), returns empty string.
// For linked worktrees (where .git is a file), extracts the name from
// .git/worktrees/<name>/ path. This name is stable across `git worktree move`.
// For submodules (where .git is a file pointing to .git/modules/<name>),
// returns empty string since submodules behave like main worktrees.
func GetWorktreeID(worktreePath string) (string, error) {
	gitPath := filepath.Join(worktreePath, ".git")

	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat .git: %w", err)
	}

	// Main worktree has .git as a directory
	if info.IsDir() {
		return "", nil
	}

	// Linked worktree has .git as a file with content: "gitdir: /path/to/.git/worktrees/<name>"
	// Submodules also have .git as a file with content: "gitdir: ../.git/modules/<name>"
	content, err := os.ReadFile(gitPath) //nolint:gosec // gitPath is constructed from worktreePath + ".git"
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}

	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("invalid .git file format: %s", line)
	}

	gitdir := strings.TrimPrefix(line, "gitdir: ")

	// Submodules point to .git/modules/<name> — treat them like main worktrees
	// since they have their own independent git state.
	if isSubmoduleGitdir(gitdir) {
		return "", nil
	}

	// Extract worktree name from path ending in /worktrees/<name>.
	// This handles both standard worktrees (.git/worktrees/<name>) and
	// worktrees inside submodules (.git/modules/<submod>/worktrees/<name>).
	const marker = "/worktrees/"
	idx := strings.LastIndex(gitdir, marker)
	if idx < 0 {
		return "", fmt.Errorf("unexpected gitdir format (no worktrees): %s", gitdir)
	}
	worktreeID := gitdir[idx+len(marker):]
	// Remove trailing slashes if any
	worktreeID = strings.TrimSuffix(worktreeID, "/")

	return worktreeID, nil
}

// isSubmoduleGitdir checks if a gitdir path points to a submodule's git directory.
// Submodule gitdir paths contain "/modules/" and do NOT contain "/worktrees/".
// Examples:
//   - "../.git/modules/submod" → true
//   - "/repo/.git/modules/libs/crypto" → true (nested submodule)
//   - "/repo/.git/worktrees/feature" → false (linked worktree)
func isSubmoduleGitdir(gitdir string) bool {
	// Normalize to forward slashes for consistent matching
	normalized := filepath.ToSlash(gitdir)
	return strings.Contains(normalized, "/modules/") && !strings.Contains(normalized, "/worktrees/")
}
