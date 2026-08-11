package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"

	"charm.land/huh/v2"
)

// globalTrackingHint is the one-line pointer printed by non-interactive
// enable paths when the machine-wide question has never been answered.
const globalTrackingHint = "To track every repo on this machine automatically, run 'entire enable --global'."

// runEnableGlobalMode turns the user-global tracking tier on. It only writes
// the user settings file (preserving existing exclude lists) and never touches
// repo-level settings, so it works outside a git repository too.
func runEnableGlobalMode(ctx context.Context, w io.Writer) error {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		return unreadableUserSettingsError(err)
	}
	if us.Global == nil {
		us.Global = &settings.GlobalConfig{}
	}
	us.Global.Enabled = true
	if err := settings.SaveUserSettings(ctx, us); err != nil {
		return fmt.Errorf("saving user settings: %w", err)
	}
	fmt.Fprintln(w, "Global tracking enabled.")
	fmt.Fprintln(w, "Entire now tracks agent sessions in every repo on this machine that has no repo-level setup.")
	return nil
}

// runDisableGlobalMode turns the user-global tracking tier off. The answer is
// durable: the file keeps global.enabled=false (rather than being removed), so
// the setup wizard never re-asks. Repo-level settings are not touched.
func runDisableGlobalMode(ctx context.Context, w io.Writer) error {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		return unreadableUserSettingsError(err)
	}
	if us.Global == nil {
		us.Global = &settings.GlobalConfig{}
	}
	us.Global.Enabled = false
	if err := settings.SaveUserSettings(ctx, us); err != nil {
		return fmt.Errorf("saving user settings: %w", err)
	}
	fmt.Fprintln(w, "Global tracking disabled.")
	return nil
}

// unreadableUserSettingsError turns a LoadUserSettings failure into an
// actionable message: the strict decoder also rejects files written by a
// newer CLI, so name the file and both ways out.
func unreadableUserSettingsError(err error) error {
	return fmt.Errorf("cannot update user settings at %s: %w; upgrade entire or remove the unknown key",
		settings.UserSettingsPath(), err)
}

// maybeAskGlobalTracking asks the one-time machine-wide tracking question
// during interactive setup. It never asks when the global tier is already
// configured (either answer), and never prompts in non-interactive contexts
// (--yes, no TTY) — those get the one-line hint instead. An answered prompt
// is persisted either way, so the question is never asked twice; a cancelled
// prompt saves nothing, so a later enable can ask again.
func maybeAskGlobalTracking(ctx context.Context, w io.Writer, opts EnableOptions) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil || us.Global != nil {
		// A malformed user settings file must never be overwritten from a
		// side prompt; a configured one must never re-ask.
		return
	}
	if opts.Yes || !interactive.CanPromptInteractively() {
		fmt.Fprintln(w, globalTrackingHint)
		return
	}
	enable := false
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Also track every repo on this machine automatically?").
				Description("Applies to repos without their own Entire setup.").
				Affirmative("Yes").
				Negative("No").
				Value(&enable),
		),
	)
	if err := form.Run(); err != nil {
		// Ctrl-C stays silent (nothing saved, a later enable asks again);
		// any other prompt failure must at least be diagnosable.
		if !errors.Is(err, huh.ErrUserAborted) && !errors.Is(err, context.Canceled) {
			logging.Debug(ctx, "global tracking prompt failed", "error", err)
		}
		return
	}
	us.Global = &settings.GlobalConfig{Enabled: enable}
	if err := settings.SaveUserSettings(ctx, us); err != nil {
		fmt.Fprintf(w, "Warning: could not save the global tracking answer: %v\n", err)
		return
	}
	if enable {
		fmt.Fprintln(w, "  ✓ Global tracking enabled")
	}
}

// printGlobalTrackingHintIfUnconfigured prints the enable --global pointer on
// enable paths that must never prompt (--agent), when the machine-wide
// question has never been answered.
func printGlobalTrackingHintIfUnconfigured(ctx context.Context, w io.Writer) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil || us.Global != nil {
		return
	}
	fmt.Fprintln(w, globalTrackingHint)
}

// globalRuntimeSubdirs are the runtime subtrees invisible routing places
// under the worktree's namespace dir (<git-common-dir>/entire/worktree/
// <worktree-key>) for globally tracked repos, relative to that base and to
// the worktree's .entire directory alike.
var globalRuntimeSubdirs = []string{"metadata", "logs", "tmp"}

