package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Hook marker used to identify Entire CLI hooks
const entireHookMarker = "Entire CLI hooks"

const backupSuffix = ".pre-entire"
const chainComment = "# Chain: run pre-existing hook"

// gitHookNames are the git hooks managed by Entire CLI
var gitHookNames = []string{"prepare-commit-msg", "commit-msg", "post-commit", "pre-push"}

// ManagedGitHookNames returns the list of git hooks managed by Entire CLI.
// This is useful for tests that need to manipulate hooks.
func ManagedGitHookNames() []string {
	return gitHookNames
}

// hookSpec defines a git hook's name and content template (without chain call).
type hookSpec struct {
	name    string
	content string
}

// GetGitDir returns the actual git directory path by delegating to git itself.
// This handles both regular repositories and worktrees, and inherits git's
// security validation for gitdir references.
func GetGitDir() (string, error) {
	return getGitDirInPath(".")
}

// getGitDirInPath returns the git directory for a repository at the given path.
// It delegates to `git rev-parse --git-dir` to leverage git's own validation.
func getGitDirInPath(dir string) (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", errors.New("not a git repository")
	}

	gitDir := strings.TrimSpace(string(output))

	// git rev-parse --git-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}

	return filepath.Clean(gitDir), nil
}

// IsGitHookInstalled checks if all generic Entire CLI hooks are installed.
func IsGitHookInstalled() bool {
	gitDir, err := GetGitDir()
	if err != nil {
		return false
	}
	return isGitHookInstalledInGitDir(gitDir)
}

// IsGitHookInstalledInDir checks if all Entire CLI hooks are installed in the given repo directory.
// This is useful for tests that need to check hooks without changing the working directory.
func IsGitHookInstalledInDir(repoDir string) bool {
	gitDir, err := getGitDirInPath(repoDir)
	if err != nil {
		return false
	}
	return isGitHookInstalledInGitDir(gitDir)
}

// isGitHookInstalledInGitDir checks if all hooks are installed in the given .git directory.
func isGitHookInstalledInGitDir(gitDir string) bool {
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(gitDir, "hooks", hook)
		data, err := os.ReadFile(hookPath) //nolint:gosec // Path is constructed from constants
		if err != nil {
			return false
		}
		if !strings.Contains(string(data), entireHookMarker) {
			return false
		}
	}
	return true
}

