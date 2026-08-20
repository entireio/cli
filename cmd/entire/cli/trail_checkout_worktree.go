package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing/format/gitignore"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const (
	trailWorktreesRelDir      = ".entire/worktrees"
	trailWorktreeFallbackName = "branch"
)

func defaultTrailWorktreePath(repoRoot, branch string, trailNumber int) string {
	name := fmt.Sprintf("trail-%d-%s", trailNumber, sanitizeTrailWorktreeName(branch))
	return filepath.Join(repoRoot, filepath.FromSlash(trailWorktreesRelDir), name)
}

func defaultReviewWorktreePath(repoRoot, branch string) string {
	digest := sha256.Sum256([]byte(branch))
	name := fmt.Sprintf("review-%s-%x", sanitizeTrailWorktreeName(branch), digest[:4])
	return filepath.Join(repoRoot, filepath.FromSlash(trailWorktreesRelDir), name)
}

func sanitizeTrailWorktreeName(branch string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, strings.TrimSpace(branch))
	name = strings.Trim(name, "-.")
	if name == "" {
		return trailWorktreeFallbackName
	}
	return name
}

// gitCommonDirForTrailWorktree returns the absolute git common dir, which is
// the main repo's .git directory even when run from a linked worktree.
// session.GetGitCommonDir is not reused here because it returns relative
// rev-parse results as-is; this feature needs an absolute path for the
// worktree location and the printed cd hint.
func gitCommonDirForTrailWorktree(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", gitOutputError("failed to get git common dir", err)
	}
	gitDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(gitDir) {
		cwd, wdErr := os.Getwd() //nolint:forbidigo // must resolve relative git common dir in cwd context
		if wdErr != nil {
			return "", fmt.Errorf("failed to get current directory: %w", wdErr)
		}
		gitDir = filepath.Join(cwd, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

func trailWorktreeBaseRoot(ctx context.Context) (string, error) {
	gitDir, err := gitCommonDirForTrailWorktree(ctx)
	if err != nil {
		return "", err
	}
	if filepath.Base(gitDir) != ".git" {
		return "", fmt.Errorf("git common dir %q is not a .git directory", gitDir)
	}
	return filepath.Dir(gitDir), nil
}

// ensureTrailWorktreeIgnoreRule appends the .entire/worktrees/ rule to an
// existing repo-root .gitignore when the directory isn't already ignored.
// Already ignored, or no .gitignore at all → silent no-op: the CLI doesn't
// impose ignore policy on a repo that hasn't opted into one, and committing
// the appended rule stays the user's choice.
func ensureTrailWorktreeIgnoreRule(ctx context.Context, w io.Writer, root string) error {
	check := exec.CommandContext(ctx, "git", "check-ignore", "-q", trailWorktreesRelDir+"/")
	check.Dir = root
	err := check.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("failed to check ignore status of %s: %w", trailWorktreesRelDir, err)
	}

	appended, err := appendIgnoreRule(filepath.Join(root, ".gitignore"))
	if err != nil {
		return err
	}
	if appended {
		fmt.Fprintln(w, "Added .entire/worktrees/ to .gitignore — commit it to keep the rule.")
	}
	return nil
}

func appendIgnoreRule(path string) (bool, error) {
	const rule = trailWorktreesRelDir + "/"
	content, err := os.ReadFile(path) //nolint:gosec // path derived from repo root / git common dir
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read %s: %w", path, err)
	}
	for line := range strings.SplitSeq(string(content), "\n") {
		if strings.TrimSpace(line) == rule {
			return false, nil
		}
	}
	prefix := ""
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		prefix = "\n"
	}
	updated := string(content) + prefix + rule + "\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { //nolint:gosec // path derived from repo root / git common dir
		return false, fmt.Errorf("failed to update %s: %w", path, err)
	}
	return true, nil
}

const worktreeIncludeFile = ".worktreeinclude"

// copyWorktreeIncludeFiles copies ignored files matching .worktreeinclude
// patterns from the main worktree root into a freshly created worktree.
// Per-file failures warn and skip; they never fail the checkout.
func copyWorktreeIncludeFiles(ctx context.Context, errW io.Writer, root, dest string) error {
	patterns, err := loadWorktreeIncludePatterns(root)
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		return nil
	}
	ignored, err := listIgnoredFiles(ctx, root)
	if err != nil {
		return err
	}
	matches := matchIncludePatterns(patterns, ignored)
	if len(matches) == 0 {
		return nil
	}
	// dest is a fresh checkout of the trail branch, whose content the invoking
	// user did not author. os.Root confines writes to dest even if the branch
	// contains a tracked symlinked directory pointing outside it.
	destRoot, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("failed to open worktree root: %w", err)
	}
	defer destRoot.Close()
	for _, rel := range matches {
		if err := copyIncludedFile(filepath.Join(root, rel), destRoot, rel); err != nil {
			fmt.Fprintf(errW, "warning: skipped %s: %v\n", filepath.ToSlash(rel), err)
		}
	}
	return nil
}

