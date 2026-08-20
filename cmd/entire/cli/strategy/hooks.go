package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Hook marker used to identify Entire CLI hooks
const entireHookMarker = "Entire CLI hooks"

const backupSuffix = ".pre-entire"
const chainComment = "# Chain: run pre-existing hook"
const missingEntireGitHookWarning = "[entire] Entire CLI is enabled but not installed or not on PATH. Skipping Entire Git hook; continuing. Installation guide: https://docs.entire.io/cli/installation#installation-methods"

// gitHookNames are the git hooks managed by Entire CLI
var gitHookNames = []string{"prepare-commit-msg", "commit-msg", "post-commit", "post-rewrite", "pre-push"}

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
func GetGitDir(ctx context.Context) (string, error) {
	return getGitDirInPath(ctx, ".")
}

// hooksDirCache caches the hooks directory to avoid repeated git subprocess spawns.
// Keyed by current working directory to handle directory changes.
var (
	hooksDirMu       sync.RWMutex
	hooksDirCache    string
	hooksDirCacheDir string
)

// GetHooksDir returns the active hooks directory path.
// This respects core.hooksPath and correctly resolves to the common hooks
// directory when called from a linked worktree.
// The result is cached per working directory.
func GetHooksDir(ctx context.Context) (string, error) {
	cwd, err := os.Getwd() //nolint:forbidigo // cache key for hooks dir, same pattern as paths.WorktreeRoot()
	if err != nil {
		cwd = ""
	}

	hooksDirMu.RLock()
	if hooksDirCache != "" && hooksDirCacheDir == cwd {
		cached := hooksDirCache
		hooksDirMu.RUnlock()
		return cached, nil
	}
	hooksDirMu.RUnlock()

	result, err := getHooksDirInPath(ctx, ".")
	if err != nil {
		return "", err
	}

	hooksDirMu.Lock()
	hooksDirCache = result
	hooksDirCacheDir = cwd
	hooksDirMu.Unlock()

	return result, nil
}

// ClearHooksDirCache clears the cached hooks directory.
// This is primarily useful for testing when changing directories.
func ClearHooksDirCache() {
	hooksDirMu.Lock()
	hooksDirCache = ""
	hooksDirCacheDir = ""
	hooksDirMu.Unlock()
}

// getGitDirInPath returns the git directory for a repository at the given path.
// It delegates to `git rev-parse --git-dir` to leverage git's own validation.
func getGitDirInPath(ctx context.Context, dir string) (string, error) {
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

// getHooksDirInPath returns the active hooks directory for a repository at the given path.
// It delegates to `git rev-parse --git-path hooks` so Git resolves:
// - linked-worktree common hooks directory
// - core.hooksPath (relative or absolute)
func getHooksDirInPath(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", errors.New("not a git repository")
	}

	hooksDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(hooksDir) {
		hooksDir = filepath.Join(dir, hooksDir)
	}

	return filepath.Clean(hooksDir), nil
}

// GitHookState describes .git/hooks relative to what InstallGitHook writes today.
//
// Deliberately the same vocabulary as agent.HookConfigState: there are two hook
// surfaces (git hooks and per-agent configs) and one set of words for "ours and
// current" vs "ours but stale" vs "not ours". A single bool cannot serve both
// questions callers ask — EnsureSetup needs "are these current", while uninstall
// needs "is there anything of ours to remove", and a stale hook answers those
// differently.
type GitHookState int

const (
	// GitHooksAbsent means there is no complete set of Entire git hooks: one is
	// missing, or a foreign hook sits at one of our paths. A partial set reads
	// Absent, which is what the predicate has always reported.
	GitHooksAbsent GitHookState = iota
	// GitHooksCurrent means every managed hook is ours and in the shape this
	// version writes.
	GitHooksCurrent
	// GitHooksOutdated means the hooks are ours but at least one is a shape we no
	// longer write. Today that means running Entire from the working tree, which
	// is broken as well as stale — the path it names is gone.
	GitHooksOutdated
)

// CheckGitHookState reports the state of the active hooks directory.
func CheckGitHookState(ctx context.Context) GitHookState {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return GitHooksAbsent
	}
	return gitHookStateInHooksDir(hooksDir)
}

// CheckGitHookStateInDir reports the state for a specific repo directory, for
// callers that must not depend on the working directory.
func CheckGitHookStateInDir(ctx context.Context, repoDir string) GitHookState {
	hooksDir, err := getHooksDirInPath(ctx, repoDir)
	if err != nil {
		return GitHooksAbsent
	}
	return gitHookStateInHooksDir(hooksDir)
}

