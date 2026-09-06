package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// Shadow strategy session state methods.
// Uses session.StateStore for persistence.

// loadSessionState loads session state using the StateStore.
func (s *ManualCommitStrategy) loadSessionState(ctx context.Context, sessionID string) (*SessionState, error) {
	store, err := s.getStateStore(ctx)
	if err != nil {
		return nil, err
	}
	state, err := store.Load(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session state: %w", err)
	}
	return state, nil
}

// saveSessionState saves session state using the StateStore.
func (s *ManualCommitStrategy) saveSessionState(ctx context.Context, state *SessionState) error {
	store, err := s.getStateStore(ctx)
	if err != nil {
		return err
	}
	if err := store.Save(ctx, state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}
	return nil
}

// clearSessionStateLocked clears session state using the StateStore. Callers
// must already hold sessionID's gate -- either by having called
// acquireSessionGate themselves, or by running inside a
// MutateSessionState/MutateSessionStateOnSaved closure for the same session
// (see CondenseSessionByID's clearAfter path, which clears while still
// inside its own locked mutation rather than after releasing it). Calling
// this without the gate held reintroduces the race clearSessionState exists
// to close: a concurrent, properly-locked write landing in the gap between
// a caller's "safe to clear" decision and the actual delete would be
// silently destroyed.
func (s *ManualCommitStrategy) clearSessionStateLocked(ctx context.Context, sessionID string) error {
	store, err := s.getStateStore(ctx)
	if err != nil {
		return err
	}
	if err := store.Clear(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to clear session state: %w", err)
	}
	return nil
}

// clearSessionState clears session state using the StateStore, under
// sessionID's gate -- the same per-session lock MutateSessionState uses for
// every other mutation of this state. Without it, a concurrently-running,
// properly-locked write (e.g. a PostToolUse hook for the same session)
// landing in the gap between a caller's decision to clear and this call
// actually clearing would be silently destroyed: the file the write just
// produced gets deleted out from under it, with nothing surfacing the loss.
// initializeSession documents and fixes the identical hazard class
// elsewhere in this file ("take the gate, then re-check under lock");
// this closes the same gap for the clear path.
func (s *ManualCommitStrategy) clearSessionState(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return ErrStateNotFound
	}
	_, isOuter, release, err := acquireSessionGate(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()
	if !isOuter {
		// A MutateSessionState frame for this session is already active on
		// this goroutine. Reaching clearSessionState reentrantly from
		// inside one is not something any current caller does (they clear
		// only after their own mutation closure has already returned and
		// released) -- if that ever changes, call clearSessionStateLocked
		// directly from inside the active closure instead, the way
		// CondenseSessionByID's clearAfter path does.
		return fmt.Errorf("clearSessionState: session %s gate already held by an active MutateSessionState frame on this goroutine", sessionID)
	}
	return s.clearSessionStateLocked(ctx, sessionID)
}

// listAllSessionStates returns all active session states.
// It filters out orphaned sessions whose shadow branch no longer exists.
func (s *ManualCommitStrategy) listAllSessionStates(ctx context.Context) ([]*SessionState, error) {
	store, err := s.getStateStore(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get state store: %w", err)
	}

	sessionStates, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list session states: %w", err)
	}

	if len(sessionStates) == 0 {
		return nil, nil
	}

	repo, err := OpenRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}
	defer repo.Close()

	var states []*SessionState
	for _, sessionState := range sessionStates {
		state := sessionState
		// Adopted-away source records are tombstones: keep them until normal stale
		// expiry so old source hooks cannot recreate a second live state.
		if state.AdoptedIntoWorktreePath != "" {
			states = append(states, state)
			continue
		}

		// Imported sessions are read-only historical records: no shadow branch
		// and (by design) no BaseCommit. Keep them regardless of the
		// shadow-branch orphan check below. Gate on Kind, not on commit
		// presence, so this stays correct once imports are linked to a commit.
		if state.Kind.IsImported() {
			states = append(states, state)
			continue
		}

		// Skip and cleanup orphaned sessions whose shadow branch no longer exists.
		// Keep active sessions (shadow branch may not be created yet) and sessions
		// with LastCheckpointID (needed for checkpoint ID reuse on subsequent commits).
		// Clean up everything else: stale pre-state-machine sessions (empty phase),
		// IDLE/ENDED sessions that were never condensed, etc.
		// Record-bearing sessions hold condensable content off the shadow branch — never orphaned.
		shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
		refName := plumbing.NewBranchReferenceName(shadowBranch)
		if _, err := repo.Reference(refName, true); err != nil {
			if !state.Phase.IsActive() && state.LastCheckpointID.IsEmpty() && !state.HasTaskContent() {
				//nolint:errcheck,gosec // G104: Cleanup is best-effort, shouldn't fail the list operation
				store.Clear(ctx, state.SessionID)
				continue
			}
		}

		states = append(states, state)
	}
	return states, nil
}