// loadWorktreeIncludePatterns reads .worktreeinclude from root. A missing
// file means nothing gets copied. Lines are gitignore-style patterns; blank
// lines and #-comments are skipped.
func loadWorktreeIncludePatterns(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, worktreeIncludeFile)) //nolint:gosec // path derived from repo root
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", worktreeIncludeFile, err)
	}
	var patterns []string
	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}

// listIgnoredFiles returns untracked files ignored by repo ignore rules,
// relative to root. Paths under .entire/worktrees are excluded: sibling trail
// worktrees' own ignored files (e.g. their .env) appear in the listing at the
// main root and would otherwise be copied into every new worktree.
func listIgnoredFiles(ctx context.Context, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list ignored files: %w", err)
	}
	var files []string
	for f := range bytes.SplitSeq(output, []byte{0}) {
		if len(f) > 0 && !isManagedTrailWorktreePath(string(f)) {
			files = append(files, string(f))
		}
	}
	return files, nil
}

func matchIncludePatterns(patterns, files []string) []string {
	ps := make([]gitignore.Pattern, 0, len(patterns))
	for _, pattern := range patterns {
		ps = append(ps, gitignore.ParsePattern(pattern, nil))
	}
	matcher := gitignore.NewMatcher(ps)
	included := make([]string, 0, len(files))
	for _, file := range files {
		rel, ok := cleanRelativeIncludeFile(file)
		if !ok || !matcher.Match(strings.Split(filepath.ToSlash(rel), "/"), false) {
			continue
		}
		included = append(included, rel)
	}
	return included
}

func isManagedTrailWorktreePath(rel string) bool {
	slash := filepath.ToSlash(rel)
	return slash == trailWorktreesRelDir || strings.HasPrefix(slash, trailWorktreesRelDir+"/")
}

func cleanRelativeIncludeFile(rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || paths.IsRelativeTraversal(clean) {
		return "", false
	}
	return clean, true
}

// copyIncludedFile copies src into destRoot at rel. destRoot confines all
// writes to the worktree root, so a tracked symlinked directory in the
// branch cannot redirect the copy outside the worktree.
func copyIncludedFile(src string, destRoot *os.Root, rel string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err //nolint:wrapcheck // lstat error is sufficient for caller context
	}
	if !srcInfo.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	in, err := os.Open(src) //nolint:gosec // src derived from repo root + .worktreeinclude
	if err != nil {
		return err //nolint:wrapcheck // open error is sufficient for caller context
	}
	defer in.Close()
	openedInfo, err := in.Stat()
	if err != nil {
		return err //nolint:wrapcheck // stat error is sufficient for caller context
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(srcInfo, openedInfo) {
		return errors.New("source changed while opening")
	}
	if err := osroot.MkdirAll(destRoot, filepath.Dir(rel), 0o750); err != nil {
		return err //nolint:wrapcheck // mkdir error is sufficient for caller context
	}
	out, err := destRoot.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcInfo.Mode().Perm())
	if err != nil {
		return err //nolint:wrapcheck // openfile error is sufficient for caller context
	}
	copied := false
	defer func() {
		if !copied {
			_ = out.Close()
			_ = osroot.Remove(destRoot, rel) //nolint:errcheck // best-effort cleanup after a failed copy
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err //nolint:wrapcheck // copy error is sufficient for caller context
	}
	if err := out.Close(); err != nil {
		return err //nolint:wrapcheck // close error is sufficient for caller context
	}
	if err := destRoot.Chmod(rel, srcInfo.Mode().Perm()); err != nil {
		return err //nolint:wrapcheck // chmod error is sufficient for caller context
	}
	copied = true
	return nil
}

