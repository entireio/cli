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
	return validateProjectHookTarget(hooks.worktreeRoot, hooks.path, "worktree")
}

// validateProjectHookTarget checks that path is exactly root/.codex/hooks.json
// for an existing root directory and returns the .codex directory path.
func validateProjectHookTarget(root, path, kind string) (string, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect Codex hooks checkout: %w", err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("codex hooks checkout %q is not a directory", root)
	}

	projectDir := filepath.Join(root, ".codex")
	expectedPath := filepath.Join(projectDir, HooksFileName)
	if path != expectedPath {
		return "", fmt.Errorf("invalid %s Codex hooks path %q: expected %q", kind, path, expectedPath)
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
	return validateMutableHooksFile(projectDir, hooks.Path())
}

// validateMutableDiscoveredHookTarget applies the same checks to the
// Codex-discovered file, using the root resolveHookDiscovery recorded rather
// than one derived from the path. Only a consented install reaches it.
func validateMutableDiscoveredHookTarget(hooks DiscoveredHooksPath) error {
	if hooks.root == "" || hooks.path == "" {
		return errors.New("invalid empty discovered Codex hooks path")
	}
	projectDir, err := validateProjectHookTarget(hooks.root, hooks.path, "Codex-discovered")
	if err != nil {
		return err
	}
	return validateMutableHooksFile(projectDir, hooks.path)
}

func validateMutableHooksFile(projectDir, hooksPath string) error {
	if err := validateExistingProjectDir(projectDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	info, err := os.Lstat(hooksPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex hooks file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("codex hooks path %q is not a regular file", hooksPath)
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
