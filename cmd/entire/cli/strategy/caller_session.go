package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// SessionResolution says how a session was arrived at, so a caller can tell an
// exact answer from a guess.
//
// This exists because the guess is genuinely useful to a human — someone who
// asks which session is current in a terminal means "the one that has been
// working here" — and genuinely dangerous to an agent, which means "mine" and
// may go on to act on the answer. One command can serve both only if it says
// which question it answered.
type SessionResolution string

const (
	// ResolutionCallerEnv means an agent published this session's ID into our
	// environment and nothing nearer in our process ancestry contradicted it.
	ResolutionCallerEnv SessionResolution = "caller-env"

	// ResolutionAncestry means this session's recorded owner process is the
	// nearest such ancestor of ours — its agent spawned us, however
	// indirectly — and no environment variable named it.
	//
	// This outranks a bare environment claim rather than backing it up, which
	// is the opposite of what it looks like it should do. An environment
	// variable says only that some ancestor published it, not that the
	// publisher is our caller: agents nest, and an inner agent that publishes
	// nothing of its own (Gemini CLI, opencode) passes the OUTER agent's
	// variable straight through to us. Depth is the only signal that
	// distinguishes "Codex ran me" from "Codex ran Gemini ran me", so the
	// nearest owner wins wherever ancestry can rank at all.
	ResolutionAncestry SessionResolution = "ancestry"

	// ResolutionCallerAmbiguous means we are demonstrably inside an agent
	// session but cannot say which: several agents claim us and nothing
	// ordered their claims — no owner recorded on any of them, or a platform
	// that cannot introspect processes. The reported session is the most
	// plausible of them and explicitly a guess, so this does NOT satisfy
	// IsCaller.
	//
	// Reported rather than resolved on purpose. Picking one and calling it
	// "your own session" is the failure this whole type exists to prevent, and
	// registry order — which is what an arbitrary pick amounts to — carries no
	// information about who invoked the command.
	ResolutionCallerAmbiguous SessionResolution = "caller-ambiguous"

	// ResolutionWorktree means nothing identified the caller, and this is the
	// most recently active session recorded in the current worktree.
	ResolutionWorktree SessionResolution = "worktree"

	// ResolutionOtherWorktree means the current worktree has no sessions at
	// all, and this is the most recently active session from somewhere else in
	// the shared store. Correct for "what has been happening in this repo",
	// never evidence about the caller — worktrees share one session store, so
	// this can be any of dozens of unrelated sessions.
	ResolutionOtherWorktree SessionResolution = "other-worktree"

	// ResolutionNone means no session was found by any tier.
	ResolutionNone SessionResolution = ""
)

// IsCaller reports whether the resolution actually identified the calling
// process's own session, rather than inferring or guessing a plausible one.
// Gate anything that acts on a session — as opposed to merely displaying it —
// on this.
//
// ResolutionCallerAmbiguous is deliberately excluded even though it means "you
// are definitely inside an agent session": knowing that is not knowing which.
func (r SessionResolution) IsCaller() bool {
	return r == ResolutionCallerEnv || r == ResolutionAncestry
}

