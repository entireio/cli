package strategy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// hookIntegrationFault is a package-local test seam for failures at transaction
// boundaries. Production leaves it nil.
var hookIntegrationFault func(stage, path string) error

type hookIntegrationSnapshot struct {
	path   string
	root   *os.Root
	name   string
	data   []byte
	mode   os.FileMode
	exists bool
	dir    bool
}

type effectiveHooksRoot struct {
	root   *os.Root
	prefix string
	path   string
}

func (hooks effectiveHooksRoot) name(hook string) string {
	if hooks.prefix == "." || hooks.prefix == "" {
		return hook
	}
	return filepath.ToSlash(filepath.Join(hooks.prefix, hook))
}

func openEffectiveHooksRoot(ctx context.Context, repoRoot string) (effectiveHooksRoot, error) {
	hooksDir, err := getHooksDirInPath(ctx, repoRoot)
	if err != nil {
		return effectiveHooksRoot{}, err
	}
	commonDir, err := gitdir.CommonDirForWorktree(ctx, repoRoot)
	if err != nil {
		return effectiveHooksRoot{}, fmt.Errorf("resolve git common directory: %w", err)
	}
	anchors := []struct {
		path string
		root *os.Root
	}{
		{path: repoRoot},
		{path: commonDir},
	}
	anchors[0].root, err = worktreedir.OpenAt(repoRoot)
	if err != nil {
		return effectiveHooksRoot{}, fmt.Errorf("open worktree: %w", err)
	}
	anchors[1].root, err = gitdir.OpenAt(commonDir)
	if err != nil {
		return effectiveHooksRoot{}, fmt.Errorf("open git common directory: %w", err)
	}
	hooksComparePath := hooksDir
	if resolved, resolveErr := filepath.EvalSymlinks(filepath.Dir(hooksDir)); resolveErr == nil {
		hooksComparePath = filepath.Join(resolved, filepath.Base(hooksDir))
	}
	for _, anchor := range anchors {
		anchorComparePath := anchor.path
		if resolved, resolveErr := filepath.EvalSymlinks(anchor.path); resolveErr == nil {
			anchorComparePath = resolved
		}
		rel, relErr := filepath.Rel(anchorComparePath, hooksComparePath)
		if relErr == nil && filepath.IsLocal(rel) {
			return validateEffectiveHooksRoot(effectiveHooksRoot{root: anchor.root, prefix: filepath.ToSlash(rel), path: hooksDir})
		}
	}
	volume := filepath.VolumeName(hooksDir)
	fsRootPath := volume + string(os.PathSeparator)
	// A configured core.hooksPath may live outside both repository anchors. The
	// fixed root of its filesystem volume is trusted independently of that
	// configured path; hooksDir remains a checked local name beneath the root.
	fsRoot, err := osroot.Shared(fsRootPath)
	if err != nil {
		return effectiveHooksRoot{}, fmt.Errorf("open filesystem root for hooks: %w", err)
	}
	rel, err := filepath.Rel(fsRootPath, hooksDir)
	if err != nil || !filepath.IsLocal(rel) {
		return effectiveHooksRoot{}, fmt.Errorf("effective hooks path %s cannot be safely anchored", hooksDir)
	}
	return validateEffectiveHooksRoot(effectiveHooksRoot{root: fsRoot, prefix: filepath.ToSlash(rel), path: hooksDir})
}

func validateEffectiveHooksRoot(hooks effectiveHooksRoot) (effectiveHooksRoot, error) {
	info, err := osroot.LstatNoSymlinks(hooks.root, hooks.prefix)
	if os.IsNotExist(err) {
		return hooks, nil
	}
	if err != nil {
		return effectiveHooksRoot{}, fmt.Errorf("inspect effective hooks directory %s: %w", hooks.path, err)
	}
	if !info.IsDir() {
		return effectiveHooksRoot{}, fmt.Errorf("effective hooks path %s is not a real directory", hooks.path)
	}
	return hooks, nil
}