// checkoutTrailWorktree checks branch out into a managed worktree under
// <main-root>/.entire/worktrees instead of switching the current checkout.
// The final output line is a shell-safe `cd '<path>'` hint.
func checkoutTrailWorktree(ctx context.Context, w, errW io.Writer, branch string, force bool, trailNumber int) error {
	// The trail number disambiguates the worktree directory: sanitized branch
	// names are lossy (feature/x and feature-x collide), so an unnumbered
	// trail cannot get a unique location.
	if trailNumber <= 0 {
		return fmt.Errorf("trail for branch %q has no number yet; cannot check out into a worktree", branch)
	}
	_, err := checkoutManagedBranchWorktree(ctx, w, errW, branch, force, false, func(root string) string {
		return defaultTrailWorktreePath(root, branch, trailNumber)
	})
	return err
}

// checkoutReviewWorktree returns a worktree containing branch. Unlike trail
// checkout, review can use a branch with no trail and can run in an existing
// worktree outside Entire's managed directory.
func checkoutReviewWorktree(ctx context.Context, w, errW io.Writer, branch string) (string, error) {
	return checkoutManagedBranchWorktree(ctx, w, errW, branch, false, true, func(root string) string {
		return defaultReviewWorktreePath(root, branch)
	})
}

func checkoutManagedBranchWorktree(
	ctx context.Context,
	w, errW io.Writer,
	branch string,
	force, reuseExternal bool,
	worktreePathForRoot func(string) string,
) (string, error) {
	if err := ValidateBranchName(ctx, branch); err != nil {
		return "", err
	}
	root, err := trailWorktreeBaseRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to find main worktree root: %w", err)
	}

	match, found, err := findWorktreeForBranch(ctx, branch, root)
	if err != nil {
		return "", err
	}
	if found {
		switch info, statErr := os.Lstat(match.path); {
		case statErr == nil && !info.IsDir():
			return "", fmt.Errorf("branch %q is registered to %s, which is not a directory", branch, match.path)
		case statErr == nil:
			if !match.managed && !reuseExternal {
				return "", fmt.Errorf("branch %q is already checked out at %s", branch, match.path)
			}
			if err := validateTrailWorktreeReuse(ctx, match.path, branch); err != nil {
				return "", staleTrailWorktreeError(branch, match.path)
			}
			printTrailWorktreeLocation(w, errW, "Worktree already exists at "+match.path, match.path)
			return match.path, nil
		case errors.Is(statErr, fs.ErrNotExist):
			return "", staleTrailWorktreeError(branch, match.path)
		default:
			return "", fmt.Errorf("failed to check worktree at %s: %w", match.path, statErr)
		}
	}

	proceed, err := ensureTrailWorktreeBranchAvailable(ctx, errW, branch, force)
	if err != nil {
		return "", err
	}
	if !proceed {
		fmt.Fprintf(errW, "Checkout of branch %s cancelled.\n", branch)
		return "", nil
	}
	if err := ensureTrailWorktreeIgnoreRule(ctx, errW, root); err != nil {
		return "", err
	}

	worktreePath := worktreePathForRoot(root)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o750); err != nil {
		return "", fmt.Errorf("failed to create worktree parent: %w", err)
	}
	add := exec.CommandContext(ctx, "git", "worktree", "add", worktreePath, branch)
	add.Dir = root
	if output, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("failed to create worktree: %s: %w", strings.TrimSpace(string(output)), err)
	}
	if err := copyWorktreeIncludeFiles(ctx, errW, root, worktreePath); err != nil {
		fmt.Fprintf(errW, "warning: could not copy %s files: %v\n", worktreeIncludeFile, err)
	}
	printTrailWorktreeLocation(w, errW, "Worktree ready at "+worktreePath, worktreePath)
	return worktreePath, nil
}

// printTrailWorktreeLocation reports where the worktree lives. On a terminal
// it prints the note and a copy-paste cd hint. Otherwise stdout carries only
// the bare path — so `cd "$(entire trail checkout <n> --worktree)"` works —
// and the note goes to stderr.
func printTrailWorktreeLocation(w, errW io.Writer, note, path string) {
	if interactive.IsTerminalWriter(w) {
		fmt.Fprintln(w, note)
		fmt.Fprintf(w, "cd %s\n", shellQuote(path))
		return
	}
	fmt.Fprintln(errW, note)
	fmt.Fprintln(w, path)
}

