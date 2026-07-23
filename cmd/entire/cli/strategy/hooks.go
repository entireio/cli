package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Hook marker used to identify Entire CLI hooks
const entireHookMarker = "Entire CLI hooks"

const backupSuffix = ".pre-entire"
const chainComment = "# Chain: run pre-existing hook"

// preEntireExcludePattern keeps hook backups out of accidental `git add -A`
// commits (critical for husky-safe installs into tracked user-hook dirs).
const preEntireExcludePattern = "**/*" + backupSuffix
const missingEntireGitHookWarning = "[entire] Entire CLI is enabled but not installed or not on PATH. Skipping Entire Git hook; continuing. Installation guide: https://docs.entire.io/cli/installation#installation-methods"

// localDevHookCmdPrefix is the command prefix used for git hooks in local
// development mode. It points at scripts/entire-dev, which compiles the CLI on
// demand and falls back to the entire binary on PATH when the tree does not
// build. The path is relative to the repository root, which is git's working
// directory when it runs hooks.
const localDevHookCmdPrefix = "./scripts/entire-dev"

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

// huskyForwardingStub matches the husky v9 stub written into core.hooksPath
// (`_/prepare-commit-msg`, etc.), modulo a trailing newline we keep for
// readability. Real husky omits that newline; both forms source `_/h`, which
// runs the sibling user hook in the parent directory.
const huskyForwardingStub = "#!/usr/bin/env sh\n. \"$(dirname \"$0\")/h\"\n"

// huskyStubDispatchMarker is the sourcing line husky stubs use to reach `_/h`.
// Detection requires this (not merely "exists and lacks Entire marker").
const huskyStubDispatchMarker = `. "$(dirname "$0")/h"`

// huskyUserHooksDir returns the user-owned Husky hooks directory when hooksDir
// is a regenerable husky `_` directory (core.hooksPath) AND husky's dispatcher
// script (`_/h`) is present. Otherwise "".
//
// Husky v9 sets core.hooksPath to `<dir>/_` (usually `.husky/_`) and regenerates
// that directory on `husky` / npm prepare. User hook scripts live in the parent
// and are invoked by the stubs in `_` via `h` — those user scripts survive
// husky reinstall (see https://github.com/entireio/cli/issues/784).
//
// Hooks installed in the parent run through husky's dispatcher: HUSKY=0 skips
// them, ~/.config/husky/init.sh is sourced first, node_modules/.bin is
// prepended to PATH, and execution is under `sh -e`.
//
// The dispatcher check is required: git only executes files under
// core.hooksPath. Redirecting to the parent without husky's forwarding layout
// would install hooks that never run.
func huskyUserHooksDir(hooksDir string) string {
	if filepath.Base(hooksDir) != "_" {
		return ""
	}
	if !fileExists(filepath.Join(hooksDir, "h")) {
		return ""
	}
	return filepath.Dir(hooksDir)
}

// hookInstallDir returns the directory where Entire should write managed git
// hooks. For Husky (`_/h` present), that is the parent user-hook dir;
// otherwise it is the git hooks directory itself.
func hookInstallDir(hooksDir string) string {
	if userDir := huskyUserHooksDir(hooksDir); userDir != "" {
		return userDir
	}
	return hooksDir
}

// IsGitHookInstalled checks if all generic Entire CLI hooks are installed.
func IsGitHookInstalled(ctx context.Context) bool {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return false
	}
	return isGitHookInstalledForHooksDir(hooksDir)
}

// IsGitHookInstalledInDir checks if all Entire CLI hooks are installed in the given repo directory.
// This is useful for tests that need to check hooks without changing the working directory.
func IsGitHookInstalledInDir(ctx context.Context, repoDir string) bool {
	hooksDir, err := getHooksDirInPath(ctx, repoDir)
	if err != nil {
		return false
	}
	return isGitHookInstalledForHooksDir(hooksDir)
}