// EnsureGitHookIntegration installs the durable integration selected by the
// repository's hook manager. It never executes a hook manager binary.
func EnsureGitHookIntegration(ctx context.Context, absolutePath bool) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve worktree root: %w", err)
	}
	managers, err := detectHookManagersForIntegration(repoRoot)
	if err != nil {
		return 0, err
	}
	_, lefthook, err := selectLefthookIntegrationManager(managers)
	if err != nil {
		return 0, err
	}
	snapshots, err := snapshotHookIntegration(ctx, repoRoot)
	if err != nil {
		return 0, err
	}
	fail := func(primary error) (int, error) {
		return 0, errors.Join(primary, restoreHookIntegrationSnapshots(snapshots))
	}

	if !lefthook {
		written, installErr := InstallGitHook(ctx, true, absolutePath)
		if installErr != nil {
			return fail(installErr)
		}
		if err := integrationFault("final-verification", "native"); err != nil {
			return fail(err)
		}
		health := CheckGitHookIntegration(ctx)
		if health.Mode != GitHookIntegrationNative || health.State != GitHookIntegrationCurrent {
			return fail(fmt.Errorf("native Git hook integration verification failed: %s", health.Reason))
		}
		return written, nil
	}

	written, err := installLefthookFilesAt(ctx, repoRoot, absolutePath, func(name string) error {
		return integrationFault("rename", name)
	})
	if err != nil {
		return fail(err)
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return fail(fmt.Errorf("open worktree for Lefthook verification: %w", err))
	}
	cmdPrefix, err := hookCmdPrefix(absolutePath)
	if err != nil {
		return fail(err)
	}
	artifactsCurrent, err := inspectLefthookArtifacts(root, cmdPrefix)
	if err != nil {
		return fail(fmt.Errorf("verify Lefthook artifacts: %w", err))
	}
	if !artifactsCurrent {
		return fail(errors.New("verify Lefthook artifacts: installed artifacts are not current"))
	}
	if err := integrationFault("artifact-verification", "lefthook"); err != nil {
		return fail(err)
	}
	activeCurrent, err := inspectActiveHookDelivery(ctx, repoRoot)
	if err != nil {
		return fail(fmt.Errorf("inspect active Git hook delivery: %w", err))
	}
	if !activeCurrent {
		nativeWritten, installErr := InstallGitHook(ctx, true, absolutePath)
		if installErr != nil {
			return fail(installErr)
		}
		written += nativeWritten
	}
	if err := restoreProvenLefthookBridges(ctx, absolutePath); err != nil {
		return fail(err)
	}
	if err := integrationFault("final-verification", "lefthook"); err != nil {
		return fail(err)
	}
	health := CheckGitHookIntegration(ctx)
	if health.Mode != GitHookIntegrationLefthook || health.State != GitHookIntegrationCurrent {
		return fail(fmt.Errorf("lefthook integration verification failed: %s", health.Reason))
	}
	return written, nil
}

func inspectActiveHookDelivery(ctx context.Context, repoRoot string) (bool, error) {
	hooks, err := openEffectiveHooksRoot(ctx, repoRoot)
	if err != nil {
		return false, err
	}
	for _, hook := range gitHookNames {
		data, info, readErr := readOptionalRegular(hooks.root, hooks.name(hook))
		if readErr != nil {
			return false, readErr
		}
		if data == nil || info.Mode().Perm()&0o111 == 0 {
			return false, nil
		}
		content := string(data)
		nativeCurrent := currentNativeHookContent(content, hook)
		if !nativeCurrent && !looksLikeLefthookHook(data, hook) {
			return false, nil
		}
	}
	return true, nil
}

func restoreProvenLefthookBridges(ctx context.Context, absolutePath bool) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}
	hooks, err := openEffectiveHooksRoot(ctx, repoRoot)
	if err != nil {
		return err
	}
	cmdPrefix, err := hookCmdPrefix(absolutePath)
	if err != nil {
		return err
	}
	for _, spec := range buildHookSpecs(cmdPrefix) {
		hookName := hooks.name(spec.name)
		backupName := hookName + backupSuffix
		active, _, activeErr := readOptionalRegular(hooks.root, hookName)
		backup, _, backupErr := readOptionalRegular(hooks.root, backupName)
		if activeErr != nil || backupErr != nil || active == nil || backup == nil || string(active) != generateChainedContent(spec.content, spec.name) {
			continue
		}
		if !looksLikeLefthookHook(backup, spec.name) {
			continue
		}
		if err := integrationFault("saved-hook-restore", spec.name); err != nil {
			return err
		}
		parent, leaf, closeParent, err := osroot.OpenParentNoSymlinks(hooks.root, hookName)
		if err != nil {
			return fmt.Errorf("open effective hooks directory for %s: %w", spec.name, err)
		}
		err = parent.Rename(leaf+backupSuffix, leaf)
		closeParent()
		if err != nil {
			return fmt.Errorf("restore saved Lefthook %s hook: %w", spec.name, err)
		}
	}
	return nil
}

