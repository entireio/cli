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
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
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
	// This confirmation is the announcement: don't stack the detection warn.
	ackGlobalWarnMarker(ctx)
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
		res, err := ua.Support.InstallUserHooks(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(w, "  ! %s: install failed: %v\n", ua.Name, err)
			continue
		case res.Repaired:
			// The file was rewritten (partial/duplicate/alternate-form install
			// normalized) — "already installed" would claim nothing changed.
			fmt.Fprintf(w, "  ✓ %s: hooks repaired\n", ua.Name)
		case res.Installed == 0:
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
	fmt.Fprintln(w, "Locally captured checkpoints in untrusted repos will not sync.")
	// The held-data line above replaces the off-detection note.
	retireGlobalWarnMarker(ctx)
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
		// This confirmation is the announcement: don't stack the detection warn.
		ackGlobalWarnMarker(ctx)
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
// returns the answer. A package var (same seam pattern as migrateMoveFile)
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

// repoLevelTakeoverPlan captures the routing-independent locations needed to
// reconcile a globally tracked worktree after repo settings become
// authoritative. Capture happens before the settings write; reconciliation
// happens after it. sourceErr is retained so a marker-backed retry is not
// silently cleared when the old namespace cannot be resolved.
type repoLevelTakeoverPlan struct {
	source    string
	root      string
	markerSet bool
	relevant  bool
	sourceErr error
}

func planRepoLevelTakeover(ctx context.Context) repoLevelTakeoverPlan {
	plan := repoLevelTakeoverPlan{}
	if prefs, err := settings.LoadClonePreferences(ctx); err == nil && prefs.GlobalSetupCompleted {
		plan.markerSet = true
		plan.relevant = true
	}
	plan.source, plan.root, plan.sourceErr = invisibleRuntimeLocations(ctx)
	if plan.sourceErr != nil {
		return plan
	}
	if entries, err := os.ReadDir(plan.source); err == nil && len(entries) > 0 {
		plan.relevant = true
	}
	return plan
}

// reconcileRepoLevelTakeover performs best-effort cleanup only after the repo
// settings discriminator has been persisted. Activation is never rolled back
// and never waits behind a hook that began on the old route. The global setup
// marker is a retry marker: it is cleared only after the whole file + state
// reconciliation pass succeeds.
func reconcileRepoLevelTakeover(ctx context.Context, w io.Writer, plan repoLevelTakeoverPlan) {
	paths.ClearInvisibleRuntimeCache()
	if plan.sourceErr != nil {
		if plan.markerSet {
			logging.Warn(ctx, "global runtime data reconciliation deferred: routed location unresolved", "error", plan.sourceErr)
			fmt.Fprintln(w, "  Runtime data migration deferred: routed location could not be resolved — re-run 'entire enable'.")
		}
		return
	}
	if plan.root == "" || plan.source == "" {
		return
	}
	if !plan.relevant && !transcriptPathRepairRelevant(ctx, plan) {
		return
	}

	if plan.relevant {
		if err := strategy.EnsureEntireGitignore(ctx); err != nil {
			logging.Warn(ctx, "repo takeover: could not ensure .entire/.gitignore before migrating runtime data", "error", err)
			fmt.Fprintln(w, "  Runtime data reconciliation incomplete: could not ensure .entire/.gitignore — re-run 'entire enable'.")
			return
		}
	}

	release, lockErr := tryAcquireGlobalRuntimeMigrationGate(ctx, plan.root)
	if lockErr != nil {
		logging.Warn(ctx, "global runtime data migration deferred: worktree migration gate is busy", "error", lockErr)
		fmt.Fprintln(w, "  Runtime data migration deferred: an agent hook is still active — re-run 'entire enable'.")
		return
	}
	defer release()

	if n := activeSessionCountInWorktree(ctx, plan.root); n > 0 {
		logging.Warn(ctx, "global runtime data migration deferred: active agent session(s) in this worktree", "count", n)
		fmt.Fprintf(w, "  Runtime data migration deferred: %d agent session(s) active in this worktree — re-run 'entire enable' once they finish.\n", n)
		return
	}

	result := runtimeMigrationResult{}
	if plan.relevant {
		result = migrateGlobalRuntimeData(ctx, plan)
	}
	result.unresolved += repairMigratedTranscriptPaths(ctx, plan)

	if plan.relevant {
		// Always prune emptied subtrees, even when a conflicting file keeps a
		// different subtree retryable. Only treat a remaining root as an extra
		// cleanup failure when no earlier file failure explains its presence.
		removeEmptyDirTree(plan.source)
		if result.failed == 0 {
			if _, err := os.Stat(plan.source); err == nil || !errors.Is(err, fs.ErrNotExist) {
				result.failed++
				logging.Warn(ctx, "global runtime data migration: routed namespace cleanup incomplete", "path", plan.source, "error", err)
			}
		}
		_ = os.Remove(filepath.Dir(plan.source))
	}

	if result.failed > 0 || result.unresolved > 0 {
		if plan.relevant {
			printRuntimeMigrationSummary(w, plan.source, result)
		}
		if !plan.relevant {
			fmt.Fprintln(w, "  Runtime data reconciliation incomplete: a stored transcript path could not be repaired — re-run 'entire enable'.")
		}
		return
	}
	if !clearGlobalSetupMarker(ctx) {
		result.maintenanceFailed++
	}
	if plan.relevant {
		printRuntimeMigrationSummary(w, plan.source, result)
	}
	if result.maintenanceFailed > 0 && !plan.relevant {
		fmt.Fprintln(w, "  Runtime data reconciliation incomplete: the global setup marker could not be cleared — re-run 'entire enable'.")
	}
}

// clearGlobalSetupMarker clears the clone's global_setup_completed marker so
// the lazy invisible setup re-runs the next time the global tier owns this
// clone. Read-before-write keeps the common case (marker absent) to one
// preferences read, without taking the preferences lock. Best-effort: a
// failure is logged and reported to reconciliation so it cannot claim a fully
// successful pass. The stale marker then preserves retryability.
func clearGlobalSetupMarker(ctx context.Context) bool {
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		logging.Warn(ctx, "could not read clone preferences while clearing the global_setup_completed marker", "error", err)
		return false
	}
	if !prefs.GlobalSetupCompleted {
		return true
	}
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		p.GlobalSetupCompleted = false
		return nil
	}); err != nil {
		logging.Warn(ctx, "could not clear the global_setup_completed marker", "error", err)
		return false
	}
	return true
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