// isGitHookInstalledForHooksDir reports whether Entire's managed hooks are
// installed in the effective install directory, and (for husky-safe installs)
// that each regenerable `_` stub still sources `_/h` (husky's forwarder). A
// non-Entire file that does not source `h` is treated as not installed so
// EnsureSetup can heal.
func isGitHookInstalledForHooksDir(hooksDir string) bool {
	installDir := hookInstallDir(hooksDir)
	if !isGitHookInstalledInHooksDir(installDir) {
		return false
	}
	if installDir != hooksDir {
		return huskyForwardingStubsPresent(hooksDir)
	}
	return true
}

// huskyForwardingStubsPresent reports whether each managed hook under the
// husky-owned directory is a non-Entire stub that sources `_/h`.
func huskyForwardingStubsPresent(hooksDir string) bool {
	for _, hook := range gitHookNames {
		data, err := os.ReadFile(filepath.Join(hooksDir, hook)) //nolint:gosec // path from constants
		if err != nil {
			return false
		}
		content := string(data)
		if strings.Contains(content, entireHookMarker) {
			return false
		}
		if !strings.Contains(content, huskyStubDispatchMarker) {
			return false
		}
	}
	return true
}

// isGitHookInstalledInHooksDir checks if all hooks are installed in the given hooks directory.
func isGitHookInstalledInHooksDir(hooksDir string) bool {
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hook)
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

func gitHookCommand(cmdPrefix, args string, warnMissing bool) string {
	invocation := fmt.Sprintf("%s hooks git %s", cmdPrefix, args)
	availableTest, ok := gitHookCommandAvailableTest(cmdPrefix)
	if !ok {
		return invocation
	}

	missingAction := ":"
	if warnMissing {
		missingAction = fmt.Sprintf("printf '%%s\\n' %s >&2 || :", shellQuote(missingEntireGitHookWarning))
	}
	return fmt.Sprintf("if %s; then %s; else %s; fi", availableTest, invocation, missingAction)
}

