package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Caps how many worktrees prepare-commit-msg will resolve via git when hunting
// auto-adopt candidates (registry entries or sibling dirs).
const (
	maxSiblingAutoAdoptScan  = 32
	maxRegistryAutoAdoptScan = 32
)

// Time bounds for prepare-commit-msg auto-adopt. Git rev-parse calls use
// CommandContext; without a deadline a hung/stale worktree path (registry or
// sibling) could block `git commit` indefinitely.
const (
	autoAdoptDiscoveryTimeout   = 2 * time.Second
	autoAdoptGitResolveTimeout  = 500 * time.Millisecond
	autoAdoptStagedFilesTimeout = 1 * time.Second
	// autoAdoptAdoptTimeout bounds adoptFromExternalSessionStore, which takes
	// a cross-process session-state flock (strategy.WithSessionStateLocks).
	// Without a deadline, a lock held by another process (or a hung
	// concurrent hook) could block git commit indefinitely — the same
	// failure mode the timeouts above already guard against for discovery.
	autoAdoptAdoptTimeout = 2 * time.Second
	// autoAdoptOverallBudget caps the whole prepare-commit-msg attempt. The
	// per-step timeouts above are worst-case and stack (target resolve + staged +
	// registry + sibling + adopt ≈ 7.5s); wrapping the attempt in one budget
	// bounds their sum so an ordinary human commit (the miss path) can add at
	// most this much latency to git commit.
	autoAdoptOverallBudget = 5 * time.Second
)

// prepareCommitMsgSourceAmend is git's prepare-commit-msg source for `git commit --amend`.
const prepareCommitMsgSourceAmend = "commit"

// shouldTryAutoAdoptOnPrepareCommitMsg reports whether prepare-commit-msg should
// attempt cross-common-dir auto-adopt. Matches ManualCommitStrategy.PrepareCommitMsg
// skip conditions so we never retire a live session when no trailer would be written.
// Amend (source "commit"): handleAmendCommitMsg only restores an existing trailer /
// HEAD LastCheckpointID match and will not write a trailer for a freshly adopted session.
func shouldTryAutoAdoptOnPrepareCommitMsg(ctx context.Context, source string) bool {
	// Amend is auto-adopt's own extra guard: handleAmendCommitMsg only restores
	// an existing trailer and never writes one for a freshly adopted session.
	if source == prepareCommitMsgSourceAmend {
		return false
	}
	// Merge/squash and git sequence operations are the shared skip invariant.
	return !strategy.SkipsPrepareCommitMsg(ctx, source)
}

