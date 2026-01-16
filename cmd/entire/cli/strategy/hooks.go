package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/paths"
)

// Hook marker used to identify Entire CLI hooks
const (
	entireHookMarker = "Entire CLI hooks"
	settingsFile     = ".entire/settings.json"
)

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
	hooks := []string{"prepare-commit-msg", "commit-msg", "post-commit", "pre-push"}
	for _, hook := range hooks {
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

// InstallGitHook installs generic git hooks that delegate to `entire hook` commands.
// These hooks work with any strategy - the strategy is determined at runtime.
// If silent is true, no output is printed.
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
		// Use absolute path to currently-running binary (avoids depending on PATH being set
		// correctly when hooks run, also useful for local dev)
		execPath, err := os.Executable()
		if err != nil {
			return 0, fmt.Errorf("failed to get executable path: %w", err)
		}
		// Resolve any symlinks to get the real path
		execPath, err = filepath.EvalSymlinks(execPath)
		if err != nil {
			return 0, fmt.Errorf("failed to resolve executable path: %w", err)
		}
		cmdPrefix = execPath
	}

	installedCount := 0

	// Install prepare-commit-msg hook
	// $1 = commit message file, $2 = source (message, template, merge, squash, commit, or empty)
	prepareCommitMsgPath := filepath.Join(hooksDir, "prepare-commit-msg")
	prepareCommitMsgContent := fmt.Sprintf(`#!/bin/sh
# %s
%s hooks git prepare-commit-msg "$1" "$2" 2>/dev/null || true
`, entireHookMarker, cmdPrefix)

	written, err := writeHookFile(prepareCommitMsgPath, prepareCommitMsgContent)
	if err != nil {
		return 0, fmt.Errorf("failed to install prepare-commit-msg hook: %w", err)
	}
	if written {
		installedCount++
	}

	// Install commit-msg hook
	commitMsgPath := filepath.Join(hooksDir, "commit-msg")
	commitMsgContent := fmt.Sprintf(`#!/bin/sh
# %s
# Commit-msg hook: strip trailer if no user content (allows aborting empty commits)
%s hooks git commit-msg "$1" || exit 1
`, entireHookMarker, cmdPrefix)

	written, err = writeHookFile(commitMsgPath, commitMsgContent)
	if err != nil {
		return 0, fmt.Errorf("failed to install commit-msg hook: %w", err)
	}
	if written {
		installedCount++
	}

	// Install post-commit hook
	postCommitPath := filepath.Join(hooksDir, "post-commit")
	postCommitContent := fmt.Sprintf(`#!/bin/sh
# %s
# Post-commit hook: condense session data if commit has Entire-Checkpoint trailer
%s hooks git post-commit 2>/dev/null || true
`, entireHookMarker, cmdPrefix)

	written, err = writeHookFile(postCommitPath, postCommitContent)
	if err != nil {
		return 0, fmt.Errorf("failed to install post-commit hook: %w", err)
	}
	if written {
		installedCount++
	}

	// Install pre-push hook
	prePushPath := filepath.Join(hooksDir, "pre-push")
	prePushContent := fmt.Sprintf(`#!/bin/sh
# %s
# Pre-push hook: push session logs alongside user's push
# $1 is the remote name (e.g., "origin")
%s hooks git pre-push "$1" || true
`, entireHookMarker, cmdPrefix)

	written, err = writeHookFile(prePushPath, prePushContent)
	if err != nil {
		return 0, fmt.Errorf("failed to install pre-push hook: %w", err)
	}
	if written {
		installedCount++
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

// isLocalDev reads the local_dev setting from .entire/settings.json
// Works correctly from any subdirectory within the repository.
func isLocalDev() bool {
	settingsFileAbs, err := paths.AbsPath(settingsFile)
	if err != nil {
		settingsFileAbs = settingsFile // Fallback to relative
	}
	data, err := os.ReadFile(settingsFileAbs) //nolint:gosec // path is from AbsPath or constant
	if err != nil {
		return false
	}
	var settings struct {
		LocalDev bool `json:"local_dev"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	return settings.LocalDev
}
