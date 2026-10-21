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

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v6"
)

// Manual-commit strategy session state methods.
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
// It clears orphaned sessions (see isOrphanedSessionState) as it lists.
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

	var states []*SessionState
	for _, sessionState := range sessionStates {
		state := sessionState
		// Adopted-away source records are tombstones: keep them until normal stale
		// expiry so old source hooks cannot recreate a second live state.
		if state.AdoptedIntoWorktreePath != "" {
			states = append(states, state)
			continue
		}

		// Imported sessions are read-only historical records with (by design)
		// no BaseCommit. Keep them regardless of the orphan check below. Gate
		// on Kind, not on commit presence, so this stays correct once imports
		// are linked to a commit.
		if state.Kind.IsImported() {
			states = append(states, state)
			continue
		}

		// Skip and clean up orphaned sessions: ended, never condensed, and
		// holding no pending work. Keep non-ended sessions (including legacy
		// empty phases normalized to IDLE) and sessions with LastCheckpointID
		// (needed for checkpoint ID reuse on subsequent commits).
		if isOrphanedSessionState(state) {
			logging.Debug(logging.WithComponent(ctx, "session"), "removing orphaned session state with no pending work",
				slog.String("session_id", state.SessionID),
				slog.String("phase", string(state.Phase)))
			//nolint:errcheck,gosec // G104: Cleanup is best-effort, shouldn't fail the list operation
			store.Clear(ctx, state.SessionID)
			continue
		}

		states = append(states, state)
	}
	return states, nil
}

// isOrphanedSessionState reports whether a listed state may be deleted: only a
// finalized session (State.IsEnded) that was never condensed and holds no
// pending work (State.HasPendingWork). IDLE states — including legacy empty
// phases, which normalize to IDLE — are live sessions between turns or belong
// to the exited-owner finalizer, so they age out through StaleSessionThreshold.
func isOrphanedSessionState(state *SessionState) bool {
	return state.IsEnded() && state.LastCheckpointID.IsEmpty() && !state.HasPendingWork()
}

// IsCondensableEndedSession reports whether an ENDED, not fully condensed
// session still carries uncondensed content (State.HasPendingWork), which condensing
// (CondenseSessionByID) can always salvage: turn-end steps condense from the
// session's transcript and recorded files, task records from their own
// transcripts. Used by the PostCommit stale-session warning and the
// background zombie sweep.
func IsCondensableEndedSession(state *SessionState) bool {
	return state.Phase == session.PhaseEnded && !state.FullyCondensed && state.HasPendingWork()
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
	return exactWorktreeMatches(ctx, allStates, worktreePath), nil
}

func exactWorktreeMatches(ctx context.Context, states []*SessionState, worktreePath string) []*SessionState {
	exact, _ := matchWorktreeHomes(ctx, states, worktreePath)
	return exact
}

// matchWorktreeHomes splits states by whether worktreePath is their home.
// exact holds those homed there, a home moved there by `git worktree move`
// included, with WorktreePath set to worktreePath in memory only (a moved
// home's stored state follows under the session's lock: post-commit, turn
// start; a home reached by another spelling of its path is not repointed, so
// post-commit's path checks still treat that commit as a guest's, as before). notHome holds the IDs of those whose home
// marker shows worktreePath is not their home even at their recorded path: a
// worktree git re-added there. Sessions the marker can't judge are matched by
// path, as before markers.
func matchWorktreeHomes(ctx context.Context, states []*SessionState, worktreePath string) (exact []*SessionState, notHome map[string]bool) {
	here := &worktreeAt{root: worktreePath}
	for _, state := range states {
		// Imported sessions are historical records reconstructed from
		// transcripts; a fresh commit must never link to one (a leaked
		// imported fixture once hijacked a commit's trailer — the sessC
		// incident).
		if state.Kind.IsImported() {
			continue
		}
		switch here.home(ctx, state) {
		case homeHere, homeMovedHere:
			state.WorktreePath = worktreePath
			exact = append(exact, state)
		case homeNotHere, homeReplaced:
			if notHome == nil {
				notHome = make(map[string]bool)
			}
			notHome[state.SessionID] = true
		case homeUnknown:
			if state.WorktreePath == worktreePath {
				exact = append(exact, state)
			}
		}
	}
	return exact, notHome
}

