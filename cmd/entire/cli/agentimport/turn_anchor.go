package agentimport

import (
	"context"
	"regexp"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// shaCandidatePattern enforces the "candidates are SHAs" contract: a hex
// string of plausible short-to-full sha length. Rejects revision syntax like
// "HEAD" or "HEAD~2" from ever reaching ResolveRevision, where it would
// otherwise resolve as a ref/expression rather than a commit sha. It does NOT
// stop a hex-named ref: a branch or tag literally named e.g. "beef" still
// resolves as that ref before a commit sha would. That's accepted here — the
// ancestry gate below still bounds the result to default-branch history, and
// this anchor is display-only.
var shaCandidatePattern = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// Ancestor-walk bounds. Candidates come from transcripts at most LookbackDays
// old, so any anchorable commit is recent; walking further buys nothing.
// ancestorWalkSlack absorbs committer-clock skew and rebases that backdate
// commits. The commit cap is a backstop for repos with pathological committer
// dates (the date cutoff can't be trusted to trigger there) and bounds both
// walk time and ancestor-set memory outright.
const (
	ancestorWalkSlack      = 60 * 24 * time.Hour
	ancestorWalkMaxCommits = 50_000
)

// turnAnchorResolver picks the commit_sha anchor for each imported turn in one
// Run: the LAST candidate (transcript order — the turn's end state) that both
// resolves in the repo and is an ancestor of fallback, else fallback itself.
// fallback is the caller-resolved default-branch tip (Options.LinkCommitSHA);
// ancestry against it doubles as the reachability check, so this needs no
// branch-name logic. Candidates are abbreviated commit SHAs recorded by the
// turn's transcript; squash-merged or rebased-away commits simply fail to
// resolve or fail ancestry and fall through, as does any candidate that isn't
// a hex sha (e.g. revision syntax like "HEAD"). Ambiguous short SHAs are NOT
// detected — go-git's ResolveRevision resolves them to an arbitrary matching
// commit rather than erroring; the ancestry gate bounds the resulting damage
// to mis-anchoring within default-branch history, never outside it.
//
// The fallback's ancestor set is walked and memoized once, lazily, on the
// first turn that actually carries a candidate. The walk is bounded — it
// emits newest-first (committer-time order) and stops at commits older than
// the lookback window plus slack, or at ancestorWalkMaxCommits — so both walk
// time and set memory stay capped on huge histories. A commit beyond either
// bound misses the set and its turn falls back, which is consistent: no
// importable turn can reference a commit that old. Turns/sessions with no
// recorded commits (the common case for older transcripts) never trigger the
// walk. Not safe for concurrent use — Run calls resolve from a single
// goroutine.
type turnAnchorResolver struct {
	repo      *git.Repository
	fallback  string
	cutoff    time.Time                  // commits with committer time before this are not collected
	maxWalk   int                        // hard cap on commits visited (overridable in tests)
	ancestors map[plumbing.Hash]struct{} // nil until first candidate-bearing call
}

// newTurnAnchorResolver builds a resolver for one Run. It does no repo work
// until resolve is first called with a non-empty candidate list. fallback
// must be a full hex sha when non-empty — resolveImportLinkCommitSHA
// guarantees this; a short fallback would silently degrade to the empty
// ancestor-set path. now anchors the walk's date cutoff (Options.Now; zero
// falls back to the wall clock).
func newTurnAnchorResolver(repo *git.Repository, fallback string, now time.Time) *turnAnchorResolver {
	if now.IsZero() {
		now = time.Now()
	}
	return &turnAnchorResolver{
		repo:     repo,
		fallback: fallback,
		cutoff:   now.Add(-(time.Duration(LookbackDays)*24*time.Hour + ancestorWalkSlack)),
		maxWalk:  ancestorWalkMaxCommits,
	}
}

// resolve returns the anchor for one turn's candidates and whether it came
// from a candidate (as opposed to the fallback — callers use this to log
// genuine fallbacks without misreporting the turn whose recorded commit IS
// the fallback tip). Empty fallback or no candidates return fallback
// unchanged (empty fallback → "" — unanchorable repo imports unlinked,
// matching resolveImportLinkCommitSHA's contract).
func (r *turnAnchorResolver) resolve(ctx context.Context, candidates []string) (anchor string, fromCandidate bool) {
	if r.fallback == "" || len(candidates) == 0 {
		return r.fallback, false
	}
	if r.ancestors == nil {
		r.ancestors = r.buildAncestors(ctx)
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if !shaCandidatePattern.MatchString(c) {
			continue
		}
		hash, err := r.repo.ResolveRevision(plumbing.Revision(c))
		if err != nil || hash == nil {
			continue
		}
		if _, ok := r.ancestors[*hash]; ok {
			return hash.String(), true
		}
	}
	return r.fallback, false
}

// buildAncestors walks fallback's history once, newest-first, collecting
// reachable commit hashes (including fallback itself — a commit is its own
// ancestor, matching go-git's IsAncestor semantics) until the date cutoff or
// commit cap stops it. Committer-time ordering is what makes early-stopping
// sound: with newest-first emission, the first commit older than the cutoff
// means everything after it is older too (modulo clock skew, absorbed by
// ancestorWalkSlack) — a depth-first walk could not stop early without
// cutting off recent commits on unvisited merge branches. If the fallback
// commit doesn't resolve or the walk fails, it returns an empty (or partial)
// set — every candidate not already collected then falls through to the
// fallback in resolve. Failure paths are logged at Debug: import decisions
// are one-shot (a re-run skips already-imported turns), so an unlogged
// failure here destroys the only evidence of why every turn in this run
// anchored to the fallback instead of its recorded commit.
func (r *turnAnchorResolver) buildAncestors(ctx context.Context) map[plumbing.Hash]struct{} {
	ancestors := make(map[plumbing.Hash]struct{})
	iter, err := r.repo.Log(&git.LogOptions{
		From:  plumbing.NewHash(r.fallback),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk unavailable, all turns fall back",
			"fallback", r.fallback, "error", err.Error())
		return ancestors
	}
	defer iter.Close()
	if err := iter.ForEach(func(c *object.Commit) error {
		if len(ancestors) >= r.maxWalk || c.Committer.When.Before(r.cutoff) {
			return storer.ErrStop
		}
		ancestors[c.Hash] = struct{}{}
		return nil
	}); err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk truncated",
			"fallback", r.fallback, "ancestors_collected", len(ancestors), "error", err.Error())
		return ancestors
	}
	return ancestors
}