// tryAutoAdoptCrossCommonDirSession adopts a unique ACTIVE session from another
// git common dir into the current worktree when it is safe to do so.
//
// Discovery order:
//  1. Live-session registry (written on ACTIVE StateStore.Save) — reaches any
//     worktree, including non-siblings under unrelated parents (issue #1439's own
//     repro), guarded by owner + distinctive-overlap + uniqueness instead of proximity
//  2. Immediate sibling repos under the parent directory (seeded / microservices)
//
// A candidate is accepted only when exactly one match remains after filtering
// for recent ACTIVE sessions that (a) share the committing process owner and
// (b) have FilesTouched overlapping the staged commit paths on a
// non-boilerplate relative path (README.md / go.mod / package.json / … alone
// never count). The sibling scan is inherently parent-scoped; the registry path
// is not, relying on the stronger owner + distinctive-overlap + uniqueness
// guards. Idle sessions are never auto-adopted. Ambiguity skips.
//
// Best-effort: never returns an error to the git hook caller.
func tryAutoAdoptCrossCommonDirSession(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "session")

	// This runs inside prepare-commit-msg, whose stderr the hook swallows. A
	// panic anywhere in the cross-common-dir adopt path would otherwise vanish
	// with no record; recover and log it at Error so it is at least visible in
	// .entire/logs. Never re-panic — a failed adopt must not break `git commit`.
	defer func() {
		if r := recover(); r != nil {
			logging.Error(logCtx, "auto-adopt: recovered from panic",
				slog.Any("panic", r),
			)
		}
	}()

	// One overall wall-clock budget for the whole attempt. Every step below
	// derives its own timeout from this context, so their worst-case sum can
	// never exceed autoAdoptOverallBudget — a miss adds bounded latency to commit.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptOverallBudget)
	defer cancel()

	if !targetIsEntireEnabled(ctx) {
		return
	}

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return
	}
	targetCtx, targetCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
	targetStore, _, targetCommonDir, err := stateStoreForWorktree(targetCtx, targetWorktree)
	targetCancel()
	if err != nil {
		return
	}
	if hasLocalActiveSession(ctx, targetStore) {
		return
	}

	stagedCtx, stagedCancel := context.WithTimeout(ctx, autoAdoptStagedFilesTimeout)
	staged, err := stagedFilesForAutoAdopt(stagedCtx, targetWorktree)
	stagedCancel()
	if err != nil {
		staged = nil
	}
	// Owner + staged overlap are both required for every candidate. Bail before
	// any registry/sibling I/O when either is missing (ordinary human commits).
	if len(staged) == 0 {
		return
	}
	owner, hasOwner := proclive.ResolveOwner()
	if !hasOwner {
		return
	}

	regCtx, regCancel := context.WithTimeout(ctx, autoAdoptDiscoveryTimeout)
	registryCandidates := collectRegistryAutoAdoptCandidates(regCtx, targetCommonDir, staged, owner, hasOwner)
	regCancel()
	scanCtx, scanCancel := context.WithTimeout(ctx, autoAdoptDiscoveryTimeout)
	siblingCandidates := collectSiblingAutoAdoptCandidates(scanCtx, targetWorktree, targetCommonDir, staged, owner, hasOwner)
	scanCancel()
	// Union both discovery sources (deduped by session ID) BEFORE the uniqueness
	// test. Gating the sibling scan on an empty registry would let a single
	// registry hit bypass the ambiguity guard when a second, distinct candidate
	// exists on disk — auto-adopt would then silently steal one of two sessions.
	candidates := unionAutoAdoptCandidates(registryCandidates, siblingCandidates)
	if len(candidates) != 1 {
		if len(candidates) > 1 {
			logging.Debug(logCtx, "auto-adopt: skipped ambiguous cross-common-dir sessions",
				slog.Int("candidates", len(candidates)),
			)
		}
		return
	}

	source := candidates[0]
	existing, err := targetStore.Load(ctx, source.SessionID)
	if err != nil || existing != nil {
		return
	}

	logging.Info(logCtx, "auto-adopt: adopting cross-common-dir session for commit",
		slog.String("session_id", source.SessionID),
		slog.String("from_worktree", source.WorktreePath),
		slog.String("to_worktree", targetWorktree),
	)

	adoptCtx, adoptCancel := context.WithTimeout(ctx, autoAdoptAdoptTimeout)
	_, _, adoptErr := adoptFromExternalSessionStore(
		adoptCtx,
		source.Store,
		source.WorktreePath,
		source.CommonDir,
		targetStore,
		targetCommonDir,
		source.SessionID,
		// DeferSourceRetire: this runs in prepare-commit-msg, before the commit
		// exists. Register the adopted session here (so the trailer lands) but
		// defer the destructive source retire to post-commit, where the commit is
		// a fact — an aborted commit must not tombstone the source.
		adoptOptions{Force: true, SkipTranscriptValidation: true, DeferSourceRetire: true},
	)
	adoptCancel()
	if adoptErr != nil {
		var rollbackFailed *adoptRollbackFailedError
		if errors.As(adoptErr, &rollbackFailed) {
			// Retire failed AND rollback failed: the session is now registered in
			// both the source and target repos. This is corruption, not a miss.
			logging.Error(logCtx, "auto-adopt: adopt left session registered in both repos",
				slog.String("session_id", source.SessionID),
				slog.String("error", adoptErr.Error()),
			)
		} else {
			logging.Warn(logCtx, "auto-adopt: adopt failed",
				slog.String("session_id", source.SessionID),
				slog.String("error", adoptErr.Error()),
			)
		}
	}
}

