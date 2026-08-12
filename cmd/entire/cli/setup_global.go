package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"charm.land/huh/v2"
)

// globalTrackingHint is the one-line pointer printed by non-interactive
// enable paths when the machine-wide question has never been answered.
const globalTrackingHint = "To track every repo on this machine automatically, run 'entire enable --global'."

// runEnableGlobalMode turns the user-global tracking tier on. It only writes
// the user settings file (preserving existing exclude lists) and never touches
// repo-level settings, so it works outside a git repository too.
func runEnableGlobalMode(ctx context.Context, w io.Writer) error {
	// Load first for the actionable unreadable-file message. The write below
	// re-loads under the user-settings lock, so this is a UX probe, not the
	// consistency mechanism.
	if _, err := settings.LoadUserSettings(ctx); err != nil {
		return unreadableUserSettingsError(err)
	}
	kept := 0
	if err := settings.ModifyUserSettings(ctx, func(us *settings.UserSettings) error {
		if us.Global == nil {
			us.Global = &settings.GlobalConfig{}
		}
		us.Global.Enabled = true
		kept = len(us.Global.ExcludePaths) + len(us.Global.ExcludeOrigins)
		return nil
	}); err != nil {
		return fmt.Errorf("saving user settings: %w", err)
	}
	fmt.Fprintln(w, "Global tracking enabled.")
	fmt.Fprintln(w, "Entire now tracks agent sessions in every repo on this machine that has no repo-level setup and matches no exclude pattern.")
	if kept > 0 {
		fmt.Fprintf(w, "Keeping %d exclude pattern(s) from your user settings.\n", kept)
	}
	succeeded, supported := installUserAgentHooks(ctx, w)
	if supported > 0 && succeeded == 0 {
		// The setting is on, but with zero agents covered no session in a
		// never-enabled repo can fire a hook: exiting 0 here would report a
		// tracking state that does not exist. Partial success stays exit 0.
		fmt.Fprintln(w, "No agent has user-level hooks installed — global tracking will not capture any sessions until the errors above are fixed and 'entire enable --global' is re-run.")
		return errors.New("user-level agent hooks could not be installed for any agent")
	}
	return nil
}

// installUserAgentHooks installs user-level agent hooks for every registered
// agent that supports them and reports the outcome per agent. User-level
// hooks are what let a session in a never-enabled repo fire a hook at all, so
// they are the activation step of enable --global. Per-agent failures are
// reported and skipped: global tracking is already on at this point, and one
// broken user config file must not block the other agents. Writes only
// user-scope config — never a repo file. Returns how many supporting agents
// ended up with hooks installed, and how many support them at all, so callers
// can detect the zero-coverage outcome.
func installUserAgentHooks(ctx context.Context, w io.Writer) (succeeded, supported int) {
	fmt.Fprintln(w, "User-level agent hooks:")
	supports, unsupportedNames := agent.UserHookSupports()
	for _, ua := range supports {
		supported++
		count, err := ua.Support.InstallUserHooks(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(w, "  ! %s: install failed: %v\n", ua.Name, err)
			continue
		case count == 0:
			fmt.Fprintf(w, "  ✓ %s: already installed\n", ua.Name)
		default:
			fmt.Fprintf(w, "  ✓ %s: installed\n", ua.Name)
		}
		succeeded++
	}
	if len(unsupportedNames) > 0 {
		names := make([]string, len(unsupportedNames))
		for i, n := range unsupportedNames {
			names[i] = string(n)
		}
		fmt.Fprintf(w, "  - user-level hooks not supported: %s\n", strings.Join(names, ", "))
	}
	return succeeded, supported
}

