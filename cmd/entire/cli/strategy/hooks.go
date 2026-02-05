package strategy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"entire.io/cli/cmd/entire/cli/gitutil"
	"entire.io/cli/cmd/entire/cli/paths"
)

// Hook marker used to identify Entire CLI hooks
const (
	entireHookMarker = "Entire CLI hooks"
	settingsFile     = ".entire/settings.json"
)

// gitHookNames are the git hooks managed by Entire CLI
var gitHookNames = []string{"prepare-commit-msg", "commit-msg", "post-commit", "pre-push"}

// IsGitHookInstalled checks if all generic Entire CLI hooks are installed.
func IsGitHookInstalled() bool {
	gitDir, err := gitutil.GetGitDirInPath(".")
	if err != nil {
		return false
	}
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

// InstallGitHook installs generic git hooks that delegate to `entire hook` commands.
// These hooks work with any strategy - the strategy is determined at runtime.
// If silent is true, no output is printed.
// Returns the number of hooks that were installed (0 if all already up to date).
func InstallGitHook(silent bool) (int, error) {
	gitDir, err := gitutil.GetGitDirInPath(".")
	if err != nil {
		return 0, fmt.Errorf("failed to get git dir: %w", err)
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

// RemoveGitHook removes all Entire CLI git hooks from the repository.
// Returns the number of hooks removed.
func RemoveGitHook() (int, error) {
	gitDir, err := gitutil.GetGitDirInPath(".")
	if err != nil {
		return 0, fmt.Errorf("failed to get git dir: %w", err)
	}

	removed := 0
	var removeErrors []string

	for _, hook := range gitHookNames {
		hookPath := filepath.Join(gitDir, "hooks", hook)
		data, err := os.ReadFile(hookPath) //nolint:gosec // path is controlled
		if err != nil {
			continue // Hook doesn't exist
		}

		if strings.Contains(string(data), entireHookMarker) {
			if err := os.Remove(hookPath); err != nil {
				removeErrors = append(removeErrors, fmt.Sprintf("%s: %v", hook, err))
			} else {
				removed++
			}
		}
	}

	if len(removeErrors) > 0 {
		return removed, fmt.Errorf("failed to remove hooks: %s", strings.Join(removeErrors, "; "))
	}
	return removed, nil
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
