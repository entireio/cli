package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/logging"
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
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		// Identity matching below needs the same listing, so nothing can
		// rescue this; report it to the caller (hooks log and skip).
		return nil, err
	}
	sessions, ambiguous := s.findSessionsForWorktreeFromStates(ctx, allStates, worktreePath)
	if guest := s.findSessionByCommitAncestry(ctx, allStates); guest != nil && !linkingSetContains(sessions, guest.SessionID) {
		sessions = append(sessions, guest)
	}
	if ambiguous && len(sessions) == 0 && !isGitSequenceOperation(ctx) {
		fmt.Fprintln(stderrWriter,
			"[entire] Agent sessions in several other worktrees could match this commit; none was linked. Run 'entire session adopt' in this worktree to link future commits to your session.")
	}
	return sessions, nil
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
		if state.WorktreePath == "" || state.Kind.IsImported() || state.AdoptedIntoWorktreePath != "" || state.Owner == nil {
			continue
		}
		depth := ancestry.Depth(*state.Owner)
		if depth < 0 {
			continue
		}
		if best == nil || depth < bestDepth || (depth == bestDepth && interactedAfter(state, best)) {
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