func looksLikeLefthookHook(data []byte, hook string) bool {
	content := string(data)
	const prefix = `#!/bin/sh

if [ "$LEFTHOOK_VERBOSE" = "1" -o "$LEFTHOOK_VERBOSE" = "true" ]; then
  set -x
fi

if [ "$LEFTHOOK" = "0" ]; then
  exit 0
fi

call_lefthook()
{
`
	suffix := "}\n\ncall_lefthook run \"" + hook + "\" \"$@\"\n"
	prefixWithGeneratedComment := strings.Replace(prefix, "\ncall_lefthook()", "\n# lefthook generated wrapper\ncall_lefthook()", 1)
	matchedPrefix := prefix
	if !strings.HasPrefix(content, matchedPrefix) {
		matchedPrefix = prefixWithGeneratedComment
	}
	if !strings.HasPrefix(content, matchedPrefix) || !strings.HasSuffix(content, suffix) {
		return false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(content, matchedPrefix), suffix)
	return knownLefthookV2Body(body) || knownDirectEntireLefthookBody(body, hook)
}

func knownLefthookV2Body(body string) bool {
	if body == "  lefthook \"$@\"\n" {
		return true
	}
	const installedBranchPrefix = `  if test -n "$LEFTHOOK_BIN"
  then
    "$LEFTHOOK_BIN" "$@"
  elif lefthook -h >/dev/null 2>&1
  then
    lefthook "$@"
  elif `
	const installedProbeSuffix = ` -h >/dev/null 2>&1
  then
    `
	if !strings.HasPrefix(body, installedBranchPrefix) {
		return false
	}
	remainder := strings.TrimPrefix(body, installedBranchPrefix)
	probeEnd := strings.Index(remainder, installedProbeSuffix)
	if probeEnd <= 0 {
		return false
	}
	installedPath := remainder[:probeEnd]
	if !filepath.IsAbs(installedPath) || strings.ContainsAny(installedPath, "\n\r\t") {
		return false
	}
	const generatedTail = ` "$@"
  else
    dir="$(git rev-parse --show-toplevel)"
    osArch=$(uname | tr '[:upper:]' '[:lower:]')
    cpuArch=$(uname -m | sed 's/aarch64/arm64/;s/x86_64/x64/')
    if test -f "$dir/node_modules/lefthook-${osArch}-${cpuArch}/bin/lefthook"
    then
      "$dir/node_modules/lefthook-${osArch}-${cpuArch}/bin/lefthook" "$@"
    elif test -f "$dir/node_modules/@evilmartians/lefthook/bin/lefthook-${osArch}-${cpuArch}/lefthook"
    then
      "$dir/node_modules/@evilmartians/lefthook/bin/lefthook-${osArch}-${cpuArch}/lefthook" "$@"
    elif test -f "$dir/node_modules/@evilmartians/lefthook-installer/bin/lefthook"
    then
      "$dir/node_modules/@evilmartians/lefthook-installer/bin/lefthook" "$@"
    elif test -f "$dir/node_modules/lefthook/bin/index.js"
    then
      "$dir/node_modules/lefthook/bin/index.js" "$@"
    elif go tool lefthook -h >/dev/null 2>&1
    then
      go tool lefthook "$@"
    elif bundle exec lefthook -h >/dev/null 2>&1
    then
      bundle exec lefthook "$@"
    elif yarn lefthook -h >/dev/null 2>&1
    then
      yarn lefthook "$@"
    elif pnpm lefthook -h >/dev/null 2>&1
    then
      pnpm lefthook "$@"
    elif swift package lefthook >/dev/null 2>&1
    then
      swift package --build-path .build/lefthook --disable-sandbox lefthook "$@"
    elif command -v mint >/dev/null 2>&1
    then
      mint run csjones/lefthook-plugin "$@"
    elif uv run lefthook -h >/dev/null 2>&1
    then
      uv run lefthook "$@"
    elif mise exec -- lefthook -h >/dev/null 2>&1
    then
      mise exec -- lefthook "$@"
    elif devbox run lefthook -h >/dev/null 2>&1
    then
      devbox run lefthook "$@"
    else
      echo "Can't find lefthook in PATH"
    fi
  fi
`
	want := installedPath + installedProbeSuffix + installedPath + generatedTail
	return remainder == want
}