// maybeMigrateGlobalRuntimeData is the direct migration/recovery entry point
// retained for focused tests and internal callers. Repo activation paths must
// instead capture planRepoLevelTakeover before their settings write and call
// reconcileRepoLevelTakeover afterward.
func maybeMigrateGlobalRuntimeData(ctx context.Context, w io.Writer) {
	reconcileRepoLevelTakeover(ctx, w, planRepoLevelTakeover(ctx))
}

type runtimeMigrationResult struct {
	moved             int
	skipped           int
	failed            int
	unresolved        int
	maintenanceFailed int
}

// migrateGlobalRuntimeData moves THIS worktree's invisible-routed
// runtime data (<git-common-dir>/entire/worktree/<worktree-key>/
// {metadata,logs,tmp} — the subtrees paths.InvisibleRuntimeSubdirs
// enumerates) into the worktree's .entire directory. Repo-level enable flows
// call it only through post-activation reconciliation. Only the current
// worktree's namespace is touched
// — sibling worktrees of the same clone keep their own namespaces (and their
// in-flight sessions) untouched. It triggers when the clone preferences carry
// the global_setup_completed marker or when the routed directory is
// non-empty. Best-effort by contract — enable never aborts on migration
// failure; files that could not be moved stay in place for a later retry and
// the outcome is summarized in one line. When the routed location itself
// cannot be resolved while the marker says there may be data, the early
// return logs one Warn — the data stays under .git and the marker persists,
// so a later enable retries.
func migrateGlobalRuntimeData(ctx context.Context, plan repoLevelTakeoverPlan) runtimeMigrationResult {
	result := runtimeMigrationResult{}
	warnLog := &migrationWarnLog{}
	for _, sub := range paths.InvisibleRuntimeSubdirs() {
		// The migration's DESTINATION is deliberately the literal worktree
		// .entire dir, in both of its states: on a fresh enable this runs
		// before .entire/settings.json exists, when AbsPath still routes to
		// the git common dir — resolving through AbsPath would move files
		// onto themselves; on a takeover recovery routing already points at
		// the worktree, and the destination must be that same literal dir
		// regardless. Routing-independent by design.
		m, s, f := moveDirContents(ctx, warnLog, filepath.Join(plan.source, sub), filepath.Join(plan.root, paths.EntireDir, sub)) // entire-join-ok: migration destination, routing-independent by design
		result.moved += m
		result.skipped += s
		result.failed += f
	}
	warnLog.flush(ctx)
	return result
}

