package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
)

// findSessionsForCommitLinking resolves which sessions a commit belongs to:
// the union of the worktree-matched set (a commit captures the worktree's
// content, so every session with pending content here belongs in it —
// concurrent sessions interleave by design) and the identity-matched session
// when the committing process's ancestry names one that worktree matching
// missed. There is no precedence between the two — an identity hit must
// never suppress worktree matches, or concurrent same-worktree sessions
// would drop out of the commit. The identity union is what makes an
// agent-made commit immune to worktree bookkeeping drift: an agent
// committing in a sibling worktree still links to its own session
// (guest-linked), where path matching alone found nothing or declined as
// ambiguous.
//
// This is also the only place the multi-worktree ambiguity decline is
// surfaced to the user: the hint fires only when the FINAL set is empty, so
// a commit rescued by identity matching never sees a false "none was
// linked", and amend/post-rewrite paths (which call findSessionsForWorktree
// directly) stay silent.
func (s *ManualCommitStrategy) findSessionsForCommitLinking(ctx context.Context, worktreePath string) ([]*SessionState, error) {
	linking, err := s.findCommitLinkingSet(ctx, worktreePath)
	return linking.sessions, err
}

// commitLinkingSet is findSessionsForCommitLinking's result with provenance:
// which session process ancestry identified, and the full listing it came from.
type commitLinkingSet struct {
	sessions      []*SessionState
	ancestryGuest string          // session whose owner is an ancestor of this hook, or ""
	all           []*SessionState // the listing, for sessionsIncludingReservedFor
}

// sessionsIncludingReservedFor adds every session whose pending condensation
// is checkpointID — the reservation prepare-commit-msg made when it stamped
// the trailer. Paths and ancestry are re-derived per hook and can differ
// between prepare-commit-msg and post-commit; the trailer is the identity the
// commit itself carries. Adopted-away tombstones are skipped: adoption retires
// them ENDED and fully condensed and clears the live copy's reservation, so a
// stale reservation must not condense stale state.
func (l commitLinkingSet) sessionsIncludingReservedFor(checkpointID id.CheckpointID) []*SessionState {
	sessions := l.sessions
	if checkpointID == id.EmptyCheckpointID {
		return sessions
	}
	for _, state := range l.all {
		if state.Kind.IsImported() || state.AdoptedIntoWorktreePath != "" || linkingSetContains(sessions, state.SessionID) {
			continue
		}
		if state.PendingCondensationID() == checkpointID {
			sessions = append(sessions, state)
		}
	}
	return sessions
}

func (s *ManualCommitStrategy) findCommitLinkingSet(ctx context.Context, worktreePath string) (commitLinkingSet, error) {
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		// Identity matching below needs the same listing, so nothing can
		// rescue this; report it to the caller (hooks log and skip).
		return commitLinkingSet{}, err
	}
	sessions, declined := s.findSessionsForWorktreeFromStates(ctx, allStates, worktreePath)
	var ancestryGuest string
	if guest := s.findSessionByCommitAncestry(ctx, allStates); guest != nil {
		ancestryGuest = guest.SessionID
		if !linkingSetContains(sessions, guest.SessionID) {
			sessions = append(sessions, guest)
		}
	}
	if len(declined) > 0 && len(sessions) == 0 && !isGitSequenceOperation(ctx) {
		announceUnlinkedCommit(declined)
	}
	return commitLinkingSet{sessions: sessions, ancestryGuest: ancestryGuest, all: allStates}, nil
}

// announceUnlinkedCommit names the candidate sessions so the user can link the
// commit afterwards. It writes to the controlling terminal because the
// installed hook wrappers discard hook stderr (the old hint was never seen),
// and to stderr for callers outside the wrapper and for tests. The remedy is
// `session attach`; `session adopt` would move a live session out of its
// agent's worktree.
func announceUnlinkedCommit(candidates []*SessionState) {
	var b strings.Builder
	fmt.Fprintf(&b, "[entire] Commit not linked to an agent session: %d sessions in other worktrees could match it, and this worktree has none of its own.\n", len(candidates))
	for _, state := range candidates {
		fmt.Fprintf(&b, "  %s", state.SessionID)
		if state.AgentType != "" {
			fmt.Fprintf(&b, "  (%s)", state.AgentType)
		}
		fmt.Fprintf(&b, "  %s\n", state.WorktreePath)
	}
	b.WriteString("To link this commit to one of them afterwards: entire session attach <session-id>\n")
	notice := b.String()

	fmt.Fprint(stderrWriter, notice)
	if interactive.UnderTest() || !interactive.CanPromptInteractively() {
		return
	}
	tty, err := interactive.OpenPromptTTY()
	if err != nil {
		return
	}
	defer tty.Close()
	fmt.Fprint(tty, "\n"+notice)
}