func knownDirectEntireLefthookBody(body, hook string) bool {
	const integrationLauncher = "  shift\n  hook=\"$1\"\n  shift\n  exec \"$PWD/.lefthook-local/$hook/entire.sh\" \"$@\"\n"
	e2eLauncher := "  shift 2\n  sh \".lefthook-local/" + hook + "/entire.sh\" \"$@\"\n"
	return body == integrationLauncher || body == e2eLauncher
}

// RemoveGitHookIntegration removes only artifacts whose exact ownership marker
// proves they belong to Entire, together with the legacy native wrappers.
func RemoveGitHookIntegration(ctx context.Context) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve worktree root: %w", err)
	}
	snapshots, err := snapshotHookIntegration(ctx, repoRoot)
	if err != nil {
		return 0, err
	}
	fail := func(primary error) (int, error) {
		return 0, errors.Join(primary, restoreHookIntegrationSnapshots(snapshots))
	}
	removed, err := removeOwnedLefthookArtifacts(ctx, repoRoot)
	if err != nil {
		return fail(err)
	}
	nativeRemoved, err := removeNativeGitHooksSafe(ctx, repoRoot)
	if err != nil {
		return fail(err)
	}
	if err := integrationFault("final-verification", "remove"); err != nil {
		return fail(err)
	}
	if err := verifyGitHookIntegrationRemoved(ctx, repoRoot); err != nil {
		return fail(err)
	}
	return removed + nativeRemoved, nil
}

func removeNativeGitHooksSafe(ctx context.Context, repoRoot string) (int, error) {
	hooks, err := openEffectiveHooksRoot(ctx, repoRoot)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, hook := range gitHookNames {
		hookName := hooks.name(hook)
		backupName := hookName + backupSuffix
		active, _, activeErr := readOptionalRegular(hooks.root, hookName)
		if activeErr != nil {
			return 0, fmt.Errorf("inspect native %s hook: %w", hook, activeErr)
		}
		backup, _, backupErr := readOptionalRegular(hooks.root, backupName)
		if backupErr != nil {
			return 0, fmt.Errorf("inspect native %s backup: %w", hook, backupErr)
		}
		hookIsOurs := active != nil && strings.Contains(string(active), entireHookMarker)
		if active != nil && !hookIsOurs && backup != nil {
			fmt.Fprintf(os.Stderr, "[entire] Warning: %s was modified since install; backup %s%s left in place\n", hook, hook, backupSuffix)
			continue
		}
		if backup != nil && (active == nil || hookIsOurs) {
			parent, leaf, closeParent, openErr := osroot.OpenParentNoSymlinks(hooks.root, hookName)
			if openErr != nil {
				return 0, fmt.Errorf("open effective hooks directory for %s: %w", hook, openErr)
			}
			renameErr := parent.Rename(leaf+backupSuffix, leaf)
			closeParent()
			if renameErr != nil {
				return 0, fmt.Errorf("restore %s%s: %w", hook, backupSuffix, renameErr)
			}
			if hookIsOurs {
				removed++
			}
			continue
		}
		if hookIsOurs {
			if removeErr := osroot.RemoveNoSymlinks(hooks.root, hookName); removeErr != nil {
				return 0, fmt.Errorf("remove native %s hook: %w", hook, removeErr)
			}
			removed++
		}
	}
	return removed, nil
}