func validateTrailWorktreeReuse(ctx context.Context, path, branch string) error {
	expectedCommonDir, err := gitCommonDirForTrailWorktree(ctx)
	if err != nil {
		return err
	}

	showTop := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	output, err := showTop.Output()
	if err != nil {
		return err //nolint:wrapcheck // caller reports a prune hint, not this low-level probe
	}
	if normalizeWorktreePath(strings.TrimSpace(string(output))) != normalizeWorktreePath(path) {
		return errors.New("path is not a worktree root")
	}

	showCommon := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--git-common-dir")
	output, err = showCommon.Output()
	if err != nil {
		return err //nolint:wrapcheck // caller reports a prune hint, not this low-level probe
	}
	commonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(path, commonDir)
	}
	if normalizeWorktreePath(commonDir) != normalizeWorktreePath(expectedCommonDir) {
		return errors.New("path belongs to another repository")
	}

	showBranch := exec.CommandContext(ctx, "git", "-C", path, "branch", "--show-current")
	output, err = showBranch.Output()
	if err != nil {
		return err //nolint:wrapcheck // caller reports a prune hint, not this low-level probe
	}
	if strings.TrimSpace(string(output)) != branch {
		return fmt.Errorf("worktree is not on branch %q", branch)
	}
	return nil
}

// gitOutputError formats a failed git invocation, including git's stderr
// (captured by cmd.Output in ExitError.Stderr) when it carries a diagnostic —
// the bare error is usually just "exit status 128".
func gitOutputError(action string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(bytes.TrimSpace(exitErr.Stderr)) > 0 {
		return fmt.Errorf("%s: %s: %w", action, bytes.TrimSpace(exitErr.Stderr), err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func staleTrailWorktreeError(branch, path string) error {
	return fmt.Errorf("branch %q is registered to a missing worktree at %s; run 'git worktree prune' to clear it", branch, path)
}

// trailWorktreeMatch describes an existing worktree that has a branch checked
// out; managed means it lives under <root>/.entire/worktrees.
type trailWorktreeMatch struct {
	path    string
	managed bool
}

// findWorktreeForBranch returns the worktree that has branch checked out,
// with found reporting whether any worktree does.
func findWorktreeForBranch(ctx context.Context, branch, root string) (match trailWorktreeMatch, found bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return trailWorktreeMatch{}, false, gitOutputError("failed to list worktrees", err)
	}
	// Empty currentRoot: match any worktree, including the current checkout.
	path, found := parseWorktreeForBranch(string(output), branch, "")
	if !found {
		return trailWorktreeMatch{}, false, nil
	}
	managedRoot := normalizeWorktreePath(filepath.Join(root, filepath.FromSlash(trailWorktreesRelDir)))
	normalized := normalizeWorktreePath(path)
	managed := normalized == managedRoot || strings.HasPrefix(normalized, managedRoot+string(filepath.Separator))
	return trailWorktreeMatch{path: path, managed: managed}, true, nil
}

// ensureTrailWorktreeBranchAvailable makes sure branch exists locally,
// fetching it from origin when it only exists there. It returns false when
// the user declined the fetch prompt. --force and non-interactive runs fetch
// without prompting.
func ensureTrailWorktreeBranchAvailable(ctx context.Context, w io.Writer, branch string, force bool) (bool, error) {
	exists, err := BranchExistsLocally(ctx, branch)
	if err != nil {
		return false, fmt.Errorf("failed to check branch: %w", err)
	}
	if exists {
		return true, nil
	}

	remoteExists, err := BranchExistsOnRemote(ctx, branch)
	if err != nil {
		return false, fmt.Errorf("failed to check remote branch: %w", err)
	}
	if !remoteExists {
		return false, fmt.Errorf("branch %q not found locally or on origin", branch)
	}
	if !force && interactive.CanPromptInteractively() {
		shouldFetch, err := promptFetchFromRemote(branch)
		if err != nil {
			return false, err
		}
		if !shouldFetch {
			return false, nil
		}
	}

	fmt.Fprintf(w, "Fetching branch '%s' from origin...\n", branch)
	if err := fetchTrailWorktreeBranch(ctx, branch); err != nil {
		return false, err
	}
	return true, nil
}

// fetchTrailWorktreeBranch fetches origin's branch directly into refs/heads
// so `git worktree add` can check it out without touching the current
// checkout.
func fetchTrailWorktreeBranch(ctx context.Context, branch string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	refSpec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)
	// NoFilter: the worktree checkout needs full branch content; a partial
	// clone would leave blobs missing.
	output, err := remote.Fetch(ctx, remote.FetchOptions{
		Remote:   "origin",
		RefSpecs: []string{refSpec},
		NoFilter: true,
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("fetch timed out after 2 minutes")
		}
		return fmt.Errorf("failed to fetch branch from origin: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