// IsCondensableEndedSession reports whether an ENDED session still carries
// uncondensed content AND can be salvaged by condensing
// (CondenseSessionByID). Two shapes qualify: checkpoint steps whose shadow
// branch still exists, and record-bearing sessions (pending subagent task
// records), whose content never lives on the shadow branch and so needs no
// branch to condense. ENDED sessions with steps but no shadow branch and no
// task records are NOT condensable; fixing those means discarding state,
// which the background sweep never initiates — those sessions are left to
// `entire doctor` (and the existing orphan cleanup in listAllSessionStates).
// Used by the PostCommit stale-session warning and the background zombie
// sweep.
func IsCondensableEndedSession(repo *git.Repository, state *SessionState) bool {
	if state.Phase != session.PhaseEnded || state.FullyCondensed ||
		(state.StepCount <= 0 && !state.HasTaskContent()) {
		return false
	}

	// Record-bearing sessions qualify without a shadow branch: task records
	// are stored off-branch, so condensation can materialize them regardless.
	if state.HasTaskContent() {
		return true
	}

	// Check shadow branch existence. For PostCommit this is a re-check —
	// its list arrives via listAllSessionStates, which already filters
	// orphaned sessions — and it is intentional even there: condensation
	// deletes shadow branches, so a branch that existed at list-load time may
	// be gone by the time the warning re-checks here, and we'd otherwise warn
	// about a session this commit (or a concurrent condense) just cleaned up.
	// The background zombie sweep, by contrast, arrives via the raw
	// ListSessionStates, so for the sweep this is the PRIMARY shadow-branch
	// check, not a re-check — it is what keeps the sweep condense-only.
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err := repo.Reference(refName, true)
	return err == nil
}

// countWarnableStaleEndedSessions returns the number of ENDED sessions that
// still remain slow and fixable after PostCommit finishes processing.
func countWarnableStaleEndedSessions(repo *git.Repository, sessions []*SessionState) int {
	n := 0
	for _, state := range sessions {
		if IsCondensableEndedSession(repo, state) {
			n++
		}
	}
	return n
}

// findExactSessionsForWorktree returns sessions recorded for exactly this
// worktree path, with no sibling/parent fallback. Callers that mutate
// worktree-coupled state (e.g. BaseCommit, which must only follow the HEAD of
// the session's own worktree) must use this instead of
// findSessionsForWorktree.
func (s *ManualCommitStrategy) findExactSessionsForWorktree(ctx context.Context, worktreePath string) ([]*SessionState, error) {
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		return nil, err
	}
	return exactWorktreeMatches(allStates, worktreePath), nil
}

func exactWorktreeMatches(states []*SessionState, worktreePath string) []*SessionState {
	var exact []*SessionState
	for _, state := range states {
		// Imported sessions are historical records reconstructed from
		// transcripts; a fresh commit must never link to one (a leaked
		// imported fixture once hijacked a commit's trailer — the sessC
		// incident).
		if state.Kind.IsImported() {
			continue
		}
		if state.WorktreePath == worktreePath {
			exact = append(exact, state)
		}
	}
	return exact
}