// AnyGitHookIntegrationInstalled reports whether any native or Lefthook-owned
// Entire hook artifact remains. Uninstall uses this after a partial run has
// already removed .entire, so it cannot rely on enabled settings for discovery.
func AnyGitHookIntegrationInstalled(ctx context.Context) bool {
	if AnyGitHookInstalled(ctx) {
		return true
	}
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return false
	}
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return false
	}
	if data, _, readErr := readOptionalRegular(root, lefthookLocalConfigName); readErr == nil && data != nil {
		owned, ownershipErr := hasOwnedLefthookConfigEntries(data)
		if ownershipErr == nil && owned {
			return true
		}
	}
	for _, hook := range gitHookNames {
		data, _, readErr := readOptionalRegular(root, lefthookScriptPath(hook))
		if readErr == nil && lefthookScriptOwned(string(data)) {
			return true
		}
	}
	return false
}

func verifyGitHookIntegrationRemoved(ctx context.Context, repoRoot string) error {
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return fmt.Errorf("open worktree: %w", err)
	}
	if data, _, err := readOptionalRegular(root, lefthookLocalConfigName); err == nil && data != nil {
		owned, ownershipErr := hasOwnedLefthookConfigEntries(data)
		if ownershipErr != nil {
			return fmt.Errorf("verify %s ownership: %w", lefthookLocalConfigName, ownershipErr)
		}
		if owned {
			return errors.New("lefthook local config still contains Entire-owned entries")
		}
	} else if err != nil {
		return fmt.Errorf("verify %s removal: %w", lefthookLocalConfigName, err)
	}
	for _, hook := range gitHookNames {
		if data, _, err := readOptionalRegular(root, lefthookScriptPath(hook)); err == nil && data != nil {
			if lefthookScriptOwned(string(data)) {
				return fmt.Errorf("owned Lefthook script %s remains", hook)
			}
		} else if err != nil {
			return fmt.Errorf("verify Lefthook script %s removal: %w", hook, err)
		}
	}
	hooks, err := openEffectiveHooksRoot(ctx, repoRoot)
	if err != nil {
		return err
	}
	for _, hook := range gitHookNames {
		if data, _, readErr := readOptionalRegular(hooks.root, hooks.name(hook)); readErr == nil && data != nil {
			if strings.Contains(string(data), entireHookMarker) {
				return fmt.Errorf("native Entire hook %s remains", hook)
			}
		} else if readErr != nil {
			return fmt.Errorf("verify native hook %s removal: %w", hook, readErr)
		}
	}
	return nil
}

func removeOwnedLefthookArtifacts(ctx context.Context, repoRoot string) (int, error) {
	removed := 0
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("open worktree: %w", err)
	}
	if data, info, err := readOptionalRegular(root, lefthookLocalConfigName); err == nil && data != nil {
		updated, changed, updateErr := removeOwnedLefthookConfig(data)
		if updateErr != nil {
			return 0, updateErr
		}
		if changed {
			if err := integrationFault("write", lefthookLocalConfigName); err != nil {
				return 0, err
			}
			if len(strings.TrimSpace(string(updated))) == 0 || string(updated) == "{}\n" {
				if err := osroot.RemoveNoSymlinks(root, lefthookLocalConfigName); err != nil {
					return 0, fmt.Errorf("remove %s: %w", lefthookLocalConfigName, err)
				}
			} else if err := jsonutil.WriteFileAtomicIn(root, lefthookLocalConfigName, updated, info.Mode().Perm()); err != nil {
				return 0, fmt.Errorf("write %s: %w", lefthookLocalConfigName, err)
			}
			removed++
		}
	} else if err != nil {
		return 0, fmt.Errorf("read %s: %w", lefthookLocalConfigName, err)
	}
	for _, hook := range gitHookNames {
		name := lefthookScriptPath(hook)
		data, _, err := readOptionalRegular(root, name)
		if err != nil {
			return 0, fmt.Errorf("inspect owned Lefthook script %s: %w", hook, err)
		}
		if data == nil {
			continue
		}
		if !lefthookScriptOwned(string(data)) {
			continue
		}
		if err := osroot.RemoveNoSymlinks(root, name); err != nil {
			return 0, fmt.Errorf("remove owned Lefthook script %s: %w", hook, err)
		}
		removed++
	}
	commonDir, err := gitdir.CommonDirForWorktree(ctx, repoRoot)
	if err != nil {
		return 0, fmt.Errorf("resolve git common directory: %w", err)
	}
	gitRoot, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return 0, fmt.Errorf("open git common directory: %w", err)
	}
	if data, info, readErr := readOptionalRegular(gitRoot, "info/exclude"); readErr == nil && data != nil {
		updated := removeLefthookExcludeLines(data)
		if string(updated) != string(data) {
			if err := jsonutil.WriteFileAtomicIn(gitRoot, "info/exclude", updated, info.Mode().Perm()); err != nil {
				return 0, fmt.Errorf("write git exclude: %w", err)
			}
		}
	} else if readErr != nil {
		return 0, fmt.Errorf("read git exclude: %w", readErr)
	}
	return removed, nil
}