// rehomeSessionAfterOwnCommit moves a session to the worktree its own agent
// just committed in. A session is homed where its first turn-start hook ran,
// and hooks run where the agent was launched, so an agent started in the main
// checkout that then works in a worktree stayed parent-homed while every
// commit landed elsewhere. A commit whose process descends from the agent is
// the best evidence of where it works, so the home follows it.
//
// Only the ancestry-identified session qualifies (a path-fallback match says
// nothing about where the agent works), only once this commit condensed it,
// and only when the old home holds nothing pending: tracked files, shadow
// steps or task records mean the agent works in two trees, and it stays
// guest-linked. WorktreePath and WorktreeID move together.
func (s *ManualCommitStrategy) rehomeSessionAfterOwnCommit(ctx context.Context, repo *git.Repository, state *SessionState, worktreePath, newHead string, condensed bool, ancestryGuest string) bool {
	if !condensed || ancestryGuest == "" || state.SessionID != ancestryGuest {
		return false
	}
	logCtx := logging.WithComponent(ctx, "checkpoint")
	if worktreePath == "" || isSessionHomeWorktree(worktreePath, state) {
		return false
	}
	if len(state.FilesTouched) > 0 || state.StepCount > 0 || state.HasTaskContent() {
		logging.Debug(logCtx, "post-commit: session committed outside its home worktree but the home holds pending content; staying guest-linked",
			slog.String("session_id", state.SessionID),
			slog.String("home_worktree", state.WorktreePath),
			slog.String("commit_worktree", worktreePath),
			slog.Int("files_touched", len(state.FilesTouched)),
			slog.Int("step_count", state.StepCount))
		return false
	}
	worktreeID, err := paths.GetWorktreeID(worktreePath)
	if err != nil {
		logging.Warn(logCtx, "post-commit: cannot resolve worktree ID for re-homing; staying guest-linked",
			slog.String("session_id", state.SessionID),
			slog.String("commit_worktree", worktreePath),
			slog.String("error", err.Error()))
		return false
	}
	old := state.WorktreePath
	state.WorktreePath = worktreePath
	state.WorktreeID = worktreeID
	state.BaseCommit = newHead
	state.RealignAttributionBase(newHead)
	// Re-baseline untracked files against the new tree (best-effort).
	if untracked, untrackedErr := collectUntrackedFiles(ctx); untrackedErr == nil {
		state.UntrackedFilesAtStart = untracked
	}
	captureSessionBranch(repo, state)
	logging.Info(logCtx, "post-commit: session re-homed to the worktree its agent committed in",
		slog.String("session_id", state.SessionID),
		slog.String("from", old),
		slog.String("to", worktreePath),
		slog.String("worktree_id", worktreeID),
		slog.String("base_commit", truncateHash(newHead)))
	return true
}

func linkingSetContains(states []*SessionState, id string) bool {
	for _, st := range states {
		if st.SessionID == id {
			return true
		}
	}
	return false
}