// buildHookSpecs returns the hook specifications for all managed hooks.
func buildHookSpecs(cmdPrefix string) []hookSpec {
	return []hookSpec{
		{
			name: "prepare-commit-msg",
			content: fmt.Sprintf(`#!/bin/sh
# %s
%s hooks git prepare-commit-msg "$1" "$2" 2>/dev/null || true
`, entireHookMarker, cmdPrefix),
		},
		{
			name: "commit-msg",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Commit-msg hook: strip trailer if no user content (allows aborting empty commits)
%s hooks git commit-msg "$1" || exit 1
`, entireHookMarker, cmdPrefix),
		},
		{
			name: "post-commit",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Post-commit hook: condense session data if commit has Entire-Checkpoint trailer
%s hooks git post-commit 2>/dev/null || true
`, entireHookMarker, cmdPrefix),
		},
		{
			name: "pre-push",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Pre-push hook: push session logs alongside user's push
# $1 is the remote name (e.g., "origin")
%s hooks git pre-push "$1" || true
`, entireHookMarker, cmdPrefix),
		},
	}
}

// InstallGitHook installs generic git hooks that delegate to `entire hook` commands.
// These hooks work with any strategy - the strategy is determined at runtime.
// If silent is true, no output is printed (except backup notifications, which always print).
// Returns the number of hooks that were installed (0 if all already up to date).
func InstallGitHook(silent bool) (int, error) {
	gitDir, err := GetGitDir()
	if err != nil {
		return 0, err
	}

	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil { //nolint:gosec // Git hooks require executable permissions
		return 0, fmt.Errorf("failed to create hooks directory: %w", err)
	}

	// Determine command prefix based on local_dev setting
	var cmdPrefix string
	if isLocalDev() {
		cmdPrefix = "go run ./cmd/entire/main.go"
	} else {
		cmdPrefix = "entire"
	}

	specs := buildHookSpecs(cmdPrefix)
	installedCount := 0

	for _, spec := range specs {
		hookPath := filepath.Join(hooksDir, spec.name)
		backupPath := hookPath + backupSuffix
		backupExists := fileExists(backupPath)

		// Back up existing non-Entire hooks
		existing, existingErr := os.ReadFile(hookPath) //nolint:gosec // path is controlled
		if existingErr == nil && !strings.Contains(string(existing), entireHookMarker) {
			if !backupExists {
				if err := os.Rename(hookPath, backupPath); err != nil {
					return installedCount, fmt.Errorf("failed to back up %s: %w", spec.name, err)
				}
				fmt.Fprintf(os.Stderr, "[entire] Backed up existing %s to %s%s\n", spec.name, spec.name, backupSuffix)
			} else {
				fmt.Fprintf(os.Stderr, "[entire] Warning: replacing %s (backup %s%s already exists from a previous install)\n", spec.name, spec.name, backupSuffix)
			}
			backupExists = true
		}

		// Chain to backup if one exists
		content := spec.content
		if backupExists {
			content = generateChainedContent(spec.content, spec.name)
		}

		written, err := writeHookFile(hookPath, content)
		if err != nil {
			return installedCount, fmt.Errorf("failed to install %s hook: %w", spec.name, err)
		}
		if written {
			installedCount++
		}
	}

	if !silent {
		fmt.Println("✓ Installed git hooks (prepare-commit-msg, commit-msg, post-commit, pre-push)")
		fmt.Println("  Hooks delegate to the current strategy at runtime")
	}

	return installedCount, nil
}

// writeHookFile writes a hook file if it doesn't exist or has different content.
// Returns true if the file was written, false if it already had the same content.
func writeHookFile(path, content string) (bool, error) {
	// Check if file already exists with same content
	existing, err := os.ReadFile(path) //nolint:gosec // path is controlled
	if err == nil && string(existing) == content {
		return false, nil // Already up to date
	}

	// Git hooks must be executable (0o755)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // Git hooks require executable permissions
		return false, fmt.Errorf("failed to write hook file %s: %w", path, err)
	}
	return true, nil
}

// RemoveGitHook removes all Entire CLI git hooks from the repository.
// If a .pre-entire backup exists, it is restored.
// Returns the number of hooks removed.
func RemoveGitHook() (int, error) {
	gitDir, err := GetGitDir()
	if err != nil {
		return 0, err
	}

	hooksDir := filepath.Join(gitDir, "hooks")
	removed := 0
	var removeErrors []string

	for _, hook := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hook)
		backupPath := hookPath + backupSuffix

		// Remove the hook if it contains our marker
		data, err := os.ReadFile(hookPath) //nolint:gosec // path is controlled
		hookIsOurs := err == nil && strings.Contains(string(data), entireHookMarker)
		hookExists := err == nil

		if hookIsOurs {
			if err := os.Remove(hookPath); err != nil {
				removeErrors = append(removeErrors, fmt.Sprintf("%s: %v", hook, err))
				continue
			}
			removed++
		}

		// Restore .pre-entire backup if it exists
		if fileExists(backupPath) {
			if hookExists && !hookIsOurs {
				// A non-Entire hook is present — don't overwrite it with the backup
				fmt.Fprintf(os.Stderr, "[entire] Warning: %s was modified since install; backup %s%s left in place\n", hook, hook, backupSuffix)
			} else {
				if err := os.Rename(backupPath, hookPath); err != nil {
					removeErrors = append(removeErrors, fmt.Sprintf("restore %s%s: %v", hook, backupSuffix, err))
				}
			}
		}
	}

	if len(removeErrors) > 0 {
		return removed, fmt.Errorf("failed to remove hooks: %s", strings.Join(removeErrors, "; "))
	}
	return removed, nil
}

// generateChainedContent appends a chain call to the base hook content,
// so the pre-existing hook (backed up to .pre-entire) is called after our hook.
func generateChainedContent(baseContent, hookName string) string {
	return baseContent + fmt.Sprintf(`%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/%s%s" ]; then
    "$_entire_hook_dir/%s%s" "$@"
fi
`, chainComment, hookName, backupSuffix, hookName, backupSuffix)
}

// isLocalDev reads the local_dev setting from .entire/settings.json
// Works correctly from any subdirectory within the repository.
func isLocalDev() bool {
	s, err := settings.Load()
	if err != nil {
		return false
	}
	return s.LocalDev
}
