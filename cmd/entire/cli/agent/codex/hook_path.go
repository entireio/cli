package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func validateWorktreeHookTarget(hooks WorktreeHooksPath) (string, error) {
	if hooks.worktreeRoot == "" || hooks.path == "" {
		return "", errors.New("invalid empty worktree Codex hooks path")
	}
	rootInfo, err := os.Stat(hooks.worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("inspect Codex hooks checkout: %w", err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("codex hooks checkout %q is not a directory", hooks.worktreeRoot)
	}

	projectDir := filepath.Join(hooks.worktreeRoot, ".codex")
	expectedPath := filepath.Join(projectDir, HooksFileName)
	if hooks.path != expectedPath {
		return "", fmt.Errorf(
			"invalid worktree Codex hooks path %q: expected %q",
			hooks.path,
			expectedPath,
		)
	}
	return projectDir, nil
}

// validateMutableHookTarget permits a missing local project layer, but rejects
// redirected directories and files before a lifecycle command writes to them.
func validateMutableHookTarget(hooks WorktreeHooksPath) error {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return err
	}
	if err := validateExistingProjectDir(projectDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	info, err := os.Lstat(hooks.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex hooks file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("codex hooks path %q is not a regular file", hooks.Path())
	}
	return nil
}

func validateDiscoveredHookTarget(hooks DiscoveredHooksPath) error {
	if hooks.path == "" {
		return errors.New("invalid empty discovered Codex hooks path")
	}
	root := filepath.Dir(filepath.Dir(hooks.path))
	expectedPath := filepath.Join(root, ".codex", HooksFileName)
	if hooks.path != expectedPath {
		return fmt.Errorf("invalid Codex-discovered hooks path %q: expected %q", hooks.path, expectedPath)
	}
	if err := validateExistingProjectDir(filepath.Dir(hooks.path)); err != nil {
		return err
	}
	return nil
}

func validateExistingProjectDir(projectDir string) error {
	info, err := os.Lstat(projectDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("inspect repository .codex directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository .codex path %q is not a directory", projectDir)
	}
	return nil
}