// ResolvedSession is a session plus the provenance of how it was picked.
type ResolvedSession struct {
	SessionID  string
	Resolution SessionResolution

	// AgentType names the agent the session belongs to, on any resolution the
	// identification tier produced — from the environment when a claim named
	// the session, otherwise from the session's own state. Set even when
	// Tracked is false, which is the case it exists for: the environment
	// identifies the agent before any state has been loaded, and "you are
	// inside a Codex session Entire is not recording" is the whole diagnosis.
	// Empty on the worktree and other-worktree tiers, which report a session
	// rather than a caller.
	AgentType types.AgentType

	// Tracked reports whether Entire holds session state for SessionID.
	//
	// False only when the environment named a session Entire has not
	// recorded — hooks not installed, hooks failed, or the first turn not yet
	// landed, since state is created at turn start. That reaches the caller as
	// ResolutionCallerEnv for a single claim or ResolutionCallerAmbiguous for
	// several, so do NOT test it by comparing Resolution; test this field.
	//
	// A diagnosis rather than an error: the caller is genuinely in a session,
	// and Entire is genuinely not tracking it. Load-bearing for exactly that
	// reason — `session current` and `session tokens` branch to their
	// untracked diagnostic on it — so resolveUntrackedClaims assigns it
	// explicitly rather than leaning on the zero value, matching the
	// `Tracked: true` its sibling paths write.
	Tracked bool

	// Incomplete is non-nil when the candidate set this resolution was
	// computed from was known to be missing entries — the session store could
	// not be read at all, or an individual state file in it could not — and
	// says which.
	//
	// A resolution is still returned, because a partial set still identifies
	// the caller in the ordinary case and answering nothing at all would break
	// `session current` on a repository with one corrupt file. What a partial
	// set cannot support is a NEGATIVE: "no session nearer than this one
	// exists" is precisely the claim a missing candidate invalidates, and that
	// claim is what IsCaller is relied on for. So anything that ACTS on a
	// session gates on this IN ADDITION to IsCaller — the two answer different
	// questions and neither implies the other.
	//
	// Deliberately not folded into IsCaller: the resolution is still the best
	// available answer and still worth displaying, labelled. Only mutation has
	// to be conservative here.
	Incomplete error
}

// Found reports whether any session was resolved.
func (r ResolvedSession) Found() bool { return r.SessionID != "" }

// ResolveCallerSession answers "which session is running this process?",
// falling back to "which session is current here?" when nothing can identify
// the caller. Tiers, strongest first:
//
//  1. Identification — the caller's own environment, its process ancestry, or
//     both, ranked together by how near the owning process is
//     (resolveCallerIdentity). Reports caller-env, ancestry, or
//     caller-ambiguous.
//  2. The most recently active session recorded in this worktree.
//  3. The most recently active session anywhere in the shared store.
//
// Every tier is reported through Resolution, and the weak ones are not
// silently interchangeable with identification — which is the whole point.
// `entire session current` used to collapse tiers 2 and 3 and report either as
// "the active session for the current worktree", so in a worktree with no
// sessions of its own it returned a live session belonging to a different
// worktree, indistinguishably from the real thing. An agent that read that ID
// back and acted on it operated on someone else's session.
//
// Environment and ancestry are ONE tier rather than two, ordered internally.
// An earlier revision tried the environment first and returned on any hit,
// which is wrong for the nesting case: an inner agent that publishes no ID of
// its own passes the outer agent's variable through, so the lone claim named
// the outer session while the inner one sat in our ancestry one hop away. See
// ResolutionAncestry.
func ResolveCallerSession(ctx context.Context) ResolvedSession {
	states, skipped, err := ListSessionStatesWithSkipped(ctx)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"),
			"caller session resolution: cannot list session states",
			slog.String("error", err.Error()))
		states = nil
	}

	resolved := resolveSessionTiers(ctx, states)
	resolved.Incomplete = incompleteCandidates(err, skipped)
	return resolved
}

// resolveSessionTiers is the tier chain itself, with no completeness
// bookkeeping. Split out so each entry point stamps
// ResolvedSession.Incomplete in exactly one place: that field is what stops a
// mutating caller acting on a partial candidate set, and a tier added later
// must not be able to return without it.
func resolveSessionTiers(ctx context.Context, states []*SessionState) ResolvedSession {
	if resolved, ok := resolveCallerIdentity(ctx, states); ok {
		return resolved
	}
	if id := mostRecentSessionID(sessionStatesForCurrentWorktree(ctx, states)); id != "" {
		return ResolvedSession{SessionID: id, Resolution: ResolutionWorktree, Tracked: true}
	}
	if id := mostRecentSessionID(states); id != "" {
		return ResolvedSession{SessionID: id, Resolution: ResolutionOtherWorktree, Tracked: true}
	}
	return ResolvedSession{Resolution: ResolutionNone}
}