// IsGitHookInstalled reports whether a CURRENT set of Entire git hooks is
// installed. Callers that need to tell "none" from "ours but stale" — uninstall,
// doctor — must use CheckGitHookState instead, or they will treat a stale hook as
// if it were not there.
func IsGitHookInstalled(ctx context.Context) bool {
	return CheckGitHookState(ctx) == GitHooksCurrent
}

// IsGitHookInstalledInDir checks if a current set of Entire CLI hooks is installed
// in the given repo directory. Useful for tests that must not change cwd.
func IsGitHookInstalledInDir(ctx context.Context, repoDir string) bool {
	return CheckGitHookStateInDir(ctx, repoDir) == GitHooksCurrent
}

// AnyGitHookInstalled reports whether Entire git hooks are present at all,
// current or not. This is what uninstall needs: a stale hook is still ours, and
// still ours to remove.
func AnyGitHookInstalled(ctx context.Context) bool {
	return CheckGitHookState(ctx) != GitHooksAbsent
}

// ReinstallGitHooks rewrites the managed git hooks using this repo's hook
// settings — the same pair EnsureSetup runs, exported so callers outside this
// package cannot open-code it and silently drop absolute_git_hook_path.
func ReinstallGitHooks(ctx context.Context) (int, error) {
	return InstallGitHook(ctx, true, hookSettingsFromConfig(ctx))
}

// legacyGitHookLaunchers are the markers of a git hook that runs Entire from the
// working tree instead of the installed binary — the two shapes local-dev mode
// wrote over time. Retained for DETECTION ONLY: a hook naming either must be
// treated as needing reinstallation.
//
// Such a hook is actively broken, not merely outdated. It carries
// entireHookMarker, so a marker-only check reports it as installed and nothing
// ever replaces it. `scripts/entire-dev` no longer exists, and `go run` on a
// single file cannot build a package split across several — so both fail, and a
// repo-relative prefix gets no availability guard to swallow it. pre-push
// deliberately propagates exit codes, so the older of these forms was one
// missing `|| true` away from rejecting every `git push`.
//
// Matching these two rather than "anything unexpected" is deliberate: neither
// can appear in a hook this version generates (which emits only bare `entire` or
// a quoted absolute path), and they are the same forms the agents match in their
// entireHookPrefixes.
var legacyGitHookLaunchers = []string{"scripts/entire-dev", "go run "}

// bareEntireHookCmd is the default hook command prefix: the entire binary
// resolved through PATH at hook runtime.
const bareEntireHookCmd = "entire"

// gitHookStateInHooksDir classifies the hooks in the given directory. A hook that
// is present but still invokes a removed local-dev launcher reads Outdated, not
// Current, which is what makes EnsureSetup reinstall it rather than leaving a
// broken hook in place forever.
func gitHookStateInHooksDir(hooksDir string) GitHookState {
	outdated := false
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hook)
		data, err := os.ReadFile(hookPath) //nolint:gosec // Path is constructed from constants
		if err != nil {
			return GitHooksAbsent
		}
		content := string(data)
		if !strings.Contains(content, entireHookMarker) {
			return GitHooksAbsent
		}
		if entireHookLineRunsFromWorkingTree(content) {
			outdated = true
		}
	}
	if outdated {
		return GitHooksOutdated
	}
	return GitHooksCurrent
}

// entireHookLineRunsFromWorkingTree reports whether the hook's OWN Entire
// invocation names a legacy launcher.
//
// Scoped to the invocation line, not the whole file. A user may hand-edit a hook
// Entire installed to append their own steps, and one of those steps containing
// `go run ` must not make the file read as ours-but-stale: InstallGitHook only
// backs up a hook that does NOT carry entireHookMarker, so a hand-edited hook is
// rewritten with no backup and a false positive here would silently discard their
// additions. Every generated invocation contains `hooks git `, so keying on that
// line separates Entire's command from anything around it.
func entireHookLineRunsFromWorkingTree(content string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		if !strings.Contains(line, "hooks git ") {
			continue
		}
		for _, launcher := range legacyGitHookLaunchers {
			if strings.Contains(line, launcher) {
				return true
			}
		}
	}
	return false
}

