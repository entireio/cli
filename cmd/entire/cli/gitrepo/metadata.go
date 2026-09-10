package gitrepo

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxMetadataPointerSize matches git's own refusal threshold for .git pointer
// files (setup.c caps at 4*PATH_MAX with "too large to be a .git file").
// Anything larger is not repository metadata, and reading it unbounded turns a
// sparse pointer file of near-zero disk size into gigabytes of allocation.
const maxMetadataPointerSize = 4 * 4096

// maxMetadataErrorPathLen bounds how much of a pointer-derived path an error
// message echoes; a hostile pointer must not become a multi-kilobyte error.
const maxMetadataErrorPathLen = 256

// ErrWorktreeMetadataNotFound reports that an explicit worktree root has no
// .git entry. Repository opening uses it to distinguish a possible bare
// repository from broken worktree metadata.
var ErrWorktreeMetadataNotFound = errors.New("worktree Git metadata not found")

// WorktreeMetadata contains Git directory facts for one explicit worktree
// root. Paths are cleaned and absolute but retain their lexical spelling.
type WorktreeMetadata struct {
	GitDir     string
	CommonDir  string
	WorktreeID string
}

// ResolveWorktreeMetadata resolves filesystem metadata for an explicit
// worktree root. It does not discover a root, inspect Git environment
// variables, run Git, cache results, or decide caller policy.
func ResolveWorktreeMetadata(worktreeRoot string) (WorktreeMetadata, error) {
	if worktreeRoot == "" {
		return WorktreeMetadata{}, errors.New("worktree root is required")
	}

	root, err := filepath.Abs(worktreeRoot)
	if err != nil {
		return WorktreeMetadata{}, fmt.Errorf("resolve worktree root %q: %w", worktreeRoot, err)
	}
	root = filepath.Clean(root)
	if err := requireMetadataDirectory("worktree root", root); err != nil {
		return WorktreeMetadata{}, err
	}

	gitDirPath, err := resolveWorktreeGitDir(root)
	if err != nil {
		return WorktreeMetadata{}, err
	}
	commonDir, err := resolveWorktreeCommonDir(gitDirPath)
	if err != nil {
		return WorktreeMetadata{}, err
	}
	worktreeID, err := resolveWorktreeID(gitDirPath, commonDir)
	if err != nil {
		return WorktreeMetadata{}, err
	}

	return WorktreeMetadata{
		GitDir:     gitDirPath,
		CommonDir:  commonDir,
		WorktreeID: worktreeID,
	}, nil
}

func resolveWorktreeGitDir(worktreeRoot string) (string, error) {
	gitEntry := filepath.Join(worktreeRoot, gitDir)
	info, err := os.Lstat(gitEntry)
	if errors.Is(err, fs.ErrNotExist) {
		if rootErr := requireMetadataDirectory("worktree root", worktreeRoot); rootErr != nil {
			return "", rootErr
		}
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

	content, err := readMetadataPointerFile(".git", gitEntry)
	if err != nil {
		return "", err
	}
	pointer, ok := strings.CutPrefix(strings.TrimRight(content, "\r\n"), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("parse .git file at %s: missing gitdir prefix", gitEntry)
	}
	if pointer == "" {
		return "", fmt.Errorf("parse .git file at %s: empty gitdir value", gitEntry)
	}

	resolved, err := resolveMetadataDirectory("Git directory", worktreeRoot, pointer)
	if err != nil {
		return "", err
	}
	return resolved, nil
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

	content, err := readMetadataPointerFile("commondir", commonFile)
	if err != nil {
		return "", err
	}
	pointer := strings.TrimRight(content, "\r\n")
	if pointer == "" {
		return "", fmt.Errorf("parse commondir file at %s: empty value", commonFile)
	}

	resolved, err := resolveMetadataDirectory("common Git directory", gitDirPath, pointer)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func resolveWorktreeID(gitDirPath, commonDir string) (string, error) {
	same, err := metadataDirectoriesIdentifySameFile(gitDirPath, commonDir)
	if err != nil {
		return "", fmt.Errorf("compare Git and common directories: %w", err)
	}
	if same {
		return "", nil
	}

	registrationRoot := filepath.Join(commonDir, "worktrees")
	registrationPath, err := filepath.EvalSymlinks(gitDirPath)
	if err != nil {
		return "", fmt.Errorf("resolve linked-worktree registration %s: %w", gitDirPath, err)
	}
	same, err = metadataDirectoriesIdentifySameFile(filepath.Dir(registrationPath), registrationRoot)
	if err != nil {
		return "", fmt.Errorf("validate linked-worktree registration for %s: %w", gitDirPath, err)
	}
	if !same {
		return "", fmt.Errorf("git directory %s is not an immediate child of %s", gitDirPath, registrationRoot)
	}

	id := filepath.Base(registrationPath)
	if id == "." || id == string(filepath.Separator) || id == "" {
		return "", fmt.Errorf("git directory %s has no worktree registration name", gitDirPath)
	}
	return id, nil
}

// readMetadataPointerFile reads a .git or commondir pointer file with git's own
// size cap, so a hostile pointer file cannot force an unbounded allocation.
func readMetadataPointerFile(kind, path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // path is anchored at validated repository metadata.
	if err != nil {
		return "", fmt.Errorf("read %s file at %s: %w", kind, path, err)
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(io.LimitReader(file, maxMetadataPointerSize+1))
	if err != nil {
		return "", fmt.Errorf("read %s file at %s: %w", kind, path, err)
	}
	if len(content) > maxMetadataPointerSize {
		return "", fmt.Errorf("parse %s file at %s: too large to be a %s file", kind, path, kind)
	}
	return string(content), nil
}

// elideMetadataPath bounds a pointer-derived path for error display. Elision
// backs up to a rune boundary so the message stays valid UTF-8.
func elideMetadataPath(path string) string {
	if len(path) <= maxMetadataErrorPathLen {
		return path
	}
	cut := maxMetadataErrorPathLen
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… (%d bytes elided)", path[:cut], len(path)-cut)
}

// Preserve the pointer spelling until validation so a missing component cannot
// disappear through lexical cleaning before the filesystem sees it.
func resolveMetadataPath(base, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return base + string(filepath.Separator) + value
}

func resolveMetadataDirectory(label, base, value string) (string, error) {
	resolved := resolveMetadataPath(base, value)
	if err := requireMetadataDirectory(label, resolved); err != nil {
		return "", err
	}

	cleaned := filepath.Clean(resolved)
	if cleaned == resolved {
		return cleaned, nil
	}
	// Cleaning a path through a symlink before resolving ".." can change which
	// directory it names, so keep the lexical result only when identity agrees.
	if same, err := metadataDirectoriesIdentifySameFile(resolved, cleaned); err == nil && same {
		return cleaned, nil
	}

	physical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve %s at %s: %w", label, elideMetadataPath(resolved), err)
	}
	return filepath.Clean(physical), nil
}

// The path may carry a pointer file's contents verbatim. The pointer size cap
// is what bounds the message; eliding here keeps the readable part readable
// instead of burying it under a padded path.
func requireMetadataDirectory(label, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect %s at %s: %w", label, elideMetadataPath(path), err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s at %s is not a directory", label, elideMetadataPath(path))
	}
	return nil
}

func metadataDirectoriesIdentifySameFile(a, b string) (bool, error) {
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