func gitHookCommandAvailableTest(cmdPrefix string) (string, bool) {
	if cmdPrefix == "entire" {
		return "command -v entire >/dev/null 2>&1", true
	}
	if isWindowsAbsoluteHookCommand(cmdPrefix) {
		return fmt.Sprintf("[ -f %s ]", cmdPrefix), true
	}
	if strings.HasPrefix(cmdPrefix, "/") || strings.HasPrefix(cmdPrefix, "'/") {
		return fmt.Sprintf("[ -x %s ]", cmdPrefix), true
	}
	return "", false
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
// If silent is true, no output is printed (except backup notifications, which always print).
// localDev controls whether hooks use "go run" (true) or the "entire" binary (false).
// absolutePath embeds the full binary path in hooks for GUI git clients.
// Returns the number of hooks that were installed (0 if all already up to date).
//
// When core.hooksPath is Husky's .husky/_ directory, hooks are written to the
// parent `.husky/` user-hook directory (which husky's stubs invoke) rather than
// into `_`, so `npm install` / `husky` prepare cannot clobber them mid-turn.
//
// For husky-safe installs the parent user-hook directory is often tracked
// (unlike regenerable `_`). Existing non-Entire scripts there are renamed to
// `<hook>.pre-entire` and chained after Entire's wrapper.
//
// The second return value is true when install targeted a husky user-hook
// directory (parent of core.hooksPath=`_/`). Callers that print husky advisories
// should use that flag so the message cannot disagree with where hooks landed.
func InstallGitHook(ctx context.Context, silent, localDev, absolutePath bool) (int, bool, error) {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return 0, false, err
	}
	installDir := hookInstallDir(hooksDir)
	huskySafe := installDir != hooksDir

	if err := os.MkdirAll(installDir, 0o755); err != nil { //nolint:gosec // Git hooks require executable permissions
		return 0, huskySafe, fmt.Errorf("failed to create hooks directory: %w", err)
	}

	cmdPrefix, err := hookCmdPrefix(localDev, absolutePath)
	if err != nil {
		return 0, huskySafe, err
	}
	specs := buildHookSpecs(cmdPrefix)
	installedCount := 0
	backedUpUserHook := false

	for _, spec := range specs {
		hookPath := filepath.Join(installDir, spec.name)
		backupPath := hookPath + backupSuffix
		backupExists := fileExists(backupPath)

		// Back up existing non-Entire hooks
		existing, existingErr := os.ReadFile(hookPath) //nolint:gosec // path is controlled
		if existingErr == nil && !strings.Contains(string(existing), entireHookMarker) {
			if !backupExists {
				if err := os.Rename(hookPath, backupPath); err != nil {
					return installedCount, huskySafe, fmt.Errorf("failed to back up %s: %w", spec.name, err)
				}
				fmt.Fprintf(os.Stderr, "[entire] Backed up existing %s to %s%s\n", spec.name, spec.name, backupSuffix)
				backedUpUserHook = true
			} else {
				fmt.Fprintf(os.Stderr, "[entire] Warning: replacing %s (backup %s%s already exists from a previous install)\n", spec.name, spec.name, backupSuffix)
				backedUpUserHook = true
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
			return installedCount, huskySafe, fmt.Errorf("failed to install %s hook: %w", spec.name, err)
		}
		if written {
			installedCount++
		}
	}

	// After parent install succeeds, migrate older Entire wrappers out of
	// husky-owned `_` and ensure forwarding stubs exist so git can reach the
	// parent user hooks. Doing this after install avoids a window with zero
	// Entire hooks.
	if huskySafe {
		if err := migrateEntireHooksFromHuskyOwnedDir(hooksDir); err != nil {
			return installedCount, huskySafe, fmt.Errorf("failed to migrate hooks out of husky-owned directory: %w", err)
		}
		// Keep .pre-entire backups out of accidental commits when we shadow
		// tracked user hooks under the husky parent directory. Heal exclude
		// whenever any backup is present (not only on this invocation).
		if backedUpUserHook || huskyUserDirHasPreEntireBackups(installDir) {
			if exclErr := ensurePreEntireExcluded(ctx); exclErr != nil {
				fmt.Fprintf(os.Stderr, "[entire] Warning: could not exclude %s backups from git: %v\n", backupSuffix, exclErr)
			}
		}
		if backedUpUserHook {
			fmt.Fprintf(os.Stderr, "[entire] Note: existing hooks in %s were backed up to *%s and chained; commit the Entire wrappers intentionally and keep backups local\n", filepath.Base(installDir), backupSuffix)
		}
	}

	if !silent {
		fmt.Println("✓ Installed git hooks (prepare-commit-msg, commit-msg, post-commit, post-rewrite, pre-push)")
		fmt.Println("  Hooks delegate to the current strategy at runtime")
		if huskySafe {
			relInstall := installDir
			if root, rootErr := paths.WorktreeRoot(ctx); rootErr == nil {
				if rel, relErr := filepath.Rel(root, installDir); relErr == nil {
					relInstall = rel
				}
			}
			fmt.Printf("  Installed into %s/ (survives husky/npm prepare)\n", filepath.ToSlash(relInstall))
		}
	}

	return installedCount, huskySafe, nil
}

// ensurePreEntireExcluded appends preEntireExcludePattern to info/exclude in the
// shared git common dir so husky-safe backups are not half-committed beside
// Entire wrappers. Uses `git rev-parse --git-path info/exclude` so linked
// worktrees write into the common exclude (not .git/worktrees/<name>/info/).
func ensurePreEntireExcluded(ctx context.Context) error {
	excludePath, err := gitInfoExcludePath(ctx)
	if err != nil {
		return err
	}
	infoDir := filepath.Dir(excludePath)
	if err := os.MkdirAll(infoDir, 0o755); err != nil { //nolint:gosec // matches git's info dir mode
		return fmt.Errorf("create git info dir: %w", err)
	}
	existing, err := os.ReadFile(excludePath) //nolint:gosec // path from git
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read git exclude: %w", err)
	}
	content := string(existing)
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == preEntireExcludePattern {
			return nil
		}
	}
	var b strings.Builder
	b.WriteString(content)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	if !strings.Contains(content, "# Entire CLI") {
		b.WriteString("# Entire CLI\n")
	}
	b.WriteString(preEntireExcludePattern)
	b.WriteByte('\n')
	if err := os.WriteFile(excludePath, []byte(b.String()), 0o644); err != nil { //nolint:gosec // git exclude is not executable
		return fmt.Errorf("write git exclude: %w", err)
	}
	return nil
}