// findSessionsForWorktree finds all sessions for the given worktree path.
// Exact WorktreePath matches win; otherwise sessions recorded in another
// worktree of the same repository (shared git common dir) are matched, as long
// as all candidates come from a single worktree. Callers that also run
// identity matching use findSessionsForWorktreeFromStates to share one state
// listing; this wrapper serves the paths that only need worktree semantics
// (amend, post-rewrite) and deliberately drops the ambiguity signal — their
// commits are history edits where an adopt hint would be noise.
func (s *ManualCommitStrategy) findSessionsForWorktree(ctx context.Context, worktreePath string) ([]*SessionState, error) {
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		return nil, err
	}
	matches, _ := s.findSessionsForWorktreeFromStates(ctx, allStates, worktreePath)
	return matches, nil
}

// findSessionsForWorktreeFromStates is findSessionsForWorktree over an
// already-loaded state list. The second result reports a multi-worktree
// ambiguity decline — candidates existed but spanned several worktrees, so
// nothing was linked; findSessionsForCommitLinking surfaces it to the user
// only when identity matching cannot rescue the commit either.
func (s *ManualCommitStrategy) findSessionsForWorktreeFromStates(ctx context.Context, allStates []*SessionState, worktreePath string) ([]*SessionState, bool) {
	if exact := exactWorktreeMatches(allStates, worktreePath); len(exact) > 0 {
		return exact, false
	}

	worktreeCommonDir, err := gitCommonDirForWorktree(ctx, worktreePath)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"),
			"session matching: cannot resolve common dir for fallback matching",
			slog.String("error", err.Error()))
		return nil, false
	}

	var parentWorktreeMatches []*SessionState
	var commonDirMatches []*SessionState
	commonDirByPath := make(map[string]string)
	for _, state := range allStates {
		if state.WorktreePath == "" || state.Kind.IsImported() {
			continue
		}

		stateCommonDir, seen := commonDirByPath[state.WorktreePath]
		if !seen {
			stateCommonDir = gitCommonDirForWorktreeOrEmpty(ctx, state.WorktreePath)
			commonDirByPath[state.WorktreePath] = stateCommonDir
		}
		if stateCommonDir == "" || stateCommonDir != worktreeCommonDir {
			continue
		}

		if isNestedWorktreeOfRecordedRepo(state.WorktreePath, worktreePath) {
			parentWorktreeMatches = append(parentWorktreeMatches, state)
			continue
		}

		commonDirMatches = append(commonDirMatches, state)
	}

	if len(parentWorktreeMatches) > 0 {
		return resolveWorktreeCandidates(ctx, worktreePath, parentWorktreeMatches)
	}
	return resolveWorktreeCandidates(ctx, worktreePath, commonDirMatches)
}

// recentSessionWindow bounds the liveness filter below: a session that
// interacted within this window is plausibly the one whose work is being
// committed right now; one idle for days is not. This deliberately converts
// some multi-worktree cases the old code declined into a best-candidate link:
// a session in a long-running build or tool call can age out and leave the
// other recent worktree to win. Commits normally follow agent activity by
// seconds to minutes, so 15 minutes is the chosen correctness tradeoff.
const recentSessionWindow = 15 * time.Minute

// resolveWorktreeCandidates reduces fallback candidates to a linkable set.
// All from one worktree: linked (the supported concurrent-session case).
// Spanning several worktrees: filter to recently-interacting sessions and
// link only if a single worktree remains — days-idle stragglers must not
// veto the obviously-live session, but between two live worktrees there is
// no safe guess. A refusal logs here and reports true, so the
// commit-linking caller can announce it on stderr with the remedy (#1852:
// silent loss of linkage) — but only after identity matching has also failed
// to rescue the commit, and never on amend/post-rewrite.
func resolveWorktreeCandidates(ctx context.Context, worktreePath string, candidates []*SessionState) (matches []*SessionState, ambiguous bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	if matches := sessionsFromSingleWorktree(candidates); matches != nil {
		return matches, false
	}
	cutoff := time.Now().Add(-recentSessionWindow)
	var live []*SessionState
	for _, state := range candidates {
		if state.LastInteractionTime != nil && state.LastInteractionTime.After(cutoff) {
			live = append(live, state)
		}
	}
	if len(live) > 0 {
		if matches := sessionsFromSingleWorktree(live); matches != nil {
			return matches, false
		}
	}
	warnAmbiguousWorktreeSessions(ctx, worktreePath, candidates)
	return nil, true
}