// maybeMigrateGlobalRuntimeData moves THIS worktree's invisible-routed
// runtime data (<git-common-dir>/entire/worktree/<worktree-key>/
// {metadata,logs,tmp}) into the worktree's .entire directory. Repo-level
// enable flows call it BEFORE writing .entire/settings.json: that write
// flips path routing to the worktree, which would otherwise strand the
// routed files in .git. Only the current worktree's namespace is touched —
// sibling worktrees of the same clone keep their own namespaces (and their
// in-flight sessions) untouched. It triggers when the clone preferences
// carry the globally_enabled marker or when the routed directory is
// non-empty. Best-effort by contract — enable never aborts on migration
// failure; files that could not be moved stay in place for a later retry and
// the outcome is summarized in one line.
func maybeMigrateGlobalRuntimeData(ctx context.Context, w io.Writer) {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return
	}
	// session.GetGitCommonDir can return a cwd-relative path (e.g. ".git").
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return
	}
	source, err := paths.InvisibleRuntimeDir(commonDir, root)
	if err != nil {
		return
	}

	triggered := false
	if prefs, prefsErr := settings.LoadClonePreferences(ctx); prefsErr == nil && prefs.GloballyEnabled {
		triggered = true
	}
	if !triggered {
		entries, readErr := os.ReadDir(source)
		if readErr != nil || len(entries) == 0 {
			return
		}
	}

	var moved, skipped, failed int
	for _, sub := range globalRuntimeSubdirs {
		m, s, f := moveDirContents(filepath.Join(source, sub), filepath.Join(root, paths.EntireDir, sub))
		moved += m
		skipped += s
		failed += f
	}
	removeEmptyDirTree(source)
	// The parent (<git-common-dir>/entire/worktree) is shared with sibling
	// worktrees' namespaces; os.Remove only succeeds once the last namespace
	// is gone, which is exactly the desired cleanup.
	_ = os.Remove(filepath.Dir(source))

	if moved+skipped+failed == 0 {
		return
	}
	// ✓ elsewhere in the enable output means unqualified success, so drop it
	// when anything failed, and name the source so leftovers are findable.
	prefix := "  ✓ "
	if failed > 0 {
		prefix = "  "
	}
	line := fmt.Sprintf("%sMoved %d globally-tracked session file(s) into .entire/", prefix, moved)
	if skipped > 0 {
		line += fmt.Sprintf(", %d already present", skipped)
	}
	if failed > 0 {
		line += fmt.Sprintf(", %d could not be moved (left in %s)", failed, source)
	}
	fmt.Fprintln(w, line)
}

// migrateRenameFile is the rename used by moveDirContents; a seam so tests
// can force the cross-device (EXDEV) fallback deterministically.
var migrateRenameFile = os.Rename

// moveDirContents moves every file under src into the same relative location
// under dst, creating directories as needed. Files already present in dst are
// skipped (left in src). Returns moved/skipped/failed counts; a missing src is
// (0, 0, 0).
func moveDirContents(src, dst string) (moved, skipped, failed int) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, 0, 0
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			m, s, f := moveDirContents(srcPath, dstPath)
			moved, skipped, failed = moved+m, skipped+s, failed+f
			continue
		}
		if _, statErr := os.Lstat(dstPath); statErr == nil {
			skipped++
			continue
		}
		//nolint:gosec // G301: runtime-data dirs match the .entire directory permissions
		if mkErr := os.MkdirAll(dst, 0o755); mkErr != nil {
			failed++
			continue
		}
		if renameErr := migrateRenameFile(srcPath, dstPath); renameErr != nil {
			// Rename fails with EXDEV when the git common dir and the
			// worktree live on different filesystems (--separate-git-dir,
			// linked worktree on another volume); fall back to copy+remove.
			if copyErr := copyFileThenRemove(srcPath, dstPath); copyErr != nil {
				failed++
				continue
			}
		}
		moved++
	}
	return moved, skipped, failed
}

// copyFileThenRemove copies src to dst (temp file in dst's directory, then
// rename, so a partial copy is never left at dst) and removes src on success.
// Only regular files are copied; anything else stays behind as failed.
func copyFileThenRemove(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", src)
	}
	in, err := os.Open(src) //nolint:gosec // path is derived from the git common dir, not user input
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy contents: %w", err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	removeTmp = false
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove source: %w", err)
	}
	return nil
}

// removeEmptyDirTree removes the now-empty directories left behind by
// moveDirContents, deepest first, including root itself. Non-empty
// directories (skipped or failed files) are left in place.
func removeEmptyDirTree(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			removeEmptyDirTree(filepath.Join(root, entry.Name()))
		}
	}
	// os.Remove fails on non-empty directories, which is exactly the
	// keep-what-couldn't-move behavior wanted here.
	if err := os.Remove(root); err != nil {
		return
	}
}