// gitInfoExcludePath resolves the repo's info/exclude path via git so linked
// worktrees share the common-dir exclude file.
func gitInfoExcludePath(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-path", "info/exclude")
	cmd.Dir = "."
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git info/exclude path: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errors.New("resolve git info/exclude path: empty")
	}
	if !filepath.IsAbs(path) {
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			return "", fmt.Errorf("absolutize info/exclude path: %w", absErr)
		}
		path = abs
	}
	return filepath.Clean(path), nil
}

func huskyUserDirHasPreEntireBackups(installDir string) bool {
	for _, hook := range gitHookNames {
		if fileExists(filepath.Join(installDir, hook+backupSuffix)) {
			return true
		}
	}
	return false
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
//
// For Husky setups, removals target the user-hook directory. Legacy Entire
// wrappers in regenerable `_` are scrubbed (restore backup or replace with a
// stub) but missing stubs are not backfilled — disable must not recreate
// husky's regenerable layout.
func RemoveGitHook(ctx context.Context) (int, error) {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return 0, err
	}
	installDir := hookInstallDir(hooksDir)

	removed, err := removeEntireHooksFromDir(installDir)
	if err != nil {
		return removed, err
	}

	// When husky-safe install is active, also scrub/restore the regenerable `_`
	// directory. If the dispatcher was deleted (hookInstallDir falls back to
	// `_`), still scrub Entire wrappers from the parent user-hook directory so
	// disable does not leave orphan Entire scripts behind.
	if installDir != hooksDir {
		scrubbed, cleanErr := scrubEntireHooksFromHuskyOwnedDir(hooksDir)
		if cleanErr != nil {
			return removed, cleanErr
		}
		removed += scrubbed
	} else if filepath.Base(hooksDir) == "_" {
		parentRemoved, parentErr := removeEntireHooksFromDir(filepath.Dir(hooksDir))
		if parentErr != nil {
			return removed, parentErr
		}
		removed += parentRemoved
	}
	return removed, nil
}

// migrateEntireHooksFromHuskyOwnedDir removes legacy Entire wrappers from the
// husky-owned `_` directory, restoring `.pre-entire` backups when present, or
// writing a standard husky forwarding stub when no backup exists. Also fills
// in any missing managed stubs so git can reach parent user hooks.
func migrateEntireHooksFromHuskyOwnedDir(hooksDir string) error {
	_, err := rewriteHuskyOwnedHooks(hooksDir, true)
	return err
}

// scrubEntireHooksFromHuskyOwnedDir removes Entire wrappers from the husky-owned
// `_` directory without creating stubs for hooks that were already absent.
// Returns how many Entire wrappers were restored or replaced.
func scrubEntireHooksFromHuskyOwnedDir(hooksDir string) (int, error) {
	return rewriteHuskyOwnedHooks(hooksDir, false)
}