// warnAmbiguousWorktreeSessions surfaces refused fallback matches: live
// sessions exist in other worktrees of this repo, but they span multiple
// worktrees so no automatic match is safe. Without this warning, commits made
// here silently lose their Entire-Checkpoint linkage (#1852) with only a
// DEBUG-level trace.
func warnAmbiguousWorktreeSessions(ctx context.Context, worktreePath string, candidates []*SessionState) {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	seen := make(map[string]struct{}, len(candidates))
	worktrees := make([]string, 0, len(candidates))
	for _, state := range candidates {
		if _, ok := seen[state.WorktreePath]; ok {
			continue
		}
		seen[state.WorktreePath] = struct{}{}
		worktrees = append(worktrees, state.WorktreePath)
	}
	logging.Warn(logCtx, "session matching: ambiguous sessions across worktrees; commit will not be linked",
		slog.String("commit_worktree", worktreePath),
		slog.Int("candidate_sessions", len(candidates)),
		slog.Any("candidate_worktrees", worktrees),
	)

	// logging.Warn only reaches the internal .entire/logs file, so on its own it
	// leaves the #1852 case silent to the person running `git commit`. Surface a
	// notice on stderrWriter too — the same user-facing hook channel
	// warnIfAttributionDiverged and warnStaleEndedSessions use — with a runnable
	// remedy. The command names a concrete session ID (the bare `--from <path>`
	// form relies on adoption's 12h auto-detect, which errors here) and passes
	// --yes (required for same-repo adoption).
	fmt.Fprint(stderrWriter, formatAmbiguousWorktreeNotice(worktrees, mostRecentlyAdoptableSession(candidates)))
}

// mostRecentlyAdoptableSession returns the newest-by-last-seen candidate that
// `entire session adopt` would accept (not ended, not fully condensed), or nil
// if none qualify. Mirrors isAdoptableSourceSession in session_adopt.go so the
// remedy we print is one adoption will actually run.
func mostRecentlyAdoptableSession(candidates []*SessionState) *SessionState {
	var best *SessionState
	for _, state := range candidates {
		if state == nil || state.Phase == session.PhaseEnded || state.EndedAt != nil || state.FullyCondensed {
			continue
		}
		if best == nil || ambiguousSessionLastSeen(state).After(ambiguousSessionLastSeen(best)) {
			best = state
		}
	}
	return best
}

func ambiguousSessionLastSeen(state *SessionState) time.Time {
	if state.LastInteractionTime != nil {
		return *state.LastInteractionTime
	}
	return state.StartedAt
}

// formatAmbiguousWorktreeNotice builds the user-facing stderr message. It always
// reports the ambiguity; when an adoptable session exists it appends a directly
// runnable adopt command. Returns "" when there are no worktrees to report.
func formatAmbiguousWorktreeNotice(worktrees []string, primary *SessionState) string {
	if len(worktrees) == 0 {
		return ""
	}
	notice := fmt.Sprintf(
		"entire: this commit was not linked to a checkpoint (live agent sessions span multiple worktrees: %s).\n",
		strings.Join(worktrees, ", "),
	)
	if primary != nil && primary.SessionID != "" && primary.WorktreePath != "" {
		// Shell-quote the worktree path: it can contain spaces or shell
		// metacharacters, so an unquoted --from value would word-split or
		// misbehave when the user copy-pastes this command.
		notice += fmt.Sprintf(
			"  to link one explicitly, run: entire session adopt %s --from %s --yes\n",
			primary.SessionID, shellSingleQuote(primary.WorktreePath),
		)
	}
	return notice
}