// buildHookSpecs returns the hook specifications for all managed hooks.
func buildHookSpecs(cmdPrefix string) []hookSpec {
	prepareCommitMsgCmd := gitHookCommand(cmdPrefix, `prepare-commit-msg "$1" "$2" 2>/dev/null || true`, false)
	commitMsgCmd := gitHookCommand(cmdPrefix, `commit-msg "$1" || true`, true)
	postCommitCmd := gitHookCommand(cmdPrefix, `post-commit 2>/dev/null || true`, false)
	postRewriteCmd := gitHookCommand(cmdPrefix, `post-rewrite "$1" 2>/dev/null || true`, false)
	// pre-push intentionally does NOT swallow exit codes — the OPF
	// rewrite returns errors when it detects a privacy-critical
	// condition (diverged remote, oversized bootstrap, CAS conflict,
	// OPF runtime failure) and the user's git push must abort.
	// Transient checkpoint-push failures (e.g. the
	// entire/checkpoints/v1 push itself failing) are NOT returned
	// from PrePush — they're logged and swallowed at the CLI level
	// so they never reach this point as non-zero exits.
	//
	// Trade-off: an unrelated `entire` crash (segfault, panic in
	// non-OPF code) ALSO aborts the user's push. This is the safer
	// failure mode — we cannot distinguish from the shell's point of
	// view whether a non-zero exit means "OPF declined to redact" or
	// "entire crashed mid-rewrite", and silently letting potentially-
	// unredacted content reach the remote would violate the contract
	// the user opted into by enabling OPF. Users hit by unrelated
	// bugs can `ENTIRE_OPF=no git push` for a one-off bypass while
	// the bug is fixed.
	prePushCmd := gitHookCommand(cmdPrefix, `pre-push "$1"`, false)

	return []hookSpec{
		{
			name: "prepare-commit-msg",
			content: fmt.Sprintf(`#!/bin/sh
# %s
%s
`, entireHookMarker, prepareCommitMsgCmd),
		},
		{
			name: "commit-msg",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Commit-msg hook: strip trailer if no user content (allows aborting empty commits)
%s
`, entireHookMarker, commitMsgCmd),
		},
		{
			name: "post-commit",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Post-commit hook: condense session data if commit has Entire-Checkpoint trailer
%s
`, entireHookMarker, postCommitCmd),
		},
		{
			name: "post-rewrite",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Post-rewrite hook: remap session linkage after amend/rebase rewrites
%s
`, entireHookMarker, postRewriteCmd),
		},
		{
			name: "pre-push",
			content: fmt.Sprintf(`#!/bin/sh
# %s
# Pre-push hook: push session logs alongside user's push
# $1 is the remote name (e.g., "origin")
%s
`, entireHookMarker, prePushCmd),
		},
	}
}

// gitHookCommand wraps an invocation in an existence test, so a hook whose
// binary is missing exits cleanly instead of failing the surrounding git
// operation. Every prefix is guarded — there is no unguarded form.
func gitHookCommand(cmdPrefix, args string, warnMissing bool) string {
	invocation := fmt.Sprintf("%s hooks git %s", cmdPrefix, args)
	missingAction := ":"
	if warnMissing {
		missingAction = fmt.Sprintf("printf '%%s\\n' %s >&2 || :", shellQuote(missingEntireGitHookWarning))
	}
	return fmt.Sprintf("if %s; then %s; else %s; fi", gitHookCommandAvailableTest(cmdPrefix), invocation, missingAction)
}

// gitHookCommandAvailableTest returns the shell test that decides whether the
// hook's command can run.
//
// It always returns a test. It used to return ok=false for prefixes it could not
// classify, and gitHookCommand then emitted the invocation unguarded — a path
// that existed only for the removed local-dev launcher (a repo-relative path).
// The prefixes hookCmdPrefix can now produce are bare "entire" or a shell-quoted
// absolute path from os.Executable(), but an odd absolute form (a Windows UNC or
// extended-length path on a network share) would previously have fallen through
// and silently lost the guard, which is the one case where losing it hurts most.
// Defaulting to an executability test keeps the contract unconditional.
func gitHookCommandAvailableTest(cmdPrefix string) string {
	if cmdPrefix == bareEntireHookCmd {
		return "command -v entire >/dev/null 2>&1"
	}
	if isWindowsAbsoluteHookCommand(cmdPrefix) {
		// Git for Windows' sh has no reliable -x for native paths; -f is what
		// distinguishes "binary is there" from "it was uninstalled".
		return fmt.Sprintf("[ -f %s ]", cmdPrefix)
	}
	return fmt.Sprintf("[ -x %s ]", cmdPrefix)
}

func isWindowsAbsoluteHookCommand(cmdPrefix string) bool {
	path := strings.TrimPrefix(cmdPrefix, "'")
	if len(path) < len("C:\\") || path[1] != ':' {
		return false
	}
	driveLetter := path[0]
	if (driveLetter < 'A' || driveLetter > 'Z') && (driveLetter < 'a' || driveLetter > 'z') {
		return false
	}
	return path[2] == '\\' || path[2] == '/'
}

// InstallGitHook installs generic git hooks that delegate to `entire hook` commands.
// These hooks work with any strategy - the strategy is determined at runtime.
// If silent is true, no output is printed (except backup notifications, which always
// print — every user-initiated install passes silent=true and prints its own summary,
// so the backup notice is the only channel telling the user a pre-existing hook was
// moved aside). The lazy global setup uses installGitHooks with a discarded notice
// writer instead, so agent stderr stays clean.
// absolutePath embeds the full binary path in hooks for GUI git clients.
// Returns the number of hooks that were installed (0 if all already up to date).
//
// Hooks always invoke the "entire" binary. Git hooks written by older versions
// in local-dev mode ran a repo-relative launcher script instead; they still
// carry entireHookMarker, so the content comparison below rewrites them to the
// binary form on the next install rather than leaving them pointed at repo
// content.
func InstallGitHook(ctx context.Context, silent, absolutePath bool) (int, error) {
	return installGitHooks(ctx, silent, absolutePath, os.Stderr)
}

// installGitHooks is InstallGitHook with an explicit destination for the
// backup notices ("[entire] Backed up existing ..."). Pass io.Discard to
// suppress them (lazy invisible setup); InstallGitHook passes os.Stderr.
func installGitHooks(ctx context.Context, silent, absolutePath bool, backupNoticeW io.Writer) (int, error) {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return 0, err
	}

	info, statErr := os.Stat(hooksDir)
	notDir := statErr == nil && !info.IsDir()
	// ENOTDIR: a path component of hooksDir is itself a non-directory
	// (e.g. core.hooksPath=/dev/null/hooks) — same misconfiguration.
	if notDir || errors.Is(statErr, syscall.ENOTDIR) {
		return 0, fmt.Errorf("git resolves the hooks directory to %s, which is not a directory — core.hooksPath is likely set to disable git hooks\n"+
			"Entire requires git hooks to capture sessions. See where it is set with:\n"+
			"  git config --show-origin --get-all core.hooksPath\n"+
			"then unset it (git config --global --unset core.hooksPath) or override for this repo (git config core.hooksPath .git/hooks) and re-run 'entire enable'", hooksDir)
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil { //nolint:gosec // Git hooks require executable permissions
		return 0, fmt.Errorf("failed to create hooks directory: %w", err)
	}

	// Serialize installs across processes: with global mode, first-ever
	// installation runs from hook processes that can fire concurrently, and
	// the unsynchronized read→rename→write below can otherwise rename a
	// half-installed wrapper over the user's hook backup — destroying the
	// user's hook and leaving a wrapper that chains to itself. The lock lives
	// in the git common dir, never the hooks dir, which can be
	// worktree-resident (core.hooksPath).
	unlock, err := lockHookInstall(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()

	cmdPrefix, err := hookCmdPrefix(absolutePath)
	if err != nil {
		return 0, err
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
				fmt.Fprintf(backupNoticeW, "[entire] Backed up existing %s to %s%s\n", spec.name, spec.name, backupSuffix)
			} else {
				fmt.Fprintf(backupNoticeW, "[entire] Warning: replacing %s (backup %s%s already exists from a previous install)\n", spec.name, spec.name, backupSuffix)
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
// The write is atomic (temp file in the hooks dir, chmod 0o755, then rename):
// git can fire a hook at any moment, and a half-written hook script must never
// be executable in place of the old one.
func writeHookFile(path, content string) (bool, error) {
	// Check if file already exists with same content
	existing, err := os.ReadFile(path) //nolint:gosec // path is controlled
	if err == nil && string(existing) == content {
		return false, nil // Already up to date
	}

	// Git hooks must be executable (0o755)
	if err := jsonutil.WriteFileAtomic(path, []byte(content), 0o755); err != nil {
		return false, fmt.Errorf("failed to write hook file %s: %w", path, err)
	}
	return true, nil
}

// lockHookInstall takes the cross-process lock serializing every hook
// install/remove for this clone (shared across worktrees via the common dir).
func lockHookInstall(ctx context.Context) (func(), error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir for hook-install lock: %w", err)
	}
	release, err := flock.Acquire(filepath.Join(commonDir, "entire-hook-install.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock hook installation: %w", err)
	}
	return release, nil
}

// RemoveGitHook removes all Entire CLI git hooks from the repository.
// If a .pre-entire backup exists, it is restored.
// Returns the number of hooks removed.
func RemoveGitHook(ctx context.Context) (int, error) {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return 0, err
	}

	// Same lock as installGitHooks: a disable racing a lazy hook-process
	// install performs the same rename dance over the same paths.
	unlock, err := lockHookInstall(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()

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
	if hookName == "post-rewrite" {
		return generatePostRewriteChainedContent(baseContent)
	}

	return baseContent + fmt.Sprintf(`%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/%s%s" ]; then
    "$_entire_hook_dir/%s%s" "$@"
fi
`, chainComment, hookName, backupSuffix, hookName, backupSuffix)
}

func generatePostRewriteChainedContent(baseContent string) string {
	const original = `hooks git post-rewrite "$1" 2>/dev/null || true`
	const replacement = `hooks git post-rewrite "$1" < "$_entire_stdin" 2>/dev/null || true`

	replayPrefix := `#!/bin/sh
_entire_stdin="$(mktemp "${TMPDIR:-/tmp}/entire-post-rewrite.XXXXXX")"
cat > "$_entire_stdin"
trap 'rm -f "$_entire_stdin"' EXIT
`

	body := strings.TrimPrefix(baseContent, "#!/bin/sh\n")
	body = strings.Replace(body, original, replacement, 1)

	return replayPrefix + body + fmt.Sprintf(`
%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/post-rewrite%s" ]; then
    "$_entire_hook_dir/post-rewrite%s" "$@" < "$_entire_stdin"
fi
`, chainComment, backupSuffix, backupSuffix)
}

// hookCmdPrefix returns the command prefix for hook scripts and warning messages.
// When absolutePath is true, resolves the full binary path via os.Executable()
// and returns an error if resolution fails. This is needed for GUI git clients
// (Xcode, Tower, etc.) that don't source shell profiles.
//
// The prefix always names a binary outside the repository — either bare
// "entire" resolved through PATH or an absolute path inlined at install time.
// Never derive it from repository content: a repo-relative prefix lets whatever
// the working tree contains run on every git operation.
func hookCmdPrefix(absolutePath bool) (string, error) {
	if absolutePath {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("--absolute-git-hook-path: failed to resolve binary path: %w", err)
		}
		resolved, err := resolveHookExePath(exe, filepath.EvalSymlinks, runtime.GOOS)
		if err != nil {
			return "", err
		}
		return shellQuote(resolved), nil
	}
	return bareEntireHookCmd, nil
}

// resolveHookExePath resolves exe through symlinks for embedding as an absolute
// path in a git hook. On Windows, filepath.EvalSymlinks can fail when a path
// component is an NTFS directory junction rather than a plain symlink — notably
// Scoop's `…\scoop\apps\<app>\current\` junction, which yields "The system
// cannot find the path specified" (issue #1424). The unresolved os.Executable()
// path is itself a valid, launchable absolute path (and on Scoop the stable
// `current\` junction path is actually preferable, since it survives version
// updates that repoint the junction), so on Windows we fall back to it rather
// than failing the hook install outright. Off Windows, an EvalSymlinks failure
// is unexpected and still surfaced as an error.
func resolveHookExePath(exe string, evalSymlinks func(string) (string, error), goos string) (string, error) {
	resolved, err := evalSymlinks(exe)
	if err != nil {
		if goos == "windows" {
			return exe, nil
		}
		return "", fmt.Errorf("--absolute-git-hook-path: failed to resolve symlinks for %s: %w", exe, err)
	}
	return resolved, nil
}

// shellQuote wraps a string in single quotes for safe use in #!/bin/sh scripts.
// Handles paths containing spaces, apostrophes, or other shell metacharacters
// (e.g., /Users/John O'Brien/bin/entire).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// hookSettingsFromConfig loads hook-related settings from .entire/settings.json.
// Returns absoluteHookPath. On error, it defaults to false.
func hookSettingsFromConfig(ctx context.Context) (absoluteHookPath bool) {
	s, err := settings.Load(ctx)
	if err != nil {
		return false
	}
	return s.AbsoluteGitHookPath
}