// finalizePendingSourceRetires completes cross-common-dir auto-adopts that were
// deferred at prepare-commit-msg time. For each session in the current
// (committing) worktree carrying a PendingSourceRetire marker, it tombstones the
// source session — now that the commit is a fact — and clears the marker. Runs
// from the post-commit git hook, after strategy.PostCommit. Best-effort: it
// never returns an error to the hook caller, and is panic-guarded so a fault in
// the retire path can never break `git commit`.
//
// An aborted commit (editor abort, empty-message strip) never reaches
// post-commit, so this never runs for it — leaving the source ACTIVE, which is
// the whole point of splitting the adopt across the two hooks.
func finalizePendingSourceRetires(ctx context.Context) {
	logCtx := logging.WithComponent(ctx, "session")

	defer func() {
		if r := recover(); r != nil {
			logging.Error(logCtx, "auto-adopt finalize: recovered from panic",
				slog.Any("panic", r),
			)
		}
	}()

	// Bound the whole finalize the same way the prepare-side attempt is bounded:
	// each pending retire takes a cross-process source flock, and a lock held by
	// a hung process must not block git's post-commit indefinitely.
	ctx, cancel := context.WithTimeout(ctx, autoAdoptOverallBudget)
	defer cancel()

	targetWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil || targetWorktree == "" {
		return
	}
	targetCtx, targetCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
	targetStore, _, targetCommonDir, err := stateStoreForWorktree(targetCtx, targetWorktree)
	targetCancel()
	if err != nil {
		return
	}

	states, err := targetStore.List(ctx)
	if err != nil {
		return
	}
	for _, state := range states {
		if ctx.Err() != nil {
			return
		}
		if state.PendingSourceRetire == nil {
			continue
		}
		retirePendingSource(ctx, targetStore, targetCommonDir, state)
	}
}

// retirePendingSource tombstones the source session recorded on adopted's
// PendingSourceRetire marker and clears the marker on the target. Idempotent:
// if the source is already gone or already retired it just clears the marker, so
// a crash between the source retire and the marker clear self-heals on the next
// post-commit.
func retirePendingSource(ctx context.Context, targetStore *session.StateStore, targetCommonDir string, adopted *session.State) {
	logCtx := logging.WithComponent(ctx, "session")
	marker := adopted.PendingSourceRetire
	if marker == nil || marker.SourceCommonDir == "" {
		return
	}
	sourceStore := session.NewStateStoreWithDir(filepath.Join(marker.SourceCommonDir, session.SessionStateDirName))

	retireCtx, cancel := context.WithTimeout(ctx, autoAdoptAdoptTimeout)
	defer cancel()

	err := strategy.WithSessionStateLocks(retireCtx, adopted.SessionID, []string{marker.SourceCommonDir, targetCommonDir}, func() error {
		// Reload the target under the lock so clearing the marker never clobbers a
		// concurrent write to the adopted session state.
		target, err := targetStore.Load(retireCtx, adopted.SessionID)
		if err != nil {
			return fmt.Errorf("reload adopted session state: %w", err)
		}
		if target == nil || target.PendingSourceRetire == nil {
			return nil // already finalized by a concurrent hook
		}
		sourceState, err := sourceStore.Load(retireCtx, adopted.SessionID)
		if err != nil {
			return fmt.Errorf("load source session state: %w", err)
		}
		if sourceState != nil && isAdoptableSourceSession(sourceState) {
			retired := retireAdoptedSourceSession(sourceState, target)
			if err := sourceStore.Save(retireCtx, &retired); err != nil {
				return fmt.Errorf("retire source session state: %w", err)
			}
		}
		target.PendingSourceRetire = nil
		if err := targetStore.Save(retireCtx, target); err != nil {
			return fmt.Errorf("clear pending source retire marker: %w", err)
		}
		return nil
	})
	if err != nil {
		logging.Warn(logCtx, "auto-adopt finalize: failed to retire source session",
			slog.String("session_id", adopted.SessionID),
			slog.String("error", err.Error()),
		)
		return
	}
	logging.Info(logCtx, "auto-adopt finalize: retired source session after commit",
		slog.String("session_id", adopted.SessionID),
		slog.String("source_common_dir", marker.SourceCommonDir),
	)
}