func printRuntimeMigrationSummary(w io.Writer, source string, result runtimeMigrationResult) {
	if result.moved+result.skipped+result.failed+result.unresolved+result.maintenanceFailed == 0 {
		return
	}
	// ✓ elsewhere in the enable output means unqualified success, so drop it
	// when anything failed, and name the source so leftovers are findable.
	prefix := "  ✓ "
	if result.failed > 0 || result.unresolved > 0 || result.maintenanceFailed > 0 {
		prefix = "  "
	}
	line := fmt.Sprintf("%sMoved %d globally-tracked file(s) into .entire/", prefix, result.moved)
	if result.skipped > 0 {
		line += fmt.Sprintf(", %d already present", result.skipped)
	}
	if result.failed > 0 {
		line += fmt.Sprintf(", %d could not be moved (left in %s)", result.failed, source)
	}
	if result.unresolved > 0 {
		line += fmt.Sprintf(", %d transcript path(s) could not be repaired", result.unresolved)
	}
	if result.maintenanceFailed > 0 {
		line += ", global setup marker could not be cleared"
	}
	fmt.Fprintln(w, line)
}

func repairMigratedTranscriptPaths(ctx context.Context, plan repoLevelTakeoverPlan) int {
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		logging.Warn(ctx, "could not list session states for runtime path repair", "error", err)
		return 1
	}
	unresolved := 0
	for _, state := range states {
		if state.WorktreePath != plan.root || state.Phase.IsActive() || state.TranscriptPath == "" {
			continue
		}
		newPath, belongs, targetExists := migratedRuntimePathCandidate(plan, state.TranscriptPath)
		if !belongs {
			continue
		}
		if !targetExists {
			unresolved++
			logging.Warn(ctx, "migrated transcript path destination is missing", "session_id", state.SessionID, "old_path", state.TranscriptPath, "new_path", newPath)
			continue
		}
		didRepair := false
		err := strategy.MutateSessionState(ctx, state.SessionID, func(current *strategy.SessionState) error {
			if current.WorktreePath != plan.root || current.Phase.IsActive() || current.TranscriptPath != state.TranscriptPath {
				return strategy.ErrMutationSkip
			}
			current.TranscriptPath = newPath
			didRepair = true
			return nil
		})
		if err != nil || !didRepair {
			unresolved++
			logging.Warn(ctx, "could not repair migrated transcript path", "session_id", state.SessionID, "error", err)
		}
	}
	return unresolved
}