// shellSingleQuote wraps s in single quotes so it pastes into a POSIX shell as a
// single literal argument, escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sessionsFromSingleWorktree returns the candidates only when they were all
// recorded in the same worktree. Multiple sessions in one worktree is the
// supported concurrent-session case (matching exact-match semantics);
// candidates spanning distinct worktrees are ambiguous, so no fallback match
// is made rather than guessing which worktree the commit belongs to.
func sessionsFromSingleWorktree(candidates []*SessionState) []*SessionState {
	if len(candidates) == 0 {
		return nil
	}
	first := candidates[0].WorktreePath
	for _, state := range candidates[1:] {
		if state.WorktreePath != first {
			return nil
		}
	}
	return candidates
}

// reconcileWorktreePathForResumedTurn repoints a main-worktree session's recorded
// WorktreePath when a turn starts from a different main worktree AND the recorded
// path no longer resolves to this repository's git common dir. That is the
// repo-relocation case (#1890): the whole repo directory was renamed or moved
// while the session was stopped, then the agent resumed the same session from the
// new location. WorktreePath is matched by exact string equality at commit time
// (findSessionsForWorktree), so without this the resumed session's commits
// silently lose their Entire-Checkpoint trailer.
//
// Scope is deliberately restricted to relocations between MAIN worktrees
// (WorktreeID == "" on both the recorded state and the current worktree). Doing
// so keeps WorktreePath and WorktreeID aligned by construction: both stay "".
// Repointing a linked-worktree session (WorktreeID != "") onto the main worktree
// would leave WorktreeID describing a different worktree than WorktreePath, and
// that disalignment breaks every consumer that re-derives the worktree id from
// the current directory — `entire clean` and `entire explain` compute the wrong
// shadow-branch name (orphaning or hiding the session's checkpoints), and the
// post-commit base/attribution updates follow the wrong worktree's HEAD.
// Linked-worktree relocation is a rare, documented non-goal; the zero-match path
// still warns so the trailer loss is not silent.
//
// The session store lives in the git common dir, so a state loaded here is by
// construction the same repo; when its recorded worktree no longer maps back to
// that common dir, the only safe target is the current worktree. A still-valid
// recorded path (e.g. a concurrent sibling worktree that resolves to the same
// common dir) is left untouched so we never steal a session from a live sibling.
func reconcileWorktreePathForResumedTurn(ctx context.Context, state *SessionState) {
	// Only main-worktree sessions are reconciled — see the disalignment note above.
	if state.WorktreeID != "" {
		return
	}

	current, err := paths.WorktreeRoot(ctx)
	if err != nil || current == "" || state.WorktreePath == "" {
		return
	}
	if filepath.Clean(current) == filepath.Clean(state.WorktreePath) {
		return
	}

	// Only repoint onto another main worktree. Resuming into a linked worktree
	// would reintroduce the WorktreeID/WorktreePath disalignment from the other
	// direction, so leave those alone.
	currentWorktreeID, err := paths.GetWorktreeID(current)
	if err != nil || currentWorktreeID != "" {
		return
	}

	// Resolve both through the same probe so normalization matches. An empty
	// current common dir means we can't establish our own repo — bail. When the
	// recorded path still resolves to our common dir it remains a valid
	// worktree, so leave it alone.
	currentCommonDir := gitCommonDirForWorktreeOrEmpty(ctx, current)
	if currentCommonDir == "" {
		return
	}
	if gitCommonDirForWorktreeOrEmpty(ctx, state.WorktreePath) == currentCommonDir {
		return
	}

	old := state.WorktreePath
	state.WorktreePath = current
	// WorktreeID stays "" — both the recorded and current worktrees are the main
	// worktree, so WorktreePath and WorktreeID remain aligned and the shadow branch
	// (entire/<base>-hash("")) is unchanged.
	logging.Info(logging.WithComponent(ctx, "hooks"), "reconciled main-worktree session path after relocation",
		slog.String("session_id", state.SessionID),
		slog.String("from", old),
		slog.String("to", current),
	)
}