// incompleteCandidates renders what a listing failed to see, or nil when it
// saw everything.
//
// The two inputs are not redundant, and the second is the one that matters. A
// store that cannot be opened at all is loud — every later read of it fails
// the same way, so a command built on it stops on its own — whereas a single
// unreadable state file leaves the listing SUCCEEDING with one candidate
// quietly missing, which no error channel reports. Measured: one state file at
// mode 000 had `entire session list` print "No sessions." over a store that
// held one, and had the adopt ownership guard authorize against a set the
// rival session had dropped out of.
func incompleteCandidates(listErr error, skipped []session.SkippedState) error {
	switch {
	case listErr != nil:
		return fmt.Errorf("this repository's session state could not be read: %w", listErr)
	case len(skipped) == 1:
		return errors.New("a session state file in this repository could not be read")
	case len(skipped) > 1:
		return fmt.Errorf("%d session state files in this repository could not be read", len(skipped))
	default:
		return nil
	}
}

// IdentifyCallerSession runs ONLY the identification tier, over this
// repository's session states plus extra.
//
// Exported for `session adopt`, which must weigh another repository's sessions
// as FIRST-CLASS candidates rather than as a fallback consulted after this
// resolver has already answered. Using them as a fallback was a hole: an inner
// agent that publishes no session ID forwards the OUTER agent's variable, so
// the environment named the outer session, the caller matched it, and the
// nearer inner owner sitting in the source store was never examined at all.
// Ranking every candidate together is the only formulation that cannot be
// short-circuited that way, and it is why adopt needs no ownership rule of its
// own — the policy here is the whole policy.
//
// The worktree and other-worktree tiers are deliberately absent. They answer
// "which session is current here?", which nothing acting on a session may use
// — and feeding another repository's states into them would let the weakest
// tier return a session from a different repository entirely. Callers that
// want those tiers want ResolveCallerSession.
//
// ok is false only when nothing claimed us and no owner placed us, which is
// the caller's cue that identification is unavailable rather than negative.
func IdentifyCallerSession(ctx context.Context, extra []*SessionState) (ResolvedSession, bool) {
	states, skipped, err := ListSessionStatesWithSkipped(ctx)
	if err != nil {
		logging.Debug(logging.WithComponent(ctx, "session"),
			"caller session identification: cannot list session states",
			slog.String("error", err.Error()))
		states = nil
	}

	resolved, ok := resolveCallerIdentity(ctx, mergeSessionStates(states, extra))
	resolved.Incomplete = incompleteCandidates(err, skipped)
	return resolved, ok
}

// mergeSessionStates combines this repository's listing with states a caller
// supplied, and **the supplied state wins an ID collision**.
//
// Deduplication is not cosmetic: a same-store adoption (linked worktrees share
// one session store) hands over a listing identical to this repository's own,
// and a session counted twice would be weighed against itself.
//
// The precedence is not cosmetic either, and it is the opposite of what
// keeping the local listing first would give you. Colliding IDs are a
// SUPPORTED condition rather than a curiosity: `session adopt --force` exists
// precisely to replace a state the target repository already holds for the
// session being adopted, so on that path the local copy is by definition the
// stale one and the supplied copy is the live session. Preferring the local
// copy discarded the caller's better evidence in both directions — a
// caller-owned source state shadowed by an ownerless leftover refused a
// legitimate adoption, and a leftover that still carried an owner from a
// previous adoption could authorize on evidence about a state nobody was
// adopting.
//
// Stated generally, because a future caller will not be adopt: a caller that
// hands over states has chosen them deliberately and knows more about them
// than a directory listing does. Merging the two copies' evidence instead —
// taking whichever has an owner, say — would be worse, since that is exactly
// how a stale owner record gets combined with a live session.
//
// Result order is local-listing order with supplied copies substituted in
// place, which no consumer depends on: resolveCallerIdentity ranks candidates
// rather than trusting their order, and resolveUntrackedClaims orders by the
// agent registry, not by this slice.
func mergeSessionStates(states, extra []*SessionState) []*SessionState {
	if len(extra) == 0 {
		return states
	}
	supplied := make(map[string]*SessionState, len(extra))
	for _, state := range extra {
		if state != nil {
			supplied[state.SessionID] = state
		}
	}

	merged := make([]*SessionState, 0, len(states)+len(extra))
	seen := make(map[string]struct{}, len(states)+len(extra))
	take := func(state *SessionState) {
		if state == nil {
			return
		}
		if _, dup := seen[state.SessionID]; dup {
			return
		}
		seen[state.SessionID] = struct{}{}
		merged = append(merged, state)
	}
	for _, state := range states {
		if state == nil {
			continue
		}
		if replacement, ok := supplied[state.SessionID]; ok {
			take(replacement)
			continue
		}
		take(state)
	}
	for _, state := range extra {
		take(state)
	}
	return merged
}

