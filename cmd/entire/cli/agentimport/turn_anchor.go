package agentimport

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
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
// resolves in the repo and is an ancestor of fallback; failing that, the
// newest ancestor committed by the importer whose committer time falls inside
// one of the turn's commit windows (Turn.CommitWindows); else fallback itself.
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
// Commit windows exist because transcripts record a SHA only when git prints
// one, so a quiet commit leaves no candidate. They rank below candidates: a
// window only shows that some ancestor was committed while the turn's git
// command ran, and the same user may have committed in the same seconds.
//
// The fallback's ancestor set is walked and memoized once, lazily, on the
// first turn that actually carries a candidate or window. The walk is bounded
// — it emits newest-first (committer-time order) and stops at commits older
// than the lookback window plus slack, or at ancestorWalkMaxCommits — so both
// walk time and set memory stay capped on huge histories. A commit beyond
// either bound misses the set and its turn falls back, which is consistent: no
// importable turn can reference a commit that old. Turns with neither a
// candidate nor a window never trigger the walk. Not safe for concurrent use
// — Run calls resolve from a single goroutine.
type turnAnchorResolver struct {
	repo      *git.Repository
	fallback  string
	cutoff    time.Time                  // commits with committer time before this are not collected
	maxWalk   int                        // hard cap on commits visited (overridable in tests)
	ancestors map[plumbing.Hash]struct{} // nil until the first call with a candidate or window
	byTime    []ancestorCommit           // the same commits, newest committer time first
	// committerEmail is the importer's git email. A window only adopts a commit
	// with this committer: the default branch also holds other people's pushed
	// commits, and one can land inside a slow git call (hooks running tests).
	committerEmail string
}

// ancestorCommit is a walked ancestor and its committer time.
type ancestorCommit struct {
	hash  plumbing.Hash
	when  time.Time
	email string // committer email
}

// newTurnAnchorResolver builds a resolver for one Run. It does no repo work
// until resolve is first called with a candidate or window. fallback
// must be a validated full hex sha — Run guarantees this before any writes; a
// short fallback would silently degrade to the empty ancestor-set path. now
// anchors the walk's date cutoff (Options.Now; zero falls back to the wall
// clock).
func newTurnAnchorResolver(repo *git.Repository, fallback, committerEmail string, now time.Time) *turnAnchorResolver {
	if now.IsZero() {
		now = time.Now()
	}
	return &turnAnchorResolver{
		repo:           repo,
		fallback:       fallback,
		committerEmail: committerEmail,
		cutoff:         now.Add(-(time.Duration(LookbackDays)*24*time.Hour + ancestorWalkSlack)),
		maxWalk:        ancestorWalkMaxCommits,
	}
}

// resolve returns the anchor for one turn's candidates and commit windows,
// and whether it came from either (as opposed to the fallback — callers use
// this to log genuine fallbacks without misreporting the turn whose commit IS
// the fallback tip). Empty fallback, or neither candidates nor windows, return
// fallback unchanged. Run rejects an empty fallback before constructing this
// resolver.
func (r *turnAnchorResolver) resolve(ctx context.Context, candidates []string, windows []TimeWindow) (anchor string, fromCandidate bool) {
	if r.fallback == "" || (len(candidates) == 0 && len(windows) == 0) {
		return r.fallback, false
	}
	if r.ancestors == nil {
		r.buildAncestors(ctx)
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
	if hash, ok := r.newestCommittedIn(windows); ok {
		return hash.String(), true
	}
	return r.fallback, false
}

// newestCommittedIn returns the newest ancestor committed by committerEmail
// whose committer time falls inside any of windows. byTime is sorted
// newest-first, so a binary search finds each window's newest commit, and the
// lowest index across windows is the newest overall.
func (r *turnAnchorResolver) newestCommittedIn(windows []TimeWindow) (plumbing.Hash, bool) {
	best := len(r.byTime)
	if r.committerEmail == "" {
		return plumbing.ZeroHash, false
	}
	for _, w := range windows {
		i := sort.Search(len(r.byTime), func(i int) bool { return !r.byTime[i].when.After(w.End) })
		for ; i < best && !r.byTime[i].when.Before(w.Start); i++ {
			if strings.EqualFold(r.byTime[i].email, r.committerEmail) {
				best = i
				break
			}
		}
	}
	if best == len(r.byTime) {
		return plumbing.ZeroHash, false
	}
	return r.byTime[best].hash, true
}

// buildAncestors walks fallback's history once, newest-first, collecting
// reachable commits into ancestors and byTime (including fallback itself — a
// commit is its own ancestor, matching go-git's IsAncestor semantics) until
// the date cutoff or commit cap stops it. Committer-time ordering is what
// makes early-stopping sound: with newest-first emission, the first commit
// older than the cutoff means everything after it is older too (modulo clock
// skew, absorbed by ancestorWalkSlack) — a depth-first walk could not stop
// early without cutting off recent commits on unvisited merge branches. If
// the fallback commit doesn't resolve or the walk fails, it leaves an empty
// (or partial) set — every candidate or window not already collected then
// falls through to the fallback in resolve. Failure paths are logged at
// Debug: import decisions are one-shot (a re-run skips already-imported
// turns), so an unlogged failure here destroys the only evidence of why every
// turn in this run anchored to the fallback instead of its own commit.
func (r *turnAnchorResolver) buildAncestors(ctx context.Context) {
	r.ancestors = make(map[plumbing.Hash]struct{})
	iter, err := r.repo.Log(&git.LogOptions{
		From:  plumbing.NewHash(r.fallback),
		Order: git.LogOrderCommitterTime,
	})
	if err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk unavailable, all turns fall back",
			"fallback", r.fallback, "error", err.Error())
		return
	}
	defer iter.Close()
	err = iter.ForEach(func(c *object.Commit) error {
		if len(r.ancestors) >= r.maxWalk || c.Committer.When.Before(r.cutoff) {
			return storer.ErrStop
		}
		r.ancestors[c.Hash] = struct{}{}
		r.byTime = append(r.byTime, ancestorCommit{hash: c.Hash, when: c.Committer.When, email: c.Committer.Email})
		return nil
	})
	// The walk is newest-first except where clock skew dates a parent after
	// its child, which would break newestCommittedIn's binary search. A stable
	// sort keeps the walk's order among equal times, and the walk reaches a
	// parent only through a child it already emitted, so in linear history a
	// commit beats its parent made in the same second (a rebase replays several
	// in one).
	slices.SortStableFunc(r.byTime, func(a, b ancestorCommit) int { return b.when.Compare(a.when) })
	if err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk truncated",
			"fallback", r.fallback, "ancestors_collected", len(r.ancestors), "error", err.Error())
	}
}