func gitCommonDirForWorktreeOrEmpty(ctx context.Context, worktreePath string) string {
	commonDir, err := gitCommonDirForWorktree(ctx, worktreePath)
	if err != nil {
		return ""
	}
	return commonDir
}

func gitCommonDirForWorktree(ctx context.Context, worktreePath string) (string, error) {
	if worktreePath == "" {
		return "", errors.New("empty worktree path")
	}

	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--git-common-dir")
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir for %s: %w", worktreePath, err)
	}

	commonDir := strings.TrimSpace(string(output))
	if commonDir == "" {
		return "", fmt.Errorf("empty git common dir for %s", worktreePath)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if resolved, err := filepath.EvalSymlinks(commonDir); err == nil {
		commonDir = resolved
	}
	return commonDir, nil
}

func isNestedWorktreeOfRecordedRepo(recordedWorktreePath, commitWorktreePath string) bool {
	nestedWorktreesDir := filepath.Join(filepath.Clean(recordedWorktreePath), ".worktrees")
	return paths.IsSubpath(nestedWorktreesDir, filepath.Clean(commitWorktreePath))
}

type rewritePair struct {
	OldSHA string
	NewSHA string
}

func remapRewriteSHA(sha string, rewrites []rewritePair) (string, bool) {
	for _, pair := range rewrites {
		if sha == pair.OldSHA {
			return pair.NewSHA, true
		}
	}
	return sha, false
}

func shadowBranchExistsForBaseCommit(repo *git.Repository, baseCommit, worktreeID string) bool {
	if repo == nil || baseCommit == "" {
		return false
	}

	refName := plumbing.NewBranchReferenceName(checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID))
	_, err := repo.Reference(refName, true)
	return err == nil
}

func (s *ManualCommitStrategy) remapSessionForRewrite(ctx context.Context, repo *git.Repository, state *SessionState, rewrites []rewritePair) (bool, error) {
	if state == nil {
		return false, nil
	}

	newBaseCommit, baseChanged := remapRewriteSHA(state.BaseCommit, rewrites)
	newAttrBaseCommit, attrChanged := remapRewriteSHA(state.AttributionBaseCommit, rewrites)
	if !baseChanged && !attrChanged {
		return false, nil
	}

	hadShadowBranch := shadowBranchExistsForBaseCommit(repo, state.BaseCommit, state.WorktreeID)
	if baseChanged {
		changed, err := s.migrateShadowBranchToBaseCommit(ctx, repo, state, newBaseCommit)
		if err != nil {
			return false, fmt.Errorf("failed to migrate rewritten shadow branch: %w", err)
		}
		baseChanged = changed
	}

	// If a shadow branch existed, preserve AttributionBaseCommit so future
	// attribution still diffs against the original checkpoint base captured on
	// that branch. Without a shadow branch, keep attribution in sync with the
	// rewritten commit lineage.
	if attrChanged && !hadShadowBranch {
		state.AttributionBaseCommit = newAttrBaseCommit
	}

	return baseChanged || attrChanged, nil
}

// findSessionsForCommit finds all sessions where base_commit matches the given SHA.
func (s *ManualCommitStrategy) findSessionsForCommit(ctx context.Context, baseCommitSHA string) ([]*SessionState, error) {
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		return nil, err
	}

	var matching []*SessionState
	for _, state := range allStates {
		if state.BaseCommit == baseCommitSHA {
			matching = append(matching, state)
		}
	}
	return matching, nil
}

// FindSessionsForCommit is the exported version of findSessionsForCommit.
// Used by `entire clean` to find sessions to clean up.
func (s *ManualCommitStrategy) FindSessionsForCommit(ctx context.Context, baseCommitSHA string) ([]*SessionState, error) {
	return s.findSessionsForCommit(ctx, baseCommitSHA)
}