// callerCandidate is one session that might be running us, with the evidence
// for it: whether the environment named it, and how near its owner process is
// in our ancestry (-1 when it could not be placed there at all).
type callerCandidate struct {
	state     *SessionState
	agentType types.AgentType
	envNamed  bool
	depth     int
}

// resolveCallerIdentity identifies the session running this process from the
// environment and the process tree together, or reports that it cannot.
//
// Both signals are partial in opposite ways, which is why neither can be a
// separate tier that short-circuits the other. The environment is a positive
// statement by a vendor, but only that SOME ancestor published it — an inner
// agent publishing nothing forwards the outer agent's variable verbatim.
// Ancestry says which owner is nearest, but only for sessions that recorded an
// owner (turn start), and not at all on a platform that cannot introspect
// processes. So candidates are gathered from either signal and ranked by
// depth, with the environment breaking ties depth cannot.
func resolveCallerIdentity(ctx context.Context, states []*SessionState) (ResolvedSession, bool) {
	claims := agent.CallerSessionCandidates()
	claimedBy := make(map[string]types.AgentType, len(claims))
	for _, claim := range claims {
		claimedBy[claim.SessionID] = claim.AgentType
	}

	// Needed whenever anything might be ranked, which is any time we have
	// either a claim or a state that could carry an owner. Snapshot once:
	// each call walks the process tree and reads the hostname and boot ID.
	var ancestry proclive.Ancestry
	haveAncestry := false
	if len(claims) > 0 || len(states) > 0 {
		ancestry, haveAncestry = proclive.CurrentAncestry()
	}

	var candidates []callerCandidate
	placedClaims := make(map[string]bool, len(claimedBy))
	for _, state := range states {
		if state.Kind.IsImported() || state.AdoptedIntoWorktreePath != "" {
			continue
		}
		agentType, envNamed := claimedBy[state.SessionID]
		depth := -1
		if haveAncestry && state.Owner != nil {
			depth = ancestry.Depth(*state.Owner)
		}
		if !envNamed && depth < 0 {
			continue
		}
		if envNamed && depth >= 0 {
			placedClaims[state.SessionID] = true
		}
		if !envNamed {
			agentType = state.AgentType
		}
		candidates = append(candidates, callerCandidate{
			state: state, agentType: agentType, envNamed: envNamed, depth: depth,
		})
	}

	if len(candidates) == 0 {
		return resolveUntrackedClaims(ctx, claims)
	}

	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if isNearerOwner(candidate.depth, best.depth, candidate.state, best.state) {
			best = candidate
		}
	}

	resolution := ResolutionCallerEnv
	switch {
	case !claimsRuledOut(claimedBy, placedClaims, best.state.SessionID, best.depth):
		resolution = ResolutionCallerAmbiguous
	case !best.envNamed:
		resolution = ResolutionAncestry
	}

	resolved := ResolvedSession{
		SessionID:  best.state.SessionID,
		Resolution: resolution,
		AgentType:  best.agentType,
		Tracked:    true,
	}
	logging.Debug(logging.WithComponent(ctx, "session"),
		"caller session identified",
		slog.String("session_id", resolved.SessionID),
		slog.String("resolution", string(resolution)),
		slog.String("agent_type", string(resolved.AgentType)),
		slog.Int("candidates", len(candidates)),
		slog.Int("env_claims", len(claimedBy)),
		slog.Int("placed_claims", len(placedClaims)),
		slog.Int("ancestry_depth", best.depth))
	return resolved, true
}