// BackgroundClaimContext is what a background task's SubagentStop needs to
// decide which new untracked files it may claim.
type BackgroundClaimContext struct {
	// Recorded holds every path a session has pending in the task's
	// worktree: in FilesTouched for one homed there, in its entry for that
	// worktree for one that edited it from elsewhere.
	Recorded map[string]struct{}

	// Overlapped reports that another background task in the worktree was
	// running at some point since this task launched: still running, or
	// completed after this task's launch.
	Overlapped bool
}

// LoadBackgroundClaimContext loads the BackgroundClaimContext for the task
// toolUseID of sessionID. ok is false when the session has no state, no
// recorded worktree, or no record for the task, so its launch time and the
// files other sessions own are unknown.
func LoadBackgroundClaimContext(ctx context.Context, sessionID, toolUseID string) (claims BackgroundClaimContext, ok bool, err error) {
	states, err := ListSessionStates(ctx)
	if err != nil {
		return BackgroundClaimContext{}, false, err
	}
	var own *SessionState
	for _, state := range states {
		if state.SessionID == sessionID {
			own = state
			break
		}
	}
	if own == nil || own.WorktreePath == "" {
		return BackgroundClaimContext{}, false, nil
	}
	task := own.FindTaskRecord(toolUseID)
	if task == nil || launchedBeforeRehome(own, task) {
		// One launched before the session re-homed ran in the removed
		// worktree: what is new here isn't its.
		return BackgroundClaimContext{}, false, nil
	}
	claims.Recorded = make(map[string]struct{})
	key := session.WorktreeKey(own.WorktreeID)
	for _, state := range states {
		work := state.OtherWorktrees[key]
		if work == nil || state.Kind.IsImported() {
			continue
		}
		// An entry from a worktree git removed before adding this one under
		// its ID records nothing here; one that can't be checked still counts,
		// so a failed check can't hand another session's file to the task.
		if current, err := worktreeEntryCurrent(ctx, own.WorktreePath, key, work); err == nil && !current {
			continue
		}
		for _, file := range work.FilesTouched {
			claims.Recorded[file] = struct{}{}
		}
	}
	for _, state := range exactWorktreeMatches(ctx, states, own.WorktreePath) {
		for _, file := range state.FilesTouched {
			claims.Recorded[file] = struct{}{}
		}
		for _, rec := range state.TaskRecords {
			if !rec.Background || (state.SessionID == sessionID && rec.ToolUseID == toolUseID) || launchedBeforeRehome(state, &rec) {
				continue
			}
			// SessionEnd completes every live record, so one left on an
			// ended session was leaked, not running.
			if rec.CompletedAt.IsZero() && state.IsEnded() {
				continue
			}
			if rec.CompletedAt.IsZero() || rec.CompletedAt.After(task.StartedAt) {
				claims.Overlapped = true
			}
		}
	}
	return claims, true, nil
}