type autoAdoptCandidate struct {
	SessionID    string
	WorktreePath string
	CommonDir    string
	Store        *session.StateStore
}

// unionAutoAdoptCandidates concatenates candidate sets, deduping by session ID
// (session IDs are globally unique, so the same session found by both the
// registry and the sibling scan collapses to one). The union feeds the
// uniqueness test so a candidate present in only one source still counts.
func unionAutoAdoptCandidates(sets ...[]autoAdoptCandidate) []autoAdoptCandidate {
	seen := make(map[string]struct{})
	var out []autoAdoptCandidate
	for _, set := range sets {
		for _, cand := range set {
			if _, ok := seen[cand.SessionID]; ok {
				continue
			}
			seen[cand.SessionID] = struct{}{}
			out = append(out, cand)
		}
	}
	return out
}

func hasLocalActiveSession(ctx context.Context, store *session.StateStore) bool {
	states, err := store.List(ctx)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"),
			"auto-adopt: local session list failed; failing closed",
			slog.String("error", err.Error()),
		)
		return true // fail closed: do not steal when local store is unreadable
	}
	for _, state := range states {
		// Bound by the same recency window as the candidate guards. Without it a
		// single months-old Idle state file would count as a local session and
		// permanently disable cross-common-dir auto-adopt for the whole repo.
		if isRecentAdoptCandidate(state) {
			return true
		}
	}
	return false
}

func collectRegistryAutoAdoptCandidates(
	ctx context.Context,
	targetCommonDir string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) []autoAdoptCandidate {
	entries, err := session.ListLiveSessions()
	if err != nil || len(entries) == 0 {
		return nil
	}

	var out []autoAdoptCandidate
	resolved := 0
	for _, entry := range entries {
		if ctx.Err() != nil || resolved >= maxRegistryAutoAdoptScan {
			break
		}
		if sameAdoptStore(entry.CommonDir, targetCommonDir) {
			continue
		}
		if !isRecentLiveEntry(entry) {
			continue
		}
		// The registry path deliberately does NOT gate on parent-dir proximity
		// (unlike the sibling scan, which is inherently parent-scoped). Issue
		// #1439's own repro is cross-parent — a session in
		// …/entire.io/.worktrees/… committing in an unrelated /private/tmp/…
		// checkout — so requiring siblinghood here would leave the motivating case
		// with no trailer. A registry candidate's stronger guards stand in for
		// proximity: candidateFromSource → candidateFromLoaded requires an exact
		// process-owner match AND a non-boilerplate distinctive FilesTouched∩staged
		// overlap, and the caller's global exactly-one-candidate uniqueness test
		// still applies. A shared owner (tmux/IDE) plus a common relative filename
		// alone therefore cannot steal across unrelated repos.
		resolved++
		resolveCtx, resolveCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
		cand, ok := candidateFromSource(resolveCtx, entry.WorktreePath, entry.SessionID, staged, owner, hasOwner)
		resolveCancel()
		if ok {
			out = append(out, cand)
		}
	}
	return out
}