// maybeRemoveUserAgentHooks offers to remove Entire's user-level agent hooks
// after disable --global. Removal only ever touches entries recognized as
// Entire's. Interactive runs confirm first; non-interactive runs remove ours
// without asking. A declined or aborted prompt leaves the hooks in place —
// they gate on the (now off) global tier, so they are inert until removed.
// An agent whose user-level config cannot be read gets a `!` line instead of
// being silently skipped: its hooks (if any) remain, and the user must know.
func maybeRemoveUserAgentHooks(ctx context.Context, w io.Writer) {
	supports, _ := agent.UserHookSupports()
	var installed []agent.UserHookAgent
	var unreadable []string
	for _, ua := range supports {
		ok, err := ua.Support.AreUserHooksInstalled(ctx)
		switch {
		case err != nil:
			unreadable = append(unreadable, fmt.Sprintf("  ! %s: could not remove: %v", ua.Name, err))
		case ok:
			installed = append(installed, ua)
		}
	}
	for _, line := range unreadable {
		fmt.Fprintln(w, line)
	}
	if len(installed) == 0 {
		return
	}
	remove := true
	if interactive.CanPromptInteractively() {
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Also remove Entire's user-level agent hooks?").
					Description("Removes only Entire's entries from the agents' user-level settings.").
					Affirmative("Yes").
					Negative("No").
					Value(&remove),
			),
		)
		if err := form.RunWithContext(ctx); err != nil {
			if !errors.Is(err, huh.ErrUserAborted) && !errors.Is(err, context.Canceled) {
				logging.Debug(ctx, "user hook removal prompt failed", "error", err)
			}
			remove = false
		}
	}
	if !remove {
		fmt.Fprintln(w, "User-level agent hooks left in place (inert while global tracking is off); run 'entire disable --global' again to remove them.")
		return
	}
	for _, ua := range installed {
		if err := ua.Support.UninstallUserHooks(ctx); err != nil {
			fmt.Fprintf(w, "  ! %s: could not remove user-level hooks: %v\n", ua.Name, err)
			continue
		}
		fmt.Fprintf(w, "  ✓ %s: user-level hooks removed\n", ua.Name)
	}
}

// runDisableGlobalMode turns the user-global tracking tier off. The answer is
// durable: the file keeps global.enabled=false (rather than being removed), so
// the setup wizard never re-asks. Repo-level settings are not touched.
func runDisableGlobalMode(ctx context.Context, w io.Writer) error {
	// Same shape as runEnableGlobalMode: probe for the actionable message,
	// then read-modify-write under the user-settings lock.
	if _, err := settings.LoadUserSettings(ctx); err != nil {
		return unreadableUserSettingsError(err)
	}
	if err := settings.ModifyUserSettings(ctx, func(us *settings.UserSettings) error {
		if us.Global == nil {
			us.Global = &settings.GlobalConfig{}
		}
		us.Global.Enabled = false
		return nil
	}); err != nil {
		return fmt.Errorf("saving user settings: %w", err)
	}
	fmt.Fprintln(w, "Global tracking disabled.")
	maybeRemoveUserAgentHooks(ctx, w)
	return nil
}

