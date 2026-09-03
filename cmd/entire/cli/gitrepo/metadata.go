package gitrepo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrWorktreeMetadataNotFound reports that an explicit worktree root has no
// .git entry. Repository opening uses this to distinguish a possible bare
// repository from malformed metadata in a checkout.
var ErrWorktreeMetadataNotFound = errors.New("worktree Git metadata not found")

// WorktreeMetadata contains Git directory facts for one explicit worktree
// root. Paths are cleaned and absolute but retain their lexical spelling;
// WorktreeID uses physical directory identity internally.
type WorktreeMetadata struct {
	GitDir     string
	CommonDir  string
	WorktreeID string
}

// ResolveWorktreeMetadata resolves filesystem metadata for an explicit
// worktree root. It does not discover a root, inspect Git environment
// variables, run Git, cache results, or decide feature-specific policy.
func ResolveWorktreeMetadata(worktreeRoot string) (WorktreeMetadata, error) {
	if strings.TrimSpace(worktreeRoot) == "" {
		return WorktreeMetadata{}, errors.New("worktree root is required")
	}

	root, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return WorktreeMetadata{}, fmt.Errorf("resolve worktree root %q: %w", worktreeRoot, err)
	}
	root = filepath.Clean(root)

	gitDir, err := resolveWorktreeGitDir(root)
	if err != nil {
		return WorktreeMetadata{}, err
	}
	commonDir, err := resolveWorktreeCommonDir(gitDir)
	if err != nil {
		return WorktreeMetadata{}, err
	}
	worktreeID, err := resolveWorktreeID(gitDir, commonDir)
	if err != nil {
		return WorktreeMetadata{}, err
	}

	return WorktreeMetadata{
		GitDir:     gitDir,
		CommonDir:  commonDir,
		WorktreeID: worktreeID,
	}, nil
}

func resolveWorktreeGitDir(worktreeRoot string) (string, error) {
	gitEntry := filepath.Join(worktreeRoot, gitDir)
	info, err := os.Lstat(gitEntry)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w at %s: %w", ErrWorktreeMetadataNotFound, gitEntry, err)
	}
	if err != nil {
		return "", fmt.Errorf("inspect .git entry at %s: %w", gitEntry, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		info, err = os.Stat(gitEntry)
		if err != nil {
			return "", fmt.Errorf("inspect .git entry at %s: %w", gitEntry, err)
		}
	}
	if info.IsDir() {
		return gitEntry, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf(".git entry at %s is neither a directory nor a regular file", gitEntry)
	}

	content, err := os.ReadFile(gitEntry) //nolint:gosec // path is anchored at the explicit worktree root.
	if err != nil {
		return "", fmt.Errorf("read .git file at %s: %w", gitEntry, err)
	}
	pointer, ok := strings.CutPrefix(strings.TrimRight(string(content), "\r\n"), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("parse .git file at %s: missing gitdir prefix", gitEntry)
	}
	if pointer == "" {
		return "", fmt.Errorf("parse .git file at %s: empty gitdir value", gitEntry)
	}

	resolved := resolveMetadataPath(worktreeRoot, pointer)
	if err := requireDirectory("Git directory", resolved); err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func resolveWorktreeCommonDir(gitDirPath string) (string, error) {
	commonFile := filepath.Join(gitDirPath, "commondir")
	info, err := os.Lstat(commonFile)
	if errors.Is(err, fs.ErrNotExist) {
		return gitDirPath, nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect commondir file at %s: %w", commonFile, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("commondir entry at %s is not a regular file", commonFile)
	}

	content, err := os.ReadFile(commonFile) //nolint:gosec // path is inside the validated per-worktree Git directory.
	if err != nil {
		return "", fmt.Errorf("read commondir file at %s: %w", commonFile, err)
	}
	pointer := strings.TrimRight(string(content), "\r\n")
	if pointer == "" {
		return "", fmt.Errorf("parse commondir file at %s: empty value", commonFile)
	}

	resolved := resolveMetadataPath(gitDirPath, pointer)
	if err := requireDirectory("common Git directory", resolved); err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func resolveWorktreeID(gitDirPath, commonDir string) (string, error) {
	same, err := directoriesIdentifySameFile(gitDirPath, commonDir)
	if err != nil {
		return "", fmt.Errorf("compare Git and common directories: %w", err)
	}
	if same {
		return "", nil
	}

	registrationRoot := filepath.Join(commonDir, "worktrees")
	same, err = directoriesIdentifySameFile(filepath.Dir(gitDirPath), registrationRoot)
	if err != nil {
		return "", fmt.Errorf("validate linked-worktree registration for %s: %w", gitDirPath, err)
	}
	if !same {
		return "", fmt.Errorf("git directory %s is not an immediate child of %s", gitDirPath, registrationRoot)
	}

	id := filepath.Base(gitDirPath)
	if id == "." || id == string(filepath.Separator) || id == "" {
		return "", fmt.Errorf("git directory %s has no worktree registration name", gitDirPath)
	}
	return id, nil
}

// Keep every component until after validation so malformed paths cannot use
// lexical cleaning to bypass a missing intermediate directory.
func resolveMetadataPath(base, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return base + string(filepath.Separator) + value
}

func requireDirectory(label, path string) error {
	info, err := os.Stat(path) //nolint:gosec // the explicit-root API intentionally resolves caller-selected repository metadata.
	if err != nil {
		return fmt.Errorf("inspect %s at %s: %w", label, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s at %s is not a directory", label, path)
	}
	return nil
}

func directoriesIdentifySameFile(a, b string) (bool, error) {
	aInfo, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", a, err)
	}
	if !aInfo.IsDir() {
		return false, fmt.Errorf("%s is not a directory", a)
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", b, err)
	}
	if !bInfo.IsDir() {
		return false, fmt.Errorf("%s is not a directory", b)
	}
	return os.SameFile(aInfo, bInfo), nil
}
