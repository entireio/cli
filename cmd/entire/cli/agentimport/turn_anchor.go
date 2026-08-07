package agentimport

import (
	"context"
	"regexp"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// shaCandidatePattern enforces the "candidates are SHAs" contract: a hex
// string of plausible short-to-full sha length. Rejects revision syntax like
// "HEAD" or "HEAD~2" from ever reaching ResolveRevision, where it would
// otherwise resolve as a ref/expression rather than a commit sha. It does NOT
// stop a hex-named ref: a branch or tag literally named e.g. "beef" still
// resolves as that ref before a commit sha would. That's accepted here — the
// reachability gate below still bounds the result to scanned history, and
// this anchor is display-only.
var shaCandidatePattern = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// Ancestor-walk bounds. Candidates come from transcripts at most one lookback
// window old, so any anchorable commit is recent; walking further buys
// nothing. ancestorWalkSlack absorbs committer-clock skew and rebases that
// backdate commits. The commit cap is a backstop for repos with pathological
// committer dates (the date cutoff can't be trusted to trigger there) and
// bounds both walk time and ancestor-set memory outright.
const (
	ancestorWalkSlack      = 60 * 24 * time.Hour
	ancestorWalkMaxCommits = 50_000
)

// turnAnchorResolver picks the commit_sha anchor for each imported turn in one
// Run: the LAST candidate (transcript order — the turn's end state) that both
// resolves in the repo and passes the reachability gate, else fallback itself.
// fallback is the caller-resolved default-branch tip (Options.LinkCommitSHA);
// ancestry against it doubles as the reachability check, so this needs no
// branch-name logic. Candidates are abbreviated commit SHAs recorded by the
// turn's transcript; squash-merged or rebased-away commits simply fail to
// resolve or fail the gate and fall through, as does any candidate that isn't
// a hex sha (e.g. revision syntax like "HEAD"). Ambiguous short SHAs are NOT
// detected — go-git's ResolveRevision resolves them to an arbitrary matching
// commit rather than erroring; the reachability gate bounds the resulting
// damage to scanned history, never outside it.
//
// extraAccept widens that gate with the reconcile scan's commit set (see
// collectCommitsMissingSessionData). Membership is itself the reachability
// proof: those commits were reached by walking the scan tips, so a
// rebased-away SHA — which still resolves as an object — is absent from the
// set and correctly falls through. Nil (the default) leaves behavior
// identical to ancestry-only resolution.
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
	// extraAccept is the reconcile scan's commit set; nil when not reconciling.
	extraAccept map[plumbing.Hash]*CommitRecord
}

// newTurnAnchorResolver builds a resolver for one Run. It does no repo work
// until resolve is first called with a non-empty candidate list. fallback
// must be a full hex sha when non-empty — resolveImportLinkCommitSHA
// guarantees this; a short fallback would silently degrade to the empty
// ancestor-set path. now anchors the walk's date cutoff (Options.Now; zero
// falls back to the wall clock), and lookbackDays is the run's effective
// lookback window (0 falls back to DefaultLookbackDays) — the walk must reach
// at least as far back as the transcripts being imported.
func newTurnAnchorResolver(repo *git.Repository, fallback string, now time.Time, lookbackDays int) *turnAnchorResolver {
	if now.IsZero() {
		now = time.Now()
	}
	if lookbackDays <= 0 {
		lookbackDays = DefaultLookbackDays
	}
	return &turnAnchorResolver{
		repo:     repo,
		fallback: fallback,
		cutoff:   now.Add(-(time.Duration(lookbackDays)*24*time.Hour + ancestorWalkSlack)),
		maxWalk:  ancestorWalkMaxCommits,
	}
}

// resolve returns the anchor for one turn's candidates and how it was derived:
// cp.CommitSHAMethodRecorded when a transcript-recorded candidate matched (so
// the link is commit-accurate), else cp.CommitSHAMethodFallback. An empty
// anchor — the unanchorable-repo case, matching resolveImportLinkCommitSHA's
// contract — reports an empty method, because there is no link to describe.
func (r *turnAnchorResolver) resolve(ctx context.Context, candidates []string) (anchor, method string) {
	// With no candidates, or nothing to check them against, there is nothing to
	// resolve. An empty fallback normally means an unanchorable repo, but a
	// non-empty scan set can still gate a candidate, so it is not on its own a
	// reason to give up.
	if len(candidates) == 0 || (r.fallback == "" && len(r.extraAccept) == 0) {
		return r.fallback, r.fallbackMethod()
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
		if r.accepts(*hash) {
			return hash.String(), cp.CommitSHAMethodRecorded
		}
	}
	return r.fallback, r.fallbackMethod()
}