// claimsRuledOut reports whether the winner can be believed: no session the
// environment named could be nearer to us than it is.
//
// A claim reached our environment, so its agent IS somewhere in our ancestry —
// that much is certain. What is unknown is where. A claim we could not place
// (untracked, so no owner to compare; or tracked with no owner recorded yet,
// since the first turn creates it) therefore sits at an unmeasured depth, and
// unmeasured means "possibly nearer than the winner". Two exemptions, and only
// two:
//
// A winner at depth 0 owns our immediate parent process. Nothing can be nearer
// than that, so no unplaced claim can shadow it however many there are. Note
// the exemption claims only that: nothing NEARER. An unplaced claim whose
// owner happens to be that same parent process — one agent process hosting two
// sessions after a resume — is equally near and invisible, which is the
// residual this cannot close. Equal depth is the case recency breaks when both
// sides are placed; here one side cannot be seen at all.
//
// A claim that IS the winner shadows nothing — it is the thing being believed.
// This is the ordinary single-agent shape: one variable, one session, whose
// owner may not be recorded yet.
//
// An earlier revision exempted "exactly one claim" instead, reasoning that a
// lone claim has nothing to be nearer than. That is true of a lone claim that
// WINS, and the revision quietly assumed the two were the same thing. They are
// not: a claim with no state cannot enter the ranking at all, so a tracked
// session found purely by ancestry won instead and was reported as the
// identified caller while the claim that named itself in our own environment
// went unexamined. Do not reintroduce a count-based exemption; the question is
// about the winner, not about how many claims there are.
func claimsRuledOut(claimedBy map[string]types.AgentType, placed map[string]bool, winnerID string, winnerDepth int) bool {
	if winnerDepth == 0 {
		return true
	}
	for id := range claimedBy {
		if id != winnerID && !placed[id] {
			return false
		}
	}
	return true
}

// resolveUntrackedClaims handles the case where agents claim us but Entire
// holds state for none of them.
//
// The claim is still worth reporting: we are demonstrably inside a session
// Entire is not recording — hooks not installed, hooks failed, or the first
// turn not yet landed (state is created at turn start) — and that is a
// diagnosis, where falling through to the most-recent tiers would answer a
// question nobody asked with an unrelated session.
//
// More than one such claim cannot be ordered at all: with no state there is no
// owner to compare and no interaction time to fall back on, so any pick is
// registry order wearing a disguise. Report it ambiguous.
func resolveUntrackedClaims(ctx context.Context, claims []agent.CallerSessionCandidate) (ResolvedSession, bool) {
	if len(claims) == 0 {
		return ResolvedSession{}, false
	}
	resolution := ResolutionCallerEnv
	if len(claims) > 1 {
		resolution = ResolutionCallerAmbiguous
	}
	resolved := ResolvedSession{
		SessionID:  claims[0].SessionID,
		Resolution: resolution,
		AgentType:  claims[0].AgentType,
		// Explicit, not the zero value: false here is the assertion this
		// whole function makes, and the flag every untracked diagnostic
		// branches on. Its sibling paths spell out Tracked: true.
		Tracked: false,
	}
	logging.Debug(logging.WithComponent(ctx, "session"),
		"caller session claimed but not tracked",
		slog.String("session_id", resolved.SessionID),
		slog.String("resolution", string(resolution)),
		slog.Int("env_claims", len(claims)))
	return resolved, true
}