func collectSiblingAutoAdoptCandidates(
	ctx context.Context,
	targetWorktree, targetCommonDir string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) []autoAdoptCandidate {
	parent := filepath.Dir(targetWorktree)
	if parent == "" || parent == targetWorktree {
		return nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}

	var out []autoAdoptCandidate
	scanned := 0
	for _, entry := range entries {
		if ctx.Err() != nil || scanned >= maxSiblingAutoAdoptScan {
			break
		}
		if !entry.IsDir() {
			continue
		}
		sibling := filepath.Join(parent, entry.Name())
		if sameAdoptPath(sibling, targetWorktree) {
			continue
		}
		// Cheap pre-filters before shelling out to git rev-parse:
		//  1. .git entry (skip node_modules / build outputs)
		//  2. .entire/ (skip git repos that don't use Entire — no sessions to adopt)
		if !siblingLooksLikeGitWorktree(sibling) || !siblingLooksLikeEntireRepo(sibling) {
			continue
		}
		scanned++

		resolveCtx, resolveCancel := context.WithTimeout(ctx, autoAdoptGitResolveTimeout)
		store, worktree, commonDir, err := stateStoreForWorktree(resolveCtx, sibling)
		resolveCancel()
		if err != nil || sameAdoptStore(commonDir, targetCommonDir) {
			continue
		}
		states, err := store.List(ctx)
		if err != nil {
			continue
		}
		for _, state := range states {
			if !isRecentAdoptCandidate(state) {
				continue
			}
			cand, ok := candidateFromLoaded(store, worktree, commonDir, state, staged, owner, hasOwner)
			if ok {
				out = append(out, cand)
			}
		}
	}
	return out
}

func candidateFromSource(
	ctx context.Context,
	sourceWorktree, sessionID string,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) (autoAdoptCandidate, bool) {
	store, worktree, commonDir, err := stateStoreForWorktree(ctx, sourceWorktree)
	if err != nil {
		return autoAdoptCandidate{}, false
	}
	state, err := store.Load(ctx, sessionID)
	if err != nil || state == nil {
		return autoAdoptCandidate{}, false
	}
	return candidateFromLoaded(store, worktree, commonDir, state, staged, owner, hasOwner)
}

func candidateFromLoaded(
	store *session.StateStore,
	worktree, commonDir string,
	state *session.State,
	staged []string,
	owner proclive.Identity,
	hasOwner bool,
) (autoAdoptCandidate, bool) {
	if !isRecentAdoptCandidate(state) {
		return autoAdoptCandidate{}, false
	}
	// Auto-adopt is ACTIVE-only (matches the live-registry prefilter; see
	// docs/architecture/sessions-and-checkpoints.md, "Automatic cross-common-dir
	// adoption").
	// Idle is adoptable manually but must not be silently relocated on a commit hook.
	if state.Phase != session.PhaseActive {
		return autoAdoptCandidate{}, false
	}
	// Reject stale WorktreePath mismatches. Overriding worktree with
	// state.WorktreePath would make sessionBelongsToSourceWorktree a tautology
	// (comparing the recorded path against itself) and miss the ownership check.
	if state.WorktreePath != "" && !sameAdoptPath(state.WorktreePath, worktree) {
		return autoAdoptCandidate{}, false
	}
	// Both owner match and distinctive FilesTouched overlap are required.
	// Boilerplate-only overlap (README.md, go.mod, …) is ignored even when
	// the owner matches — those names collide across unrelated siblings.
	overlapMatch := filesTouchedOverlap(state.FilesTouched, staged)
	if !overlapMatch {
		return autoAdoptCandidate{}, false
	}
	ownerMatch := hasOwner && ownerMatches(state.Owner, owner)
	if !ownerMatch {
		return autoAdoptCandidate{}, false
	}
	return autoAdoptCandidate{
		SessionID:    state.SessionID,
		WorktreePath: worktree,
		CommonDir:    commonDir,
		Store:        store,
	}, true
}

func isRecentLiveEntry(entry session.LiveSessionEntry) bool {
	if entry.Phase != session.PhaseActive {
		return false
	}
	if entry.LastInteractionTime == nil {
		return false
	}
	// LiveSessionMaxAge matches adoptRecentWindow; registry TTL sweep uses the same bound.
	return time.Since(*entry.LastInteractionTime) <= session.LiveSessionMaxAge
}