func transcriptPathRepairRelevant(ctx context.Context, plan repoLevelTakeoverPlan) bool {
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		// Explicit enable is a recovery point. If state cannot be inspected, run
		// reconciliation so the failure is retained and reported, not mistaken
		// for proof that there is nothing to repair.
		return true
	}
	for _, state := range states {
		if state.WorktreePath != plan.root || state.TranscriptPath == "" {
			continue
		}
		if _, belongs, _ := migratedRuntimePathCandidate(plan, state.TranscriptPath); belongs {
			return true
		}
	}
	return false
}

func migratedRuntimePath(plan repoLevelTakeoverPlan, oldPath string) (string, bool) {
	newPath, belongs, exists := migratedRuntimePathCandidate(plan, oldPath)
	return newPath, belongs && exists
}

func migratedRuntimePathCandidate(plan repoLevelTakeoverPlan, oldPath string) (newPath string, belongs, targetExists bool) {
	if !filepath.IsAbs(oldPath) || plan.source == "" || plan.root == "" {
		return "", false, false
	}
	rel, err := filepath.Rel(filepath.Clean(plan.source), filepath.Clean(oldPath))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	runtimeSubdir := false
	for _, subdir := range paths.InvisibleRuntimeSubdirs() {
		if first == subdir {
			runtimeSubdir = true
			break
		}
	}
	if !runtimeSubdir {
		return "", false, false
	}
	newPath = filepath.Join(plan.root, paths.EntireDir, rel) // entire-join-ok: migration destination, routing-independent by design
	// A retained source means migration did not establish that the destination
	// is its replacement (most commonly the no-overwrite conflict case). Never
	// repoint state at merely-existing, potentially divergent destination data.
	if _, sourceErr := os.Stat(filepath.Clean(oldPath)); sourceErr == nil {
		return newPath, true, false
	} else if !errors.Is(sourceErr, fs.ErrNotExist) {
		return newPath, true, false
	}
	_, statErr := os.Stat(newPath)
	return newPath, true, statErr == nil
}

func tryAcquireGlobalRuntimeMigrationGate(ctx context.Context, root string) (func(), error) {
	tryCtx, cancel := context.WithDeadline(ctx, time.Now())
	defer cancel()
	return acquireGlobalRuntimeMigrationGate(tryCtx, root)
}

// acquireGlobalRuntimeMigrationGate serializes lifecycle hooks with takeover
// migration for one worktree. The lock file is a sibling of the routed
// namespace rather than a child, so successful cleanup can remove the runtime
// tree without unlinking the inode that carries the lock.
func acquireGlobalRuntimeMigrationGate(ctx context.Context, root string) (func(), error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve git common dir: %w", err)
	}
	runtimeDir, err := paths.InvisibleRuntimeDir(commonDir, root)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree runtime namespace: %w", err)
	}
	lockPath := runtimeDir + ".migration.lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return nil, fmt.Errorf("create migration lock directory: %w", err)
	}
	release, err := flock.AcquireContext(ctx, lockPath)
	if err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	return release, nil
}

// activeSessionCountInWorktree reports how many sessions are in an active
// phase in the given worktree (WorktreePath compared exactly, the same idiom
// as strategy's worktree scoping). Best-effort: a listing failure reports
// zero after a Warn, so the migration proceeds exactly as it would have
// before the guard existed.
func activeSessionCountInWorktree(ctx context.Context, root string) int {
	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		logging.Warn(ctx, "could not check for active sessions before runtime data migration", "error", err)
		return 0
	}
	n := 0
	for _, s := range states {
		if s.Phase.IsActive() && s.WorktreePath == root {
			n++
		}
	}
	return n
}