// CountOtherActiveSessionsWithCheckpoints counts how many other active sessions
// from the SAME worktree (different from currentSessionID) have created checkpoints
// on the SAME base commit (current HEAD). This is used to show an informational message
// about concurrent sessions that will be included in the next commit.
// Returns 0, nil if no such sessions exist.
func (s *ManualCommitStrategy) CountOtherActiveSessionsWithCheckpoints(ctx context.Context, currentSessionID string) (int, error) {
	currentWorktree, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get worktree root: %w", err)
	}

	// Get current HEAD to compare with session base commits
	repo, err := OpenRepository(ctx)
	if err != nil {
		return 0, err
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		return 0, fmt.Errorf("failed to get HEAD: %w", err)
	}
	currentHead := head.Hash().String()

	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, state := range allStates {
		// Only consider sessions from the same worktree with checkpoints
		// AND based on the same commit (current HEAD)
		// Sessions from different base commits are independent and shouldn't be counted
		if state.SessionID != currentSessionID &&
			state.WorktreePath == currentWorktree &&
			(state.StepCount > 0 || state.HasTaskContent()) &&
			state.BaseCommit == currentHead {
			count++
		}
	}
	return count, nil
}

// initializeSession creates a new session state or updates a partial one.
// A partial state may exist if the concurrent session warning was shown.
// agentType is the human-readable name of the agent (e.g., "Claude Code").
// transcriptPath is the path to the live transcript file (for mid-session commit detection).
// userPrompt is the user's prompt text (stored truncated as LastPrompt for display).
// model is the LLM model identifier (e.g., "claude-sonnet-4-20250514"); empty if unknown.
func (s *ManualCommitStrategy) initializeSession(ctx context.Context, repo *git.Repository, sessionID string, agentType types.AgentType, transcriptPath string, userPrompt string, model string) error {
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}

	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("failed to get worktree path: %w", err)
	}

	// Get worktree ID for shadow branch naming
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		return fmt.Errorf("failed to get worktree ID: %w", err)
	}

	// Capture untracked files at session start so checkpoint bookkeeping can tell
	// them apart from files the session created
	untrackedFiles, err := collectUntrackedFiles(ctx)
	if err != nil {
		// Non-fatal: continue even if we can't collect untracked files
		untrackedFiles = nil
	}

	// Generate TurnID for the first turn
	turnID, err := id.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate turn ID: %w", err)
	}

	now := time.Now()
	headHash := head.Hash().String()
	state := &SessionState{
		SessionID:             sessionID,
		CLIVersion:            versioninfo.Version,
		BaseCommit:            headHash,
		AttributionBaseCommit: headHash,
		WorktreePath:          worktreePath,
		WorktreeID:            worktreeID,
		StartedAt:             now,
		LastInteractionTime:   &now,
		TurnID:                turnID.String(),
		StepCount:             0,
		UntrackedFilesAtStart: untrackedFiles,
		AgentType:             agentType,
		ModelName:             model,
		TranscriptPath:        transcriptPath,
		LastPrompt:            truncatePromptForStorage(userPrompt),
	}

	// Take the gate, then re-check under lock. Without this re-check a
	// concurrent turn-start hook that wrote a richer state in the gap
	// between our caller's existence check and our save would have its
	// fields (TranscriptPath, LastPrompt, ModelName, accumulated TurnID)
	// overwritten with blanks here.
	_, _, release, lockErr := acquireSessionGate(ctx, sessionID)
	if lockErr != nil {
		return fmt.Errorf("acquire state lock: %w", lockErr)
	}
	defer release()
	existing, loadErr := s.loadSessionState(ctx, sessionID)
	if loadErr != nil {
		return fmt.Errorf("re-load session state under lock: %w", loadErr)
	}
	if existing != nil && existing.BaseCommit != "" {
		return nil
	}
	return s.saveSessionState(ctx, state)
}

// getShadowBranchNameForCommit returns the shadow branch name for the given base commit and worktree ID.
// worktreeID should be empty for the main worktree or the internal git worktree name for linked worktrees.
func getShadowBranchNameForCommit(baseCommit, worktreeID string) string {
	return checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
}
