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
	"syscall"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Hook marker used to identify Entire CLI hooks
const entireHookMarker = "Entire CLI hooks"

const backupSuffix = ".pre-entire"
const chainComment = "# Chain: run pre-existing hook"

// preEntireExcludePattern keeps hook backups out of accidental `git add -A`
// commits (legacy husky-safe installs that still use .pre-entire companions).
const preEntireExcludePattern = "**/*" + backupSuffix

// Delimited so RemoveGitHook can strip Entire's exclude entry without leaving a
// forever-broad pattern that hides unrelated *{backupSuffix} paths.
const (
	preEntireExcludeBegin = "# BEGIN Entire CLI pre-entire exclude"
	preEntireExcludeEnd   = "# END Entire CLI pre-entire exclude"
)

// Managed block markers for husky-safe installs: Entire is injected into the
// tracked user hook so clones keep the original team logic.
const (
	entireManagedBegin = "# BEGIN Entire CLI managed block"
	entireManagedEnd   = "# END Entire CLI managed block"
)

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

// huskyForwardingStubsPresent reports whether each managed hook under the
// husky-owned directory is a regular, executable, non-Entire stub that actively
// sources `_/h` (not merely a commented-out dispatch line).
func huskyForwardingStubsPresent(hooksDir string) bool {
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hook)
		info, err := os.Lstat(hookPath)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		if info.Mode()&0o111 == 0 {
			return false
		}
		data, err := os.ReadFile(hookPath) //nolint:gosec // path from constants
		if err != nil {
			return false
		}
		content := string(data)
		if strings.Contains(content, entireHookMarker) {
			return false
		}
		if !hasActiveHuskyStubDispatch(content) {
			return false
		}
	}
	return true
}

// hasActiveHuskyStubDispatch reports whether content contains an uncommented
// huskyStubDispatchMarker line.
func hasActiveHuskyStubDispatch(content string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, huskyStubDispatchMarker) {
			return true
		}
	}
	return false
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
	count, _, err := InstallGitHook(ctx, true, hookSettingsFromConfig(ctx))
	return count, err
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

// gitHookStateInHooksDir classifies the hooks git will actually run for the
// given core.hooksPath. For a husky-owned `_` directory (dispatcher present),
// Entire's hooks live in the parent user-hook directory instead — see
// hookInstallDir — so this checks that directory, plus that the regenerable
// `_` stubs still forward to it (huskyForwardingStubsPresent). A husky-owned
// directory whose stubs do not forward reads Absent even if the parent
// directory itself holds a current set of Entire hooks, because git would
// never reach them.
func gitHookStateInHooksDir(hooksDir string) GitHookState {
	installDir := hookInstallDir(hooksDir)
	state := gitHookStateInInstallDir(installDir)
	if state == GitHooksAbsent {
		return GitHooksAbsent
	}
	if installDir != hooksDir && !huskyForwardingStubsPresent(hooksDir) {
		return GitHooksAbsent
	}
	return state
}