// migrateMoveFile and migrateLinkFile are seams over the no-replace move and
// its same-filesystem fast path. Tests use them to open the live-writer race
// and force the cross-device fallback deterministically.
var (
	migrateMoveFile = moveFileNoReplace
	migrateLinkFile = os.Link
)

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
// under dst, creating directories as needed. A file already present in dst is
// skipped only when its content is identical to the source (which is then
// removed as a duplicate); a divergent or uncomparable destination counts the
// source as failed, left in src. Returns moved/skipped/failed counts; a missing src is
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
			// A file already at dst: only identical content lets the source
			// copy be dropped. A blind skip would silently strand a DIVERGENT
			// source under .git forever — dst may have been written by a
			// post-flip session between two migration attempts — so divergent
			// or uncomparable pairs count as failed, putting them in the
			// summary's "left in <source>" bucket where they stay findable.
			same, cmpErr := filesIdentical(srcPath, dstPath)
			switch {
			case cmpErr != nil:
				warnLog.warn(ctx, "global runtime data migration: destination exists but could not be compared with source", "path", srcPath, "error", cmpErr)
				failed++
			case !same:
				warnLog.warn(ctx, "global runtime data migration: destination already exists with different content; source left in place", "path", srcPath)
				failed++
			default:
				if rmErr := os.Remove(srcPath); rmErr != nil {
					warnLog.warn(ctx, "global runtime data migration: identical duplicate source could not be removed", "path", srcPath, "error", rmErr)
					failed++
				} else {
					skipped++
				}
			}
			continue
		}
		//nolint:gosec // G301: runtime-data dirs match the .entire directory permissions
		if mkErr := os.MkdirAll(dst, 0o755); mkErr != nil {
			warnLog.warn(ctx, "global runtime data migration: cannot create destination directory", "path", dst, "error", mkErr)
			failed++
			continue
		}
		copied, moveErr := migrateMoveFile(srcPath, dstPath)
		if moveErr != nil {
			if copied {
				// The destination landed; only removing the source failed.
				warnLog.warn(ctx, "global runtime data migration: file copied but source residue remains", "path", srcPath, "error", moveErr)
			} else {
				warnLog.warn(ctx, "global runtime data migration: file could not be moved", "path", srcPath, "error", moveErr)
				failed++
				continue
			}
		}
		moved++
	}
	return moved, skipped, failed
}

// filesIdentical reports whether two files have identical contents, comparing
// sizes first so distinct files rarely cost a read.
func filesIdentical(pathA, pathB string) (bool, error) {
	infoA, err := os.Stat(pathA)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", pathA, err)
	}
	infoB, err := os.Stat(pathB)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", pathB, err)
	}
	if !infoA.Mode().IsRegular() || !infoB.Mode().IsRegular() || infoA.Size() != infoB.Size() {
		return false, nil
	}
	sumA, err := fileSHA256(pathA)
	if err != nil {
		return false, err
	}
	sumB, err := fileSHA256(pathB)
	if err != nil {
		return false, err
	}
	return sumA == sumB, nil
}

// moveFileNoReplace atomically publishes src at dst only when dst is absent.
// A hard-link fast path preserves the file without copying on one filesystem;
// filesystems that cannot link src to dst fall back to a destination-side
// temp copy. Both publication paths reject an existing destination.
func moveFileNoReplace(src, dst string) (copied bool, err error) {
	info, err := os.Lstat(src)
	if err != nil {
		return false, fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("not a regular file: %s", src)
	}
	if err := migrateLinkFile(src, dst); err == nil {
		if err := os.Remove(src); err != nil {
			return true, fmt.Errorf("remove source: %w", err)
		}
		return true, nil
	} else if errors.Is(err, fs.ErrExist) {
		return false, fmt.Errorf("publish destination: %w", err)
	}
	return copyFileThenRemove(src, dst)
}

// copyFileThenRemove copies src to a temp file in dst's directory, then
// hard-links that complete temp file into place without replacement and
// removes src on success. A partial copy is never visible at dst.
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
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("copy contents: %w", err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	// fsync before close: the source is removed after the rename, so a crash
	// surfacing the rename with the bytes still in cache would lose the file
	// outright (same rationale as jsonutil.WriteFileAtomic).
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Link(tmpName, dst); err != nil {
		return false, fmt.Errorf("publish destination: %w", err)
	}
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