// rewriteHuskyOwnedHooks migrates (backfillMissing=true) or scrubs
// (backfillMissing=false) Entire wrappers under the husky-owned hooks directory.
// When backfillMissing is true, non-forwarding files in `_` are replaced with
// huskyForwardingStub so EnsureSetup can heal broken stubs (IsGitHookInstalled
// treats those as not installed).
func rewriteHuskyOwnedHooks(hooksDir string, backfillMissing bool) (int, error) {
	changed := 0
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hook)
		backupPath := hookPath + backupSuffix

		data, err := os.ReadFile(hookPath) //nolint:gosec // path is controlled
		hookIsOurs := err == nil && strings.Contains(string(data), entireHookMarker)

		switch {
		case hookIsOurs && fileExists(backupPath):
			if err := os.Remove(hookPath); err != nil {
				return changed, fmt.Errorf("remove legacy %s: %w", hookPath, err)
			}
			if err := os.Rename(backupPath, hookPath); err != nil {
				return changed, fmt.Errorf("restore %s%s: %w", hookPath, backupSuffix, err)
			}
			changed++
		case hookIsOurs:
			if err := writeHookFileForced(hookPath, huskyForwardingStub); err != nil {
				return changed, fmt.Errorf("replace legacy %s with husky stub: %w", hookPath, err)
			}
			changed++
		case errors.Is(err, os.ErrNotExist):
			if !backfillMissing {
				continue
			}
			if err := writeHookFileForced(hookPath, huskyForwardingStub); err != nil {
				return changed, fmt.Errorf("write missing husky stub %s: %w", hookPath, err)
			}
			changed++
		case err != nil:
			return changed, fmt.Errorf("read husky-owned hook %s: %w", hookPath, err)
		default:
			// Existing non-Entire file under husky-owned `_`.
			if !backfillMissing {
				continue
			}
			if strings.Contains(string(data), huskyStubDispatchMarker) {
				continue // already a valid forwarding stub
			}
			if err := writeHookFileForced(hookPath, huskyForwardingStub); err != nil {
				return changed, fmt.Errorf("replace non-forwarding husky stub %s: %w", hookPath, err)
			}
			changed++
		}
	}
	return changed, nil
}

// writeHookFileForced writes content to path unconditionally (used for husky stubs).
func writeHookFileForced(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // Git hooks require executable permissions
		return fmt.Errorf("failed to write hook file %s: %w", path, err)
	}
	return nil
}

// removeEntireHooksFromDir removes Entire-managed hooks from dir, restoring
// .pre-entire backups when present. Returns the number of Entire hooks removed.
func removeEntireHooksFromDir(dir string) (int, error) {
	removed := 0
	var removeErrors []string

	for _, hook := range gitHookNames {
		hookPath := filepath.Join(dir, hook)
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
//
// Executable backups are run directly so shebang interpreters (Python, Node,
// …) keep working. Non-executable backups (common for husky mode-0644 hooks)
// fall back to `sh -e`, matching husky's own runner.
//
// Entire's exit status is preserved: a successful backup must not mask a
// failing pre-push (OPF abort). A failing backup still fails the hook.
func generateChainedContent(baseContent, hookName string) string {
	if hookName == "post-rewrite" {
		return generatePostRewriteChainedContent(baseContent)
	}

	return baseContent + fmt.Sprintf(`
_entire_status=$?
%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/%s%s" ]; then
    "$_entire_hook_dir/%s%s" "$@" || exit $?
elif [ -f "$_entire_hook_dir/%s%s" ]; then
    sh -e "$_entire_hook_dir/%s%s" "$@" || exit $?
fi
exit $_entire_status
`, chainComment, hookName, backupSuffix, hookName, backupSuffix, hookName, backupSuffix, hookName, backupSuffix)
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
_entire_status=$?
%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/post-rewrite%s" ]; then
    "$_entire_hook_dir/post-rewrite%s" "$@" < "$_entire_stdin" || exit $?
elif [ -f "$_entire_hook_dir/post-rewrite%s" ]; then
    sh -e "$_entire_hook_dir/post-rewrite%s" "$@" < "$_entire_stdin" || exit $?
fi
exit $_entire_status
`, chainComment, backupSuffix, backupSuffix, backupSuffix, backupSuffix)
}

// hookCmdPrefix returns the command prefix for hook scripts and warning messages.
// Returns the scripts/entire-dev launcher when local_dev is enabled.
// When absolutePath is true, resolves the full binary path via os.Executable()
// and returns an error if resolution fails. This is needed for GUI git clients
// (Xcode, Tower, etc.) that don't source shell profiles.
func hookCmdPrefix(localDev, absolutePath bool) (string, error) {
	if localDev {
		return localDevHookCmdPrefix, nil
	}
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
	return "entire", nil
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
// Returns (localDev, absoluteHookPath). On error, both default to false.
func hookSettingsFromConfig(ctx context.Context) (localDev, absoluteHookPath bool) {
	s, err := settings.Load(ctx)
	if err != nil {
		return false, false
	}
	return s.LocalDev, s.AbsoluteGitHookPath
}