// launchedBeforeRehome reports whether task, one of state's, was launched
// before the session re-homed (State.RehomedAt), in the worktree git removed.
func launchedBeforeRehome(state *SessionState, task *session.TaskRecord) bool {
	return state.RehomedAt != nil && task.StartedAt.Before(*state.RehomedAt)
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
	exact, notHome := matchWorktreeHomes(ctx, allStates, worktreePath)
	if len(exact) > 0 {
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
		// A session whose home this worktree is known not to be can't be
		// matched as a sibling of it either.
		if state.WorktreePath == "" || state.Kind.IsImported() || notHome[state.SessionID] {
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

// reconcileWorktreePathForResumedTurn keeps a session's recorded home
// (WorktreePath) pointing at its worktree when a turn starts after that
// worktree moved, since commits are matched to sessions by their home
// (findSessionsForWorktree).
//
// A main-worktree session (WorktreeID "") is repointed when the turn starts
// from a different main worktree AND the recorded path no longer resolves to
// this repository's git common dir. That is the repo-relocation case (#1890):
// the whole repo directory was renamed or moved while the session was
// stopped, then the agent resumed the same session from the new location.
// Repointing between main worktrees keeps WorktreePath and WorktreeID aligned
// by construction: both stay "". A session resumed into a linked worktree is
// left alone, or WorktreePath would name a different worktree than
// WorktreeID, breaking every consumer that re-derives the worktree id from
// the current directory.
//
// A linked-worktree session is judged by its home marker (homeAt), which git
// keeps through `git worktree move`: a turn starting in its home moved there
// repoints WorktreePath and keeps WorktreeID, which the move keeps too, so the
// two stay aligned. A turn starting in a worktree git added at the home's
// path after removing the home re-homes the session there (rehome): the agent
// resumed in it. A session homed before markers were kept gains one here at
// its next turn at home (stampLinkedHome), and with it this handling.
//
// The session store lives in the git common dir, so a state loaded here is by
// construction the same repo; when its recorded worktree no longer maps back to
// that common dir, the only safe target is the current worktree. A still-valid
// recorded path (e.g. a concurrent sibling worktree that resolves to the same
// common dir) is left untouched so we never steal a session from a live sibling.
func reconcileWorktreePathForResumedTurn(ctx context.Context, state *SessionState) {
	current, err := paths.WorktreeRoot(ctx)
	if err != nil || current == "" || state.WorktreePath == "" {
		return
	}
	if state.WorktreeID != "" {
		if followMovedHome(ctx, state, current) == homeReplaced {
			rehome(ctx, state, current)
		}
		stampLinkedHome(ctx, state, current)
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
	// worktree, so WorktreePath and WorktreeID remain aligned.
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

// syncBaseCommitToHead moves state.BaseCommit to the current HEAD when HEAD
// has moved since the session last saw it (a user commit, pull, rebase,
// reset). BaseCommit records the commit a session's pending work sits on top
// of; nothing else is keyed on it anymore, so following HEAD is all there is
// to do. A state with no BaseCommit yet is left for initialization.
func syncBaseCommitToHead(ctx context.Context, repo *git.Repository, state *SessionState) error {
	if state == nil || state.BaseCommit == "" {
		return nil
	}
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("failed to get HEAD: %w", err)
	}
	if currentHead := head.Hash().String(); state.BaseCommit != currentHead {
		logging.Debug(logging.WithComponent(ctx, "session"), "updated session base commit",
			slog.String("session_id", state.SessionID),
			slog.String("new_base", truncateHash(currentHead)))
		state.BaseCommit = currentHead
	}
	return nil
}

// remapSessionForRewrite moves a session's BaseCommit through an amend or
// rebase rewrite. It reports whether the state changed.
func remapSessionForRewrite(state *SessionState, rewrites []rewritePair) bool {
	if state == nil {
		return false
	}
	newBaseCommit, baseChanged := remapRewriteSHA(state.BaseCommit, rewrites)
	if !baseChanged || newBaseCommit == state.BaseCommit {
		return false
	}
	state.BaseCommit = newBaseCommit
	return true
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

	// Get the worktree ID that identifies this session's worktree
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		return fmt.Errorf("failed to get worktree ID: %w", err)
	}

	// A linked home is known by its marker, which follows it through `git
	// worktree move` (homeAt). Unmarked, the session is matched by path, as
	// before markers.
	worktreeIncarnation, err := MarkWorktree(ctx, worktreePath, worktreeID)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"), "could not mark the session's home worktree",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		worktreeIncarnation = ""
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
		WorktreePath:          worktreePath,
		WorktreeID:            worktreeID,
		WorktreeIncarnation:   worktreeIncarnation,
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
	if agentType == agent.AgentTypeCodex {
		complete := true
		state.SubagentInventoryComplete = &complete
		state.SubagentTokensBaselineComplete = &complete
	}

	// Take the gate, then re-check under lock. Without this re-check a
	// concurrent turn-start hook that wrote a richer state in the gap
	// between our caller's existence check and our save would have its
	// fields (TranscriptPath, LastPrompt, ModelName, accumulated TurnID)
	// overwritten with blanks here.
	//
	// The agent home is remembered only after the gate is released, so the
	// registry's I/O never extends the hold time for a concurrent hook.
	remember, err := func() (string, error) {
		_, _, release, lockErr := acquireSessionGate(ctx, sessionID)
		if lockErr != nil {
			return "", fmt.Errorf("acquire state lock: %w", lockErr)
		}
		defer release()
		existing, loadErr := s.loadSessionState(ctx, sessionID)
		if loadErr != nil {
			return "", fmt.Errorf("re-load session state under lock: %w", loadErr)
		}
		if existing != nil && existing.BaseCommit != "" {
			return "", nil
		}
		if existing != nil && agentType == agent.AgentTypeCodex {
			// Repair the partial state in place. A child hook can have
			// recorded task content and accounting before the parent session
			// initializes, so a fresh replacement would silently discard
			// durable child state.
			state = existing
			state.CLIVersion = versioninfo.Version
			state.BaseCommit = headHash
			state.WorktreePath = worktreePath
			state.WorktreeID = worktreeID
			state.WorktreeIncarnation = worktreeIncarnation
			if state.StartedAt.IsZero() {
				state.StartedAt = now
			}
			state.LastInteractionTime = &now
			state.TurnID = turnID.String()
			state.AgentType = agentType
			if model != "" {
				state.ModelName = model
			}
			if transcriptPath != "" {
				state.TranscriptPath = transcriptPath
			}
			if userPrompt != "" {
				state.LastPrompt = truncatePromptForStorage(userPrompt)
			}
			if state.UntrackedFilesAtStart == nil {
				state.UntrackedFilesAtStart = untrackedFiles
			}

			// This is a repair, not an authoritative SessionStart inventory.
			// Keep the ledger and token data but retain conservative coverage
			// markers.
			incomplete := false
			state.SubagentInventoryComplete = &incomplete
			state.SubagentTokensBaselineComplete = &incomplete
		}
		remember := updateSessionAgentHome(ctx, state)
		return remember, s.saveSessionState(ctx, state)
	}()
	if err != nil {
		return err
	}
	rememberAgentHome(ctx, state.AgentType, remember)
	return nil
}

// updateSessionAgentHome sets state.AgentHome for state.AgentType and
// state.TranscriptPath, and returns the home the caller should pass to
// rememberAgentHome once state is saved, or "" when there is none.
//
// state.AgentHome becomes the agent's active home when one of that home's
// stores holds the transcript, spelled as the environment sets it or in its
// canonical form, whichever contains the transcript path; that home is
// returned. Otherwise an existing state.AgentHome is kept while its stores hold
// the transcript, and cleared when they do not. With no transcript path,
// state.AgentHome is left unchanged. For an unknown agent, or one without a
// home layout, it is cleared. A home read from state is never returned, so it
// never reaches the registry.
func updateSessionAgentHome(ctx context.Context, state *SessionState) (remember string) {
	ag, err := agent.GetByAgentType(state.AgentType)
	if err != nil {
		state.AgentHome = ""
		return ""
	}
	provider, ok := agent.AsHomeLayoutProvider(ag)
	if !ok {
		state.AgentHome = ""
		return ""
	}
	if state.TranscriptPath == "" {
		return ""
	}
	active, ok, err := agent.ActiveHomeHolding(provider, state.TranscriptPath)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"), "agent home unavailable",
			slog.String("agent", string(state.AgentType)),
			slog.String("error", err.Error()))
	}
	if ok {
		state.AgentHome = active
		return active
	}
	if state.AgentHome != "" {
		transcript := filepath.Clean(state.TranscriptPath)
		if provider.HomeLayout().Holds(filepath.Clean(state.AgentHome), transcript) {
			return ""
		}
	}
	state.AgentHome = ""
	return ""
}

// rememberAgentHome records home, as returned by updateSessionAgentHome, in the
// per-user registry of agentType's homes, and does nothing for "". A failure is
// logged rather than returned: it only stops later lookups from trusting this
// home.
func rememberAgentHome(ctx context.Context, agentType types.AgentType, home string) {
	if home == "" {
		return
	}
	if err := agent.RememberAgentHome(agentType, home); err != nil {
		logging.Warn(logging.WithComponent(ctx, "session"), "failed to record agent home",
			slog.String("agent", string(agentType)),
			slog.String("error", err.Error()))
	}
}