// gitHookStateInInstallDir classifies the hooks in the given directory. A hook
// that is present but still invokes a removed local-dev launcher reads
// Outdated, not Current, which is what makes EnsureSetup reinstall it rather
// than leaving a broken hook in place forever.
func gitHookStateInInstallDir(hooksDir string) GitHookState {
	outdated := false
	for _, hook := range gitHookNames {
		hookPath := filepath.Join(hooksDir, hook)
		data, err := os.ReadFile(hookPath) //nolint:gosec // Path is constructed from constants
		if err != nil {
			return GitHooksAbsent
		}
		content := string(data)
		if !strings.Contains(content, entireHookMarker) && !strings.Contains(content, entireManagedBegin) {
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
// If silent is true, no output is printed (except backup notifications, which always print).
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
//
// Hooks always invoke the "entire" binary. Git hooks written by older versions
// in local-dev mode ran a repo-relative launcher script instead; they still
// carry entireHookMarker, so the content comparison below rewrites them to the
// binary form on the next install rather than leaving them pointed at repo
// content.
func InstallGitHook(ctx context.Context, silent, absolutePath bool) (int, bool, error) {
	hooksDir, err := GetHooksDir(ctx)
	if err != nil {
		return 0, false, err
	}
	installDir := hookInstallDir(hooksDir)
	huskySafe := installDir != hooksDir

	info, statErr := os.Stat(hooksDir)
	notDir := statErr == nil && !info.IsDir()
	// ENOTDIR: a path component of hooksDir is itself a non-directory
	// (e.g. core.hooksPath=/dev/null/hooks) — same misconfiguration.
	if notDir || errors.Is(statErr, syscall.ENOTDIR) {
		return 0, huskySafe, fmt.Errorf("git resolves the hooks directory to %s, which is not a directory — core.hooksPath is likely set to disable git hooks\n"+
			"Entire requires git hooks to capture sessions. See where it is set with:\n"+
			"  git config --show-origin --get-all core.hooksPath\n"+
			"then unset it (git config --global --unset core.hooksPath) or override for this repo (git config core.hooksPath .git/hooks) and re-run 'entire enable'", hooksDir)
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil { //nolint:gosec // Git hooks require executable permissions
		return 0, huskySafe, fmt.Errorf("failed to create hooks directory: %w", err)
	}

	cmdPrefix, err := hookCmdPrefix(absolutePath)
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

		existing, existingErr := os.ReadFile(hookPath) //nolint:gosec // path is controlled

		// Husky-safe: inject Entire into the tracked user hook so clones keep
		// the original team logic. Legacy `.pre-entire` companions are migrated
		// into the injected form when present.
		if huskySafe {
			content, touchedUser, err := huskySafeHookContent(spec.content, existing, existingErr, backupPath, backupExists)
			if err != nil {
				return installedCount, huskySafe, fmt.Errorf("failed to prepare %s hook: %w", spec.name, err)
			}
			if touchedUser {
				backedUpUserHook = true
			}
			written, err := writeHookFile(hookPath, content)
			if err != nil {
				return installedCount, huskySafe, fmt.Errorf("failed to install %s hook: %w", spec.name, err)
			}
			if written {
				installedCount++
			}
			if backupExists {
				_ = os.Remove(backupPath)
			}
			continue
		}

		// Non-Husky: back up existing non-Entire hooks and chain.
		if existingErr == nil && !strings.Contains(string(existing), entireHookMarker) {
			if !backupExists {
				if err := os.Rename(hookPath, backupPath); err != nil {
					return installedCount, huskySafe, fmt.Errorf("failed to back up %s: %w", spec.name, err)
				}
				fmt.Fprintf(os.Stderr, "[entire] Backed up existing %s to %s%s\n", spec.name, spec.name, backupSuffix)
				backedUpUserHook = true
			} else {
				// Stale backup must not win over a newer current hook.
				if err := preserveCurrentHookOverStaleBackup(hookPath, backupPath, existing); err != nil {
					return installedCount, huskySafe, fmt.Errorf("failed to rotate backup for %s: %w", spec.name, err)
				}
				fmt.Fprintf(os.Stderr, "[entire] Warning: replaced stale %s%s with current %s before reinstall\n", spec.name, backupSuffix, spec.name)
				backedUpUserHook = true
			}
			backupExists = true
		}

		content := spec.content
		if backupExists {
			// allowShFallback=false intentionally: plain .git/hooks only runs
			// executable files. A mode-0644 .pre-entire was never an active
			// hook; forcing it through `sh -e` (as husky does) can execute
			// Python/Node/disabled scripts and break commits/pushes. Husky
			// installs use injectEntireManagedBlock above, not this path.
			content = generateChainedContent(spec.content, spec.name, false)
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
		// Keep any leftover legacy .pre-entire backups out of accidental commits.
		if backedUpUserHook || huskyUserDirHasPreEntireBackups(installDir) {
			if exclErr := ensurePreEntireExcluded(ctx); exclErr != nil {
				fmt.Fprintf(os.Stderr, "[entire] Warning: could not exclude %s backups from git: %v\n", backupSuffix, exclErr)
			}
		}
		if backedUpUserHook {
			fmt.Fprintf(os.Stderr, "[entire] Note: injected Entire into existing hooks under %s/ (original logic stays in the tracked file)\n", filepath.Base(installDir))
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

// huskySafeHookContent builds the on-disk content for a husky user-hook path.
// It injects Entire into the existing (or legacy-backup) script so clones keep
// team logic. touchedUser is true when preexisting user content was preserved.
func huskySafeHookContent(entireContent string, existing []byte, existingErr error, backupPath string, backupExists bool) (content string, touchedUser bool, err error) {
	var userScript string
	switch {
	case existingErr == nil && strings.Contains(string(existing), entireManagedBegin):
		userScript = stripEntireManagedBlock(string(existing))
		touchedUser = strings.TrimSpace(stripShebang(userScript)) != ""
	case existingErr == nil && strings.Contains(string(existing), entireHookMarker):
		// Legacy Entire wrapper (possibly chained). Prefer the backup as the
		// original user script when present.
		if backupExists {
			backupData, readErr := os.ReadFile(backupPath) //nolint:gosec // path is controlled
			if readErr != nil {
				return "", false, fmt.Errorf("read legacy backup: %w", readErr)
			}
			userScript = string(backupData)
			touchedUser = true
		} else {
			return entireContent, false, nil
		}
	case existingErr == nil:
		userScript = string(existing)
		touchedUser = true
	case backupExists:
		backupData, readErr := os.ReadFile(backupPath) //nolint:gosec // path is controlled
		if readErr != nil {
			return "", false, fmt.Errorf("read legacy backup: %w", readErr)
		}
		userScript = string(backupData)
		touchedUser = true
	case errors.Is(existingErr, os.ErrNotExist):
		return entireContent, false, nil
	default:
		return "", false, existingErr
	}

	if strings.TrimSpace(stripShebang(userScript)) == "" {
		return entireContent, touchedUser, nil
	}
	return injectEntireManagedBlock(userScript, entireContent), touchedUser, nil
}

// preserveCurrentHookOverStaleBackup replaces backupPath with the current hook
// bytes when they differ, so reinstall does not discard newer user logic.
// hookPath is the live hook that the caller will overwrite next; it is left in
// place here so a crash or later write failure cannot erase the only copy.
func preserveCurrentHookOverStaleBackup(hookPath, backupPath string, current []byte) error {
	_ = hookPath
	prev, err := os.ReadFile(backupPath) //nolint:gosec // path is controlled
	if err != nil {
		return fmt.Errorf("read existing backup: %w", err)
	}
	if string(prev) == string(current) {
		return nil
	}
	stalePath := backupPath + ".stale"
	if err := os.Rename(backupPath, stalePath); err != nil {
		// If a prior stale exists, overwrite it.
		_ = os.Remove(stalePath)
		if err := os.Rename(backupPath, stalePath); err != nil {
			return fmt.Errorf("rotate stale backup: %w", err)
		}
	}
	if err := os.WriteFile(backupPath, current, 0o755); err != nil { //nolint:gosec // hook backup may be executed
		return fmt.Errorf("write updated backup: %w", err)
	}
	// Confirm the backup is readable and complete before the caller overwrites
	// hookPath. Do not delete hookPath here — that would open a window where a
	// crash or later write failure loses the only live copy of the user hook.
	got, err := os.ReadFile(backupPath) //nolint:gosec // path is controlled
	if err != nil {
		return fmt.Errorf("verify updated backup: %w", err)
	}
	if string(got) != string(current) {
		return fmt.Errorf("verify updated backup: wrote %d bytes, read back %d", len(current), len(got))
	}
	return nil
}

// ensurePreEntireExcluded appends a delimited preEntireExcludePattern block to
// info/exclude in the shared git common dir so leftover husky-safe backups are
// not half-committed. Uses `git rev-parse --git-path info/exclude` so linked
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
	if strings.Contains(content, preEntireExcludeBegin) && strings.Contains(content, preEntireExcludePattern) {
		return nil
	}
	// Drop a prior undelimited pattern so we don't leave a forever-broad rule.
	content = removeUndelimitedPreEntireExclude(content)

	var b strings.Builder
	b.WriteString(content)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(preEntireExcludeBegin)
	b.WriteByte('\n')
	b.WriteString(preEntireExcludePattern)
	b.WriteByte('\n')
	b.WriteString(preEntireExcludeEnd)
	b.WriteByte('\n')
	if err := os.WriteFile(excludePath, []byte(b.String()), 0o644); err != nil { //nolint:gosec // git exclude is not executable
		return fmt.Errorf("write git exclude: %w", err)
	}
	return nil
}

// removePreEntireExcluded strips Entire's managed exclude block (and any legacy
// undelimited pattern) when no managed backups remain.
func removePreEntireExcluded(ctx context.Context) error {
	excludePath, err := gitInfoExcludePath(ctx)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(excludePath) //nolint:gosec // path from git
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read git exclude: %w", err)
	}
	content := stripDelimitedBlock(string(existing), preEntireExcludeBegin, preEntireExcludeEnd)
	content = removeUndelimitedPreEntireExclude(content)
	if content == string(existing) {
		return nil
	}
	if strings.TrimSpace(content) == "" {
		_ = os.Remove(excludePath)
		return nil
	}
	if err := os.WriteFile(excludePath, []byte(content), 0o644); err != nil { //nolint:gosec // git exclude is not executable
		return fmt.Errorf("write git exclude: %w", err)
	}
	return nil
}

func removeUndelimitedPreEntireExclude(content string) string {
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == preEntireExcludePattern || trimmed == "# Entire CLI" {
			continue
		}
		lines = append(lines, line)
	}
	out := strings.Join(lines, "\n")
	if content != "" && strings.HasSuffix(content, "\n") && out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
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
// Returns true if the file was written or its mode was healed to executable.
func writeHookFile(path, content string) (bool, error) {
	existing, err := os.ReadFile(path) //nolint:gosec // path is controlled
	if err == nil && string(existing) == content {
		info, statErr := os.Stat(path)
		if statErr == nil && info.Mode()&0o111 != 0 {
			return false, nil // Already up to date
		}
		if chmodErr := os.Chmod(path, 0o755); chmodErr != nil { //nolint:gosec // Git hooks require executable permissions
			return false, fmt.Errorf("failed to heal executable bit on %s: %w", path, chmodErr)
		}
		return true, nil
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

	// Drop the managed exclude block once no backups remain in either dir.
	if !huskyUserDirHasPreEntireBackups(installDir) && !huskyUserDirHasPreEntireBackups(hooksDir) {
		if exclErr := removePreEntireExcluded(ctx); exclErr != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: could not remove %s exclude rule: %v\n", backupSuffix, exclErr)
		}
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
			if hasActiveHuskyStubDispatch(string(data)) {
				// Heal executable bit on an otherwise-valid stub.
				if _, err := writeHookFile(hookPath, string(data)); err != nil {
					return changed, fmt.Errorf("heal husky stub %s: %w", hookPath, err)
				}
				continue
			}
			// Preserve unknown content before healing.
			if !fileExists(backupPath) {
				if err := os.Rename(hookPath, backupPath); err != nil {
					return changed, fmt.Errorf("back up non-forwarding husky stub %s: %w", hookPath, err)
				}
				fmt.Fprintf(os.Stderr, "[entire] Backed up existing %s to %s%s\n", filepath.Base(hookPath), filepath.Base(hookPath), backupSuffix)
			} else {
				fmt.Fprintf(os.Stderr, "[entire] Warning: replacing %s (backup %s%s already exists)\n", filepath.Base(hookPath), filepath.Base(hookPath), backupSuffix)
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
// .pre-entire backups when present, or stripping injected managed blocks.
// Returns the number of Entire hooks removed.
func removeEntireHooksFromDir(dir string) (int, error) {
	removed := 0
	var removeErrors []string

	for _, hook := range gitHookNames {
		hookPath := filepath.Join(dir, hook)
		backupPath := hookPath + backupSuffix

		data, err := os.ReadFile(hookPath) //nolint:gosec // path is controlled
		hookExists := err == nil
		content := ""
		if hookExists {
			content = string(data)
		}
		hookIsOurs := hookExists && strings.Contains(content, entireHookMarker)
		hasManaged := hookExists && strings.Contains(content, entireManagedBegin)

		if hasManaged {
			restored := stripEntireManagedBlock(content)
			if strings.TrimSpace(stripShebang(restored)) == "" {
				if err := os.Remove(hookPath); err != nil {
					removeErrors = append(removeErrors, fmt.Sprintf("%s: %v", hook, err))
					continue
				}
			} else if err := os.WriteFile(hookPath, []byte(restored), 0o755); err != nil { //nolint:gosec // restore user hook
				removeErrors = append(removeErrors, fmt.Sprintf("strip managed block %s: %v", hook, err))
				continue
			}
			removed++
			// Managed-block installs should not leave a companion backup behind.
			_ = os.Remove(backupPath)
			continue
		}

		if hookIsOurs {
			if err := os.Remove(hookPath); err != nil {
				removeErrors = append(removeErrors, fmt.Sprintf("%s: %v", hook, err))
				continue
			}
			removed++
		}

		// Restore .pre-entire backup if it exists
		if fileExists(backupPath) {
			if hookExists && !hookIsOurs && !hasManaged {
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
// …) keep working. When allowShFallback is true (Husky-safe installs only),
// non-executable backups fall back to `sh -e`, matching husky's own runner.
// Outside Husky, non-executable backups stay inert — git ignored them before.
//
// Entire's exit status is preserved: a successful backup must not mask a
// failing pre-push (OPF abort). A failing backup still fails the hook.
func generateChainedContent(baseContent, hookName string, allowShFallback bool) string {
	if hookName == "post-rewrite" {
		return generatePostRewriteChainedContent(baseContent, allowShFallback)
	}

	chain := fmt.Sprintf(`
_entire_status=$?
%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/%s%s" ]; then
    "$_entire_hook_dir/%s%s" "$@" || exit $?
`, chainComment, hookName, backupSuffix, hookName, backupSuffix)
	if allowShFallback {
		chain += fmt.Sprintf(`elif [ -f "$_entire_hook_dir/%s%s" ]; then
    sh -e "$_entire_hook_dir/%s%s" "$@" || exit $?
`, hookName, backupSuffix, hookName, backupSuffix)
	}
	chain += "fi\nexit $_entire_status\n"
	return baseContent + chain
}

func generatePostRewriteChainedContent(baseContent string, allowShFallback bool) string {
	const original = `hooks git post-rewrite "$1" 2>/dev/null || true`
	const replacement = `hooks git post-rewrite "$1" < "$_entire_stdin" 2>/dev/null || true`

	replayPrefix := `#!/bin/sh
_entire_stdin="$(mktemp "${TMPDIR:-/tmp}/entire-post-rewrite.XXXXXX")"
cat > "$_entire_stdin"
trap 'rm -f "$_entire_stdin"' EXIT
`

	body := strings.TrimPrefix(baseContent, "#!/bin/sh\n")
	body = strings.Replace(body, original, replacement, 1)

	chain := fmt.Sprintf(`
_entire_status=$?
%s
_entire_hook_dir="$(dirname "$0")"
if [ -x "$_entire_hook_dir/post-rewrite%s" ]; then
    "$_entire_hook_dir/post-rewrite%s" "$@" < "$_entire_stdin" || exit $?
`, chainComment, backupSuffix, backupSuffix)
	if allowShFallback {
		chain += fmt.Sprintf(`elif [ -f "$_entire_hook_dir/post-rewrite%s" ]; then
    sh -e "$_entire_hook_dir/post-rewrite%s" "$@" < "$_entire_stdin" || exit $?
`, backupSuffix, backupSuffix)
	}
	chain += "fi\nexit $_entire_status\n"
	return replayPrefix + body + chain
}

// injectEntireManagedBlock inserts Entire's hook body into an existing user
// script so the original logic remains in the same tracked file.
func injectEntireManagedBlock(existing, entireContent string) string {
	existing = stripEntireManagedBlock(existing)
	shebang, rest := splitShebang(existing)
	entireBody := strings.TrimSpace(stripShebang(entireContent))

	var b strings.Builder
	if shebang != "" {
		b.WriteString(shebang)
		b.WriteByte('\n')
	} else {
		b.WriteString("#!/bin/sh\n")
	}
	b.WriteString(entireManagedBegin)
	b.WriteByte('\n')
	b.WriteString(entireBody)
	b.WriteByte('\n')
	b.WriteString(entireManagedEnd)
	b.WriteByte('\n')
	rest = strings.TrimLeft(rest, "\n")
	if rest != "" {
		b.WriteString(rest)
		if !strings.HasSuffix(rest, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func stripEntireManagedBlock(content string) string {
	return stripDelimitedBlock(content, entireManagedBegin, entireManagedEnd)
}

func stripDelimitedBlock(content, begin, end string) string {
	start := strings.Index(content, begin)
	if start < 0 {
		return content
	}
	// Match only a line-anchored end marker so a mention of `end` inside the
	// managed body (comment/string) cannot truncate the block early.
	endAbs := indexLineAnchored(content, end, start+len(begin))
	if endAbs < 0 {
		return content
	}
	after := content[endAbs+len(end):]
	after = strings.TrimPrefix(after, "\n")
	before := content[:start]
	if strings.HasSuffix(before, "\n\n") {
		before = strings.TrimSuffix(before, "\n")
	}
	return before + after
}

// indexLineAnchored returns the absolute index of needle in content at or after
// from, but only when needle starts a line (offset 0 or preceded by '\n') and
// ends at EOF or a newline. Returns -1 if no such match exists.
func indexLineAnchored(content, needle string, from int) int {
	if needle == "" || from < 0 {
		return -1
	}
	if from > len(content) {
		return -1
	}
	searchFrom := from
	for {
		rel := strings.Index(content[searchFrom:], needle)
		if rel < 0 {
			return -1
		}
		abs := searchFrom + rel
		atLineStart := abs == 0 || content[abs-1] == '\n'
		after := abs + len(needle)
		atLineEnd := after == len(content) || content[after] == '\n'
		if atLineStart && atLineEnd {
			return abs
		}
		searchFrom = abs + 1
	}
}

func stripShebang(content string) string {
	_, rest := splitShebang(content)
	return rest
}

func splitShebang(content string) (shebang, rest string) {
	if strings.HasPrefix(content, "#!") {
		if i := strings.IndexByte(content, '\n'); i >= 0 {
			return content[:i], content[i+1:]
		}
		return content, ""
	}
	return "", content
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
