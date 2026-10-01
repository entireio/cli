package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
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
	if guest := s.findSessionByCommitAncestry(ctx, allStates, agent.CallerSessionCandidates()); guest != nil && !linkingSetContains(sessions, guest.SessionID) {
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
// Several sessions can match, and pickOwner decides between them: the nearest
// owner, then the sessions one of the claims names, then those that have not
// ended. The claims are the agents' caller-session variables, and they only
// ever choose among sessions ancestry has already placed: a process can carry
// a variable without being run by that agent, so a claim alone links nothing.
// When sessions stay tied — Codex runs its TUI sessions in one app-server
// daemon, so they share one owner — the match declines and the linking set is
// the worktree-matched sessions alone. Imported sessions are historical
// records and adopted-away tombstones belong to another store; neither ever
// matches. Returns nil when no session's owner is in our ancestry — including
// on platforms where proclive cannot introspect processes (Windows), which
// deliberately fall back to worktree matching rather than trusting
// unverifiable ancestry.
//
// The ancestry is snapshotted once (proclive.CurrentAncestry) and every
// candidate matched in memory: this runs in the commit hook with one state
// file per session in the shared store, so a per-candidate walk would repeat
// the hostname/boot/proc reads dozens of times per commit.
func (s *ManualCommitStrategy) findSessionByCommitAncestry(ctx context.Context, states []*SessionState, claims []agent.CallerSessionCandidate) *SessionState {
	ancestry, ok := proclive.CurrentAncestry()
	if !ok {
		return nil
	}
	claimed := make(map[string]bool, len(claims))
	for _, claim := range claims {
		claimed[claim.SessionID] = true
	}
	var candidates []ownerCandidate
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
		candidates = append(candidates, ownerCandidate{state: state, depth: depth, envNamed: claimed[state.SessionID]})
	}
	if len(candidates) == 0 {
		return nil
	}
	best, decided := pickOwner(candidates)
	logCtx := logging.WithComponent(ctx, "checkpoint")
	if !decided {
		logging.Debug(logCtx,
			"commit ancestry cannot tell apart sessions of one owner; linking worktree matches only",
			slog.Int("candidates", len(candidates)),
			slog.Int("owner_pid", best.state.Owner.PID),
			slog.String("owner_name", best.state.Owner.Name),
		)
		return nil
	}
	logging.Debug(logCtx,
		"commit attributed to session by process ancestry",
		slog.String("session_id", best.state.SessionID),
		slog.Int("owner_pid", best.state.Owner.PID),
		slog.String("owner_name", best.state.Owner.Name),
		slog.Bool("env_named", best.envNamed),
	)
	return best.state
}

// ownerCandidate is a session that might be running this process, with the
// evidence for it: how near its recorded owner process is in our ancestry (-1
// when it could not be placed there at all) and whether the environment named
// it.
type ownerCandidate struct {
	state    *SessionState
	depth    int
	envNamed bool
}

// pickOwner decides which candidate is running this process. It is the rule
// commit attribution (findSessionByCommitAncestry, above) and caller
// resolution (resolveCallerIdentity in caller_session.go) share; they differ
// only in what they admit as a candidate. Each step narrows the candidates
// the previous one left:
//
//  1. The nearest owner wins, and a placed owner beats an unplaced one at any
//     depth: the process closest to us is our author, a nested agent rather
//     than the outer agent that spawned it.
//  2. Among the nearest, the sessions the environment named win. The variable
//     names the session whose command is running, which is what ancestry
//     cannot tell apart when several sessions share one owner process.
//  3. Among those, the sessions that have not ended win. Ended comes from the
//     session's phase, never from owner liveness or recency: sessions that
//     share an owner share its liveness, and a session that ended while
//     another was mid-turn is the more recent of the two.
//
// decided is true when one candidate is left. Otherwise several sessions
// share the nearest owner and nothing tells them apart — the ordinary shape
// under the Codex app-server daemon, which hosts Codex's TUI sessions, when
// the variable is missing. best is then the most recently interacting of
// them: a guess caller resolution reports as one and commit attribution
// discards, since under a shared owner recency names whichever session last
// crossed a turn boundary, in whatever worktree.
//
// candidates must not be empty.
func pickOwner(candidates []ownerCandidate) (best ownerCandidate, decided bool) {
	var tied []ownerCandidate
	for _, c := range candidates {
		switch {
		case len(tied) == 0 || nearerOwner(c.depth, tied[0].depth):
			tied = append(tied[:0], c)
		case c.depth == tied[0].depth:
			tied = append(tied, c)
		}
	}
	if named := candidatesWhere(tied, func(c ownerCandidate) bool { return c.envNamed }); len(named) > 0 {
		tied = named
	}
	if live := candidatesWhere(tied, func(c ownerCandidate) bool { return !c.state.IsEnded() }); len(live) > 0 {
		tied = live
	}
	best = tied[0]
	for _, c := range tied[1:] {
		if interactedAfter(c.state, best.state) {
			best = c
		}
	}
	return best, len(tied) == 1
}

// nearerOwner reports whether an owner at depth is nearer than one at other.
// A depth of -1 means the owner could not be placed in our ancestry at all,
// which is farther than any placed depth and level with another unplaced one.
func nearerOwner(depth, other int) bool {
	if (depth >= 0) != (other >= 0) {
		return depth >= 0
	}
	return depth < other
}

func candidatesWhere(candidates []ownerCandidate, keep func(ownerCandidate) bool) []ownerCandidate {
	var kept []ownerCandidate
	for _, c := range candidates {
		if keep(c) {
			kept = append(kept, c)
		}
	}
	return kept
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