// findSessionByCommitAncestry attributes the running commit to the session
// whose recorded owner process is an ancestor of this hook process. The
// owner is the proclive.Identity that captureSessionOwner already persists
// on every turn start (SessionState.Owner) — the same fingerprint liveness
// checks use, carrying host, boot, and start-time guards so a recycled PID
// or an identity recorded on another machine can never match. A commit hook
// whose ancestry contains a session's owner was spawned (however indirectly)
// by that session's agent, in whatever worktree the commit happens.
//
// When owners at different depths of the ancestry both match — a nested
// agent and the outer agent that spawned it — the nearest ancestor wins: the
// process closest to the commit is its author. Recency only breaks ties
// within one depth (the same agent process hosting several sessions over its
// lifetime, e.g. after a resume): there the most recently interacting
// session wins — the one whose turn this commit concludes. Imported sessions
// are historical records and adopted-away tombstones belong to another
// store; neither ever matches. Returns nil when no session's owner is in our
// ancestry (the linking set is then the worktree-matched sessions alone) —
// including on platforms where proclive cannot introspect processes
// (Windows), which deliberately fall back to worktree matching rather than
// trusting unverifiable ancestry.
//
// The ancestry is snapshotted once (proclive.CurrentAncestry) and every
// candidate matched in memory: this runs in the commit hook with one state
// file per session in the shared store, so a per-candidate walk would repeat
// the hostname/boot/proc reads dozens of times per commit.
func (s *ManualCommitStrategy) findSessionByCommitAncestry(ctx context.Context, states []*SessionState) *SessionState {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		return nil
	}
	var best *SessionState
	bestDepth := -1
	for _, state := range states {
		// WorktreePath is required here and NOT by caller resolution: a commit
		// is attributed in order to condense and link it, and both need
		// somewhere to do that, while resolving a caller only names a session.
		if state.Owner == nil || state.WorktreePath == "" || state.Kind.IsImported() || state.AdoptedIntoWorktreePath != "" {
			continue
		}
		depth := ancestry.Depth(*state.Owner)
		if depth < 0 {
			continue
		}
		if best == nil || isNearerOwner(depth, bestDepth, state, best) {
			best, bestDepth = state, depth
		}
	}
	if best != nil {
		logging.Debug(logging.WithComponent(ctx, "checkpoint"),
			"commit attributed to session by process ancestry",
			slog.String("session_id", best.SessionID),
			slog.Int("owner_pid", best.Owner.PID),
			slog.String("owner_name", best.Owner.Name),
		)
	}
	return best
}

// isNearerOwner reports whether an owner match at depth beats the incumbent at
// bestDepth. A depth of -1 means the owner could not be placed in our
// ancestry at all. The contract, for any pair of inputs:
//
//   - exactly one placed — the placed one wins, at any depth;
//   - both placed — the nearer wins, and an exact tie goes to the more
//     recently interacting session;
//   - neither placed — the more recently interacting session wins.
//
// Recency therefore only ever breaks a tie between equals, never across
// depths, where the nearer process is the answer whatever the clocks say. Two
// equal placed depths means one agent process hosting several sessions over
// its lifetime, e.g. after a resume. Two unplaced depths means nothing located
// either, and recency is the last thing left — deliberate rather than a
// fallthrough, though a caller that treats the result as an identification
// must not use it (see claimsRuledOut, which is why resolveCallerIdentity
// reports caller-ambiguous in exactly that case).
//
// The one shared piece between commit attribution
// (findSessionByCommitAncestry, above) and caller resolution
// (resolveCallerIdentity in caller_session.go). Their loops differ in what
// they admit as a candidate, so only the comparison is common — an earlier
// revision extracted the whole loop, which stopped paying for itself the
// moment caller resolution merged its ancestry pass into the environment one
// and left the extraction with a single caller. Attribution never passes an
// unplaced depth today; the contract above holds regardless, so a future
// caller that does needs no change here.
func isNearerOwner(depth, bestDepth int, state, best *SessionState) bool {
	if (depth >= 0) != (bestDepth >= 0) {
		return depth >= 0
	}
	if depth >= 0 {
		return depth < bestDepth || (depth == bestDepth && interactedAfter(state, best))
	}
	return interactedAfter(state, best)
}

// isSessionHomeWorktree reports whether worktreePath — the commit's worktree,
// already resolved by the hook entry point — is the one the session is
// recorded in. Worktree-coupled state (BaseCommit, shadow-branch content and
// deletion) may only be mutated from the session's home worktree; a
// guest-linked commit elsewhere condenses and links without moving it. A
// pure comparison by design: an earlier version re-resolved the worktree
// here and read resolution failure as "home", which would have mutated a
// guest session's state in exactly the way the gate exists to prevent.
func isSessionHomeWorktree(worktreePath string, state *SessionState) bool {
	return worktreePath != "" && state.WorktreePath != "" && filepath.Clean(state.WorktreePath) == filepath.Clean(worktreePath)
}

func interactedAfter(a, b *SessionState) bool {
	if a.LastInteractionTime == nil {
		return false
	}
	if b.LastInteractionTime == nil {
		return true
	}
	return a.LastInteractionTime.After(*b.LastInteractionTime)
}