// unreadableUserSettingsError turns a LoadUserSettings failure into an
// actionable message: the strict decoder also rejects files written by a
// newer CLI, so name the file and both ways out.
func unreadableUserSettingsError(err error) error {
	return fmt.Errorf("cannot update user settings at %s: %w; fix or remove the file (an unknown key means a newer entire wrote it — upgrade instead)",
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
	if err != nil {
		// A malformed user settings file must never be overwritten from a
		// side prompt — but full silence would hide the problem, so name the
		// file and the ways out (one line, enable itself still succeeds).
		fmt.Fprintf(w, "Warning: %v\n", unreadableUserSettingsError(err))
		return
	}
	if us.GlobalConfigured() {
		// A configured tier (either answer) must never re-ask.
		return
	}
	if opts.Yes || !interactive.CanPromptInteractively() {
		fmt.Fprintln(w, globalTrackingHint)
		return
	}
	enable, err := askGlobalTrackingConfirm(ctx)
	if err != nil {
		// Ctrl-C stays silent (nothing saved, a later enable asks again);
		// any other prompt failure must at least be diagnosable.
		if !errors.Is(err, huh.ErrUserAborted) && !errors.Is(err, context.Canceled) {
			logging.Debug(ctx, "global tracking prompt failed", "error", err)
		}
		return
	}
	// Persist through a locked read-modify-write of the CURRENT file state:
	// the prompt can stay open for a while, and saving the pre-prompt
	// snapshot would clobber anything (say, exclude lists) another process
	// wrote in that window. And if the tier became CONFIGURED in that window
	// (e.g. `entire disable --global` in another terminal), the question
	// this prompt answered no longer exists — persisting would overturn
	// that explicit choice. Explicit off is durable.
	saveErr := settings.ModifyUserSettings(ctx, func(cur *settings.UserSettings) error {
		if cur.GlobalConfigured() {
			return errGlobalAnswerSuperseded
		}
		cur.Global = &settings.GlobalConfig{Enabled: enable}
		return nil
	})
	if errors.Is(saveErr, errGlobalAnswerSuperseded) {
		return
	}
	if saveErr != nil {
		fmt.Fprintf(w, "Warning: could not save the global tracking answer: %v\n", saveErr)
		return
	}
	if enable {
		fmt.Fprintln(w, "  ✓ Global tracking enabled")
		// The wizard's yes is the same commitment as enable --global, so it
		// triggers the same user-level hook install (user-scope config only).
		// The wizard is best-effort (enable itself already succeeded), so the
		// zero-coverage outcome warns instead of failing the command.
		succeeded, supported := installUserAgentHooks(ctx, w)
		if supported > 0 && succeeded == 0 {
			fmt.Fprintln(w, "No agent has user-level hooks installed — global tracking will not capture any sessions until the errors above are fixed and 'entire enable --global' is re-run.")
		}
	}
}

// errGlobalAnswerSuperseded aborts the wizard's locked write when the global
// tier became configured while the prompt was open: the answer is stale, and
// writing it would overturn the configuration made in that window. Checked
// inside the ModifyUserSettings callback, so the race is closed under the
// user-settings lock, not merely narrowed.
var errGlobalAnswerSuperseded = errors.New("global tier configured while the prompt was open")

// askGlobalTrackingConfirm runs the machine-wide tracking confirm prompt and
// returns the answer. A package var (same seam pattern as migrateRenameFile)
// so tests can pin the persistence contract of each outcome — durable yes,
// durable no, cancelled — without a TTY.
var askGlobalTrackingConfirm = func(ctx context.Context) (bool, error) {
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
	if err := form.RunWithContext(ctx); err != nil {
		return false, err //nolint:wrapcheck // caller classifies huh.ErrUserAborted/context.Canceled
	}
	return enable, nil
}

// printGlobalTrackingHintIfUnconfigured prints the enable --global pointer on
// enable paths that must never prompt (--agent), when the machine-wide
// question has never been answered.
func printGlobalTrackingHintIfUnconfigured(ctx context.Context, w io.Writer) {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil || us.GlobalConfigured() {
		return
	}
	fmt.Fprintln(w, globalTrackingHint)
}

// ensureRepoLevelTakeover runs the bookkeeping owed whenever repo-level
// settings take over a clone the user-global tier was (or may have been)
// tracking. It is invoked from EVERY path that creates the first repo-level
// settings file — the interactive and --agent enable flows, the raw
// enable/disable flag flip (a bare `entire disable` also creates the routing
// discriminator) — plus `entire enable` on an already-configured repo, which
// is the user-initiated recovery point for clones a pre-fix binary stranded.
//
// It self-guards on the migration's own trigger logic (the clone's
// global_setup_completed marker, or a non-empty routed runtime dir), so it is
// a cheap no-op for repos the global tier never touched. When the guard
// holds it, in order:
//  1. ensures .entire/.gitignore exists, so the migrated runtime files are
//     never visible in git status (order is load-bearing);
//  2. migrates this worktree's routed runtime data into .entire/;
//  3. clears the global_setup_completed marker — repo-level machinery owns
//     hooks now, and a stale marker would short-circuit the lazy global
//     setup if the repo ever returns to global-only tracking.
//
// It always clears the invisible-routing cache: the settings file being
// created (or having been created) is the routing discriminator, and the
// writer process must observe its own write.
func ensureRepoLevelTakeover(ctx context.Context, w io.Writer) {
	defer paths.ClearInvisibleRuntimeCache()
	if !globalTakeoverRelevant(ctx) {
		return
	}
	if err := strategy.EnsureEntireGitignore(ctx); err != nil {
		logging.Warn(ctx, "repo takeover: could not ensure .entire/.gitignore before migrating runtime data; runtime files will appear untracked in git status until .entire/.gitignore exists — create it or re-run enable", "error", err)
	}
	maybeMigrateGlobalRuntimeData(ctx, w)
	clearGlobalSetupMarker(ctx)
}

// globalTakeoverRelevant reports whether the global tier is or was relevant
// for this clone: the once-per-clone lazy-setup marker is set, or the current
// worktree's routed runtime dir is non-empty. This is the same trigger logic
// maybeMigrateGlobalRuntimeData applies.
func globalTakeoverRelevant(ctx context.Context) bool {
	if prefs, err := settings.LoadClonePreferences(ctx); err == nil && prefs.GlobalSetupCompleted {
		return true
	}
	source, _, err := invisibleRuntimeLocations(ctx)
	if err != nil {
		return false
	}
	entries, err := os.ReadDir(source)
	return err == nil && len(entries) > 0
}

// clearGlobalSetupMarker clears the clone's global_setup_completed marker so
// the lazy invisible setup re-runs the next time the global tier owns this
// clone. Read-before-write keeps the common case (marker absent) to one
// preferences read, without taking the preferences lock. Best-effort: a
// failure is logged, and the stale marker's only cost is that a later
// globally-tracked session skips hook reinstallation until doctor clears it.
func clearGlobalSetupMarker(ctx context.Context) {
	if prefs, err := settings.LoadClonePreferences(ctx); err != nil || !prefs.GlobalSetupCompleted {
		return
	}
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		p.GlobalSetupCompleted = false
		return nil
	}); err != nil {
		logging.Warn(ctx, "could not clear the global_setup_completed marker", "error", err)
	}
}