// siblingLooksLikeGitWorktree reports whether dir has a .git entry (directory,
// gitfile, or symlink) so we can skip non-repos before spawning git.
func siblingLooksLikeGitWorktree(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// siblingLooksLikeEntireRepo reports whether dir has a .entire/ directory
// (written by `entire enable`). Non-Entire siblings have nothing to adopt.
func siblingLooksLikeEntireRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".entire"))
	return err == nil
}

func ownerMatches(recorded *proclive.Identity, current proclive.Identity) bool {
	if recorded == nil || recorded.PID == 0 || current.PID == 0 {
		return false
	}
	if recorded.PID != current.PID || recorded.Start != current.Start {
		return false
	}
	// Boot mismatch means a reboot; Start is only unique within a single boot
	// (same guard as proclive.Check).
	if recorded.Boot != "" && current.Boot != "" && recorded.Boot != current.Boot {
		return false
	}
	if recorded.Host != "" && current.Host != "" && recorded.Host != current.Host {
		return false
	}
	return true
}

// autoAdoptBoilerplateBasenames are relative-path basenames that commonly exist
// in every sibling repo. A match on these alone is not evidence the agent was
// working on the commit's real change set.
var autoAdoptBoilerplateBasenames = map[string]struct{}{
	"readme":              {},
	"readme.md":           {},
	"readme.rst":          {},
	"readme.txt":          {},
	"license":             {},
	"license.md":          {},
	"copying":             {},
	"changelog.md":        {},
	"contributing.md":     {},
	"codeowners":          {},
	".gitignore":          {},
	".gitattributes":      {},
	".editorconfig":       {},
	".env":                {},
	".env.example":        {},
	".nvmrc":              {},
	".node-version":       {},
	"go.mod":              {},
	"go.sum":              {},
	"package.json":        {},
	"package-lock.json":   {},
	"yarn.lock":           {},
	"pnpm-lock.yaml":      {},
	"cargo.toml":          {},
	"cargo.lock":          {},
	"gemfile":             {},
	"gemfile.lock":        {},
	"makefile":            {},
	"dockerfile":          {},
	"docker-compose.yml":  {},
	"docker-compose.yaml": {},
	"tsconfig.json":       {},
	"jsconfig.json":       {},
	"pyproject.toml":      {},
	"requirements.txt":    {},
	"setup.py":            {},
	"setup.cfg":           {},
	"pipfile":             {},
	"pipfile.lock":        {},
	"composer.json":       {},
	"composer.lock":       {},
}

func filesTouchedOverlap(touched, staged []string) bool {
	if len(touched) == 0 || len(staged) == 0 {
		return false
	}
	stagedSet := make(map[string]struct{}, len(staged))
	for _, f := range staged {
		stagedSet[filepath.ToSlash(filepath.Clean(f))] = struct{}{}
	}
	for _, f := range touched {
		key := filepath.ToSlash(filepath.Clean(f))
		if _, ok := stagedSet[key]; !ok {
			continue
		}
		if isAutoAdoptBoilerplatePath(key) {
			continue
		}
		return true
	}
	return false
}

func isAutoAdoptBoilerplatePath(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	_, ok := autoAdoptBoilerplateBasenames[base]
	return ok
}

func stagedFilesForAutoAdopt(ctx context.Context, repoRoot string) ([]string, error) {
	// -z emits NUL-separated, unquoted paths. Without it git C-quotes non-ASCII
	// names (core.quotepath defaults on, e.g. "caf\303\251.txt") which would
	// never match the UTF-8 paths recorded in FilesTouched, so an agent working
	// on a non-ASCII/spaced file would silently miss the overlap check.
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-only", "-z")
	cmd.Dir = repoRoot
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --cached --name-only -z: %w", err)
	}
	var staged []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			staged = append(staged, filepath.ToSlash(name))
		}
	}
	return staged, nil
}

func targetIsEntireEnabled(ctx context.Context) bool {
	stngs, err := settings.Load(ctx)
	if err != nil || stngs == nil {
		return false
	}
	return stngs.Enabled
}