func removeLefthookExcludeLines(data []byte) []byte {
	return []byte(strings.Replace(string(data), lefthookExcludeBlock(), "", 1))
}

func snapshotHookIntegration(ctx context.Context, repoRoot string) ([]hookIntegrationSnapshot, error) {
	commonDir, err := gitdir.CommonDirForWorktree(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve git common directory: %w", err)
	}
	hooks, err := openEffectiveHooksRoot(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	workRoot, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open worktree: %w", err)
	}
	gitRoot, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, fmt.Errorf("open git common directory: %w", err)
	}
	snapshots := []hookIntegrationSnapshot{
		{path: filepath.Join(repoRoot, lefthookLocalConfigName), root: workRoot, name: lefthookLocalConfigName},
		{path: filepath.Join(commonDir, "info", "exclude"), root: gitRoot, name: "info/exclude"},
	}
	for _, hook := range gitHookNames {
		snapshots = append(snapshots,
			hookIntegrationSnapshot{path: filepath.Join(repoRoot, filepath.FromSlash(lefthookScriptPath(hook))), root: workRoot, name: lefthookScriptPath(hook)},
			hookIntegrationSnapshot{path: filepath.Join(hooks.path, hook), root: hooks.root, name: hooks.name(hook)},
			hookIntegrationSnapshot{path: filepath.Join(hooks.path, hook) + backupSuffix, root: hooks.root, name: hooks.name(hook) + backupSuffix},
		)
	}
	for i := range snapshots {
		snapshot := &snapshots[i]
		info, statErr := osroot.LstatNoSymlinks(snapshot.root, snapshot.name)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, fmt.Errorf("inspect transaction path %s: %w", snapshot.path, statErr)
		}
		if info.IsDir() {
			snapshot.mode, snapshot.exists, snapshot.dir = info.Mode().Perm(), true, true
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("transaction path %s is not a regular file", snapshot.path)
		}
		data, readErr := osroot.ReadFileNoFollow(snapshot.root, snapshot.name)
		if readErr != nil {
			return nil, fmt.Errorf("read transaction path %s: %w", snapshot.path, readErr)
		}
		snapshot.data, snapshot.mode, snapshot.exists = data, info.Mode().Perm(), true
	}
	return snapshots, nil
}

func restoreHookIntegrationSnapshots(snapshots []hookIntegrationSnapshot) error {
	var errs []error
	for i := len(snapshots) - 1; i >= 0; i-- {
		snapshot := snapshots[i]
		if snapshot.dir {
			// Artifact operations reject directories at file paths without
			// modifying them, so the captured obstruction is already restored.
			continue
		}
		if snapshot.exists {
			if err := jsonutil.WriteFileAtomicIn(snapshot.root, snapshot.name, snapshot.data, snapshot.mode); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", snapshot.path, err))
			}
			continue
		}
		if err := osroot.RemoveNoSymlinks(snapshot.root, snapshot.name); err != nil {
			errs = append(errs, fmt.Errorf("remove newly-created %s: %w", snapshot.path, err))
		}
	}
	return errors.Join(errs...)
}

func integrationFault(stage, path string) error {
	if hookIntegrationFault == nil {
		return nil
	}
	if err := hookIntegrationFault(stage, path); err != nil {
		return fmt.Errorf("hook integration %s failure at %s: %w", stage, path, err)
	}
	return nil
}