// accepts reports whether a resolved candidate passes the reachability gate:
// reachable from the fallback tip, or present in the reconcile scan's set.
func (r *turnAnchorResolver) accepts(hash plumbing.Hash) bool {
	if _, ok := r.ancestors[hash]; ok {
		return true
	}
	_, ok := r.extraAccept[hash]
	return ok
}

// fallbackAnchor returns the run's display anchor and its method, for callers
// that decided against a candidate link after the fact (an unaccepted
// heuristic match) and must fall back to what a plain import would have
// written.
func (r *turnAnchorResolver) fallbackAnchor() (anchor, method string) {
	return r.fallback, r.fallbackMethod()
}

// fallbackMethod labels the fallback anchor, or reports no method at all when
// there is no anchor to describe.
func (r *turnAnchorResolver) fallbackMethod() string {
	if r.fallback == "" {
		return ""
	}
	return cp.CommitSHAMethodFallback
}

// buildAncestors walks fallback's history once, newest-first, collecting
// reachable commit hashes (including fallback itself — a commit is its own
// ancestor, matching go-git's IsAncestor semantics) until the date cutoff or
// commit cap stops it. If the fallback commit doesn't resolve or the walk
// fails, it returns an empty (or partial) set — every candidate not already
// collected then falls through to the fallback in resolve. Failure paths are
// logged at Debug: import decisions are one-shot (a re-run skips
// already-imported turns), so an unlogged failure here destroys the only
// evidence of why every turn in this run anchored to the fallback instead of
// its recorded commit.
func (r *turnAnchorResolver) buildAncestors(ctx context.Context) map[plumbing.Hash]struct{} {
	ancestors := make(map[plumbing.Hash]struct{})
	if r.fallback == "" {
		return ancestors // nothing to walk from; the scan set is the only gate
	}
	err := walkRecentCommits(r.repo, plumbing.NewHash(r.fallback), r.cutoff, r.maxWalk, func(c *object.Commit) {
		ancestors[c.Hash] = struct{}{}
	})
	if err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk truncated or unavailable",
			"fallback", r.fallback, "ancestors_collected", len(ancestors), "error", err.Error())
	}
	return ancestors
}

// walkRecentCommits visits history from tip newest-first (committer-time
// order), calling visit for each commit until it reaches one older than cutoff
// or has visited maxWalk commits. Both the anchor resolver's ancestor set and
// the reconcile scan are built on it, so their bounds — and the early-stop
// reasoning below — stay identical by construction.
//
// Committer-time ordering is what makes early-stopping sound: with newest-first
// emission, the first commit older than the cutoff means everything after it is
// older too (modulo clock skew, absorbed by ancestorWalkSlack). A depth-first
// walk could not stop early without cutting off recent commits on unvisited
// merge branches.
//
// An unresolvable tip or a mid-walk failure is returned to the caller, which
// keeps whatever was collected so far: a partial set degrades to fewer links,
// never to wrong ones.
func walkRecentCommits(repo *git.Repository, tip plumbing.Hash, cutoff time.Time, maxWalk int, visit func(*object.Commit)) error {
	iter, err := repo.Log(&git.LogOptions{From: tip, Order: git.LogOrderCommitterTime})
	if err != nil {
		return err //nolint:wrapcheck // caller logs with its own context; wrapping adds nothing
	}
	defer iter.Close()
	visited := 0
	if err := iter.ForEach(func(c *object.Commit) error {
		if visited >= maxWalk || c.Committer.When.Before(cutoff) {
			return storer.ErrStop
		}
		visited++
		visit(c)
		return nil
	}); err != nil {
		return err //nolint:wrapcheck // caller logs with its own context; wrapping adds nothing
	}
	return nil
}