// invisibleRuntimeLocations resolves the current worktree's routed runtime
// dir (<git-common-dir>/entire/worktree/<worktree-key>) and the worktree
// root. session.GetGitCommonDir returns an absolute path (relative rev-parse
// output is absolutized against the cwd it was resolved in), satisfying
// InvisibleRuntimeDir's absolute-commonDir precondition.
func invisibleRuntimeLocations(ctx context.Context) (source, root string, err error) {
	root, err = paths.WorktreeRoot(ctx)
	if err != nil {
		return "", "", fmt.Errorf("resolve worktree root: %w", err)
	}
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", "", fmt.Errorf("resolve git common dir: %w", err)
	}
	source, err = paths.InvisibleRuntimeDir(commonDir, root)
	if err != nil {
		return "", "", fmt.Errorf("classify worktree: %w", err)
	}
	return source, root, nil
}

// maybeMigrateGlobalRuntimeData moves THIS worktree's invisible-routed
// runtime data (<git-common-dir>/entire/worktree/<worktree-key>/
// {metadata,logs,tmp} — the subtrees paths.InvisibleRuntimeSubdirs
// enumerates) into the worktree's .entire directory. Repo-level enable flows
// call it (via ensureRepoLevelTakeover) BEFORE writing .entire/settings.json:
// that write flips path routing to the worktree, which would otherwise strand
// the routed files in .git. Only the current worktree's namespace is touched
// — sibling worktrees of the same clone keep their own namespaces (and their
// in-flight sessions) untouched. It triggers when the clone preferences carry
// the global_setup_completed marker or when the routed directory is
// non-empty. Best-effort by contract — enable never aborts on migration
// failure; files that could not be moved stay in place for a later retry and
// the outcome is summarized in one line. When the routed location itself
// cannot be resolved while the marker says there may be data, the early
// return logs one Warn — the data stays under .git and the marker persists,
// so a later enable retries.
func maybeMigrateGlobalRuntimeData(ctx context.Context, w io.Writer) {
	markerSet := false
	if prefs, prefsErr := settings.LoadClonePreferences(ctx); prefsErr == nil && prefs.GlobalSetupCompleted {
		markerSet = true
	}
	source, root, err := invisibleRuntimeLocations(ctx)
	if err != nil {
		if markerSet {
			logging.Warn(ctx, "global runtime data migration skipped: routed location unresolved; any routed data stays under .git until a later enable retries", "error", err)
		}
		return
	}

	if !markerSet {
		entries, readErr := os.ReadDir(source)
		if readErr != nil || len(entries) == 0 {
			return
		}
	}

	var moved, skipped, failed int
	warnLog := &migrationWarnLog{}
	for _, sub := range paths.InvisibleRuntimeSubdirs() {
		m, s, f := moveDirContents(ctx, warnLog, filepath.Join(source, sub), filepath.Join(root, paths.EntireDir, sub))
		moved += m
		skipped += s
		failed += f
	}
	warnLog.flush(ctx)
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
	line := fmt.Sprintf("%sMoved %d globally-tracked file(s) into .entire/", prefix, moved)
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

// migrationWarnLimit caps the per-file Warn records one migration emits. The
// one-line summary carries the user-facing signal (counts + source path); the
// first few Warns carry the diagnosable which/why. Past the cap, a mass
// failure (say, a read-only .entire) would only repeat the same reason per
// file, so the remainder collapses into a single suppression Warn.
const migrationWarnLimit = 5

// migrationWarnLog is the capped Warn sink shared across one migration's
// recursive moveDirContents walk.
type migrationWarnLog struct {
	emitted    int
	suppressed int
}

func (l *migrationWarnLog) warn(ctx context.Context, msg string, args ...any) {
	if l.emitted >= migrationWarnLimit {
		l.suppressed++
		return
	}
	l.emitted++
	logging.Warn(ctx, msg, args...)
}

// flush emits the one collapsed record for warnings past the cap.
func (l *migrationWarnLog) flush(ctx context.Context) {
	if l.suppressed > 0 {
		logging.Warn(ctx, "global runtime data migration: further failures suppressed (counts in the summary line)", "count", l.suppressed)
	}
}

// moveDirContents moves every file under src into the same relative location
// under dst, creating directories as needed. Files already present in dst are
// skipped (left in src). Returns moved/skipped/failed counts; a missing src is
// (0, 0, 0), while any other ReadDir failure counts as one failure — an
// unreadable source is not "nothing to move", and the summary must not claim
// unqualified success over it. The summary line keeps the counts; individual
// failures are logged with their path and reason through the caller's capped
// warnLog, so "N could not be moved" is diagnosable from the log without a
// mass failure flooding it.
func moveDirContents(ctx context.Context, warnLog *migrationWarnLog, src, dst string) (moved, skipped, failed int) {
	entries, err := os.ReadDir(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, 0
		}
		warnLog.warn(ctx, "global runtime data migration: source directory unreadable", "path", src, "error", err)
		return 0, 0, 1
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			m, s, f := moveDirContents(ctx, warnLog, srcPath, dstPath)
			moved, skipped, failed = moved+m, skipped+s, failed+f
			continue
		}
		if _, statErr := os.Lstat(dstPath); statErr == nil {
			skipped++
			continue
		}
		//nolint:gosec // G301: runtime-data dirs match the .entire directory permissions
		if mkErr := os.MkdirAll(dst, 0o755); mkErr != nil {
			warnLog.warn(ctx, "global runtime data migration: cannot create destination directory", "path", dst, "error", mkErr)
			failed++
			continue
		}
		if renameErr := migrateRenameFile(srcPath, dstPath); renameErr != nil {
			// Rename fails with EXDEV when the git common dir and the
			// worktree live on different filesystems (--separate-git-dir,
			// linked worktree on another volume); fall back to copy+remove.
			copied, copyErr := copyFileThenRemove(srcPath, dstPath)
			switch {
			case copyErr == nil:
				// Fully moved via the fallback.
			case copied:
				// The copy landed in .entire; only removing the source
				// failed. The data IS where the user needs it, so count it
				// moved — claiming it "could not be moved" would be false —
				// and record the residue left under .git.
				warnLog.warn(ctx, "global runtime data migration: file copied but source residue remains", "path", srcPath, "error", copyErr)
			default:
				warnLog.warn(ctx, "global runtime data migration: file could not be moved", "path", srcPath, "rename_error", renameErr, "copy_error", copyErr)
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
// copied reports whether dst landed: (true, non-nil error) means the copy
// succeeded and only the source removal failed, leaving a residue at src.
func copyFileThenRemove(src, dst string) (copied bool, err error) {
	info, err := os.Lstat(src)
	if err != nil {
		return false, fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("not a regular file: %s", src)
	}
	in, err := os.Open(src) //nolint:gosec // path is derived from the git common dir, not user input
	if err != nil {
		return false, fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temp: %w", err)
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
		return false, fmt.Errorf("copy contents: %w", err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, fmt.Errorf("rename into place: %w", err)
	}
	removeTmp = false
	if err := os.Remove(src); err != nil {
		return true, fmt.Errorf("remove source: %w", err)
	}
	return true, nil
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
