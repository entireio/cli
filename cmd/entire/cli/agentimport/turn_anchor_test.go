package agentimport

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// buildAnchorTestRepo builds:
//
//	main:   C1 ── C2   (fallback anchor = C2's full sha)
//	side:   C1 ── S1   (side branch commit — resolvable but NOT an ancestor of C2)
//
// and returns the repo plus the full SHAs of C1, C2, and S1. turnAnchorResolver
// never consults HEAD/current branch, so the repo is left checked out on the
// side branch after this helper runs — that's fine for these tests.
func buildAnchorTestRepo(t *testing.T) (repo *git.Repository, c1, c2, s1 string) {
	t.Helper()
	repo, repoDir := initRepoWithCommit(t)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c1 = head.Hash().String()

	// C2 on the default branch.
	writeAndCommit(t, wt, repoDir, "c2", "second")
	head, err = repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c2 = head.Hash().String()

	// side branch off C1, with a commit S1 that the default branch never merges.
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:   plumbing.NewHash(c1),
		Branch: plumbing.NewBranchReferenceName("side"),
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, wt, repoDir, "s1", "side commit")
	head, err = repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	s1 = head.Hash().String()

	return repo, c1, c2, s1
}

func writeAndCommit(t *testing.T, wt *git.Worktree, repoDir, content, msg string) {
	t.Helper()
	testutil.WriteFile(t, repoDir, "f.txt", content)
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(msg, &git.CommitOptions{
		// When must be a real timestamp: the anchor resolver's bounded walk
		// stops at commits older than its date cutoff, and a zero-value When
		// (year 1) would halt the walk at the first commit.
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTurnAnchor_PicksLastReachableCandidate(t *testing.T) {
	t.Parallel()
	repo, c1, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)

	got, method := r.resolve(context.Background(), []string{c1[:7], c2[:7]})
	if got != c2 {
		t.Fatalf("resolve = %q, want last candidate (full) %q", got, c2)
	}
	// The winning candidate happens to equal the fallback tip — resolve must
	// still report it as a recorded match, not a fallback (the caller's "fell
	// back" debug log and the stored commit_sha_method both key off this).
	if method != cp.CommitSHAMethodRecorded {
		t.Fatalf("resolve method = %q, want %q for a turn whose candidate matched", method, cp.CommitSHAMethodRecorded)
	}
}

// TestResolveTurnAnchor_ReportsFallback proves the returned method is
// "fallback" when the anchor genuinely came from the fallback (unreachable
// candidate), so the caller's debug log fires only for real fallbacks and the
// stored link is labeled as the display anchor it is.
func TestResolveTurnAnchor_ReportsFallback(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)

	got, method := r.resolve(context.Background(), []string{s1[:7]})
	if got != c2 || method != cp.CommitSHAMethodFallback {
		t.Fatalf("resolve = (%q, %q), want fallback %q labeled %q", got, method, c2, cp.CommitSHAMethodFallback)
	}
}

// TestResolveTurnAnchor_ExtraAcceptGatesUnreachableCandidate proves the
// reconcile widening: a commit that is NOT reachable from the fallback tip
// (an unmerged feature-branch commit) anchors as "recorded" once it is in the
// scanned set, and still falls back when it isn't. Set membership is the whole
// gate — that is what keeps a rebased-away SHA, which resolves as an object
// but was never scanned, from being linked.
func TestResolveTurnAnchor_ExtraAcceptGatesUnreachableCandidate(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)

	withoutScan := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)
	if got, method := withoutScan.resolve(context.Background(), []string{s1[:7]}); got != c2 || method != cp.CommitSHAMethodFallback {
		t.Fatalf("unscanned side commit: resolve = (%q, %q), want fallback %q", got, method, c2)
	}

	withScan := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)
	withScan.extraAccept = map[plumbing.Hash]*CommitRecord{
		plumbing.NewHash(s1): {SHA: s1},
	}
	got, method := withScan.resolve(context.Background(), []string{s1[:7]})
	if got != s1 || method != cp.CommitSHAMethodRecorded {
		t.Fatalf("scanned side commit: resolve = (%q, %q), want (%q, %q)", got, method, s1, cp.CommitSHAMethodRecorded)
	}

	// An empty fallback (nothing to anchor a display link to) must not disable
	// the scan set: it is an independent gate, so a scanned candidate still
	// resolves to a recorded link.
	noFallback := newTurnAnchorResolver(repo, "", time.Now(), DefaultLookbackDays)
	noFallback.extraAccept = map[plumbing.Hash]*CommitRecord{plumbing.NewHash(s1): {SHA: s1}}
	if got, method := noFallback.resolve(context.Background(), []string{s1[:7]}); got != s1 || method != cp.CommitSHAMethodRecorded {
		t.Fatalf("empty fallback with a scan set: resolve = (%q, %q), want (%q, %q)", got, method, s1, cp.CommitSHAMethodRecorded)
	}
}

// TestResolveTurnAnchor_DateCutoffBoundsWalk proves the ancestor walk stops at
// commits older than the lookback-plus-slack cutoff: a candidate commit
// backdated past the cutoff misses the (bounded) ancestor set and its turn
// falls back, even though the commit is genuinely reachable from the tip.
func TestResolveTurnAnchor_DateCutoffBoundsWalk(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	// A commit far older than LookbackDays+slack, then a fresh tip on top.
	old := time.Now().Add(-365 * 24 * time.Hour)
	testutil.WriteFile(t, repoDir, "f.txt", "old")
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	oldHash, err := wt.Commit("backdated", &git.CommitOptions{
		Author:    &object.Signature{Name: "Test", Email: "test@test.com", When: old},
		Committer: &object.Signature{Name: "Test", Email: "test@test.com", When: old},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, wt, repoDir, "tip", "fresh tip")
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	tip := head.Hash().String()

	r := newTurnAnchorResolver(repo, tip, time.Now(), DefaultLookbackDays)
	got, method := r.resolve(context.Background(), []string{oldHash.String()[:7]})
	if got != tip || method != cp.CommitSHAMethodFallback {
		t.Fatalf("resolve = (%q, %q), want fallback %q: pre-cutoff commit must miss the bounded walk", got, method, tip)
	}
}

// TestResolveTurnAnchor_MaxWalkCapBoundsWalk proves the commit-count cap: with
// maxWalk forced to 1, only the tip is collected, so an older (but recent and
// reachable) candidate misses the set and falls back.
func TestResolveTurnAnchor_MaxWalkCapBoundsWalk(t *testing.T) {
	t.Parallel()
	repo, c1, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)
	r.maxWalk = 1

	got, method := r.resolve(context.Background(), []string{c1[:7]})
	if got != c2 || method != cp.CommitSHAMethodFallback {
		t.Fatalf("resolve = (%q, %q), want fallback %q: capped walk must not collect c1", got, method, c2)
	}
	// The tip itself was collected before the cap hit, so it still anchors.
	if got, method := r.resolve(context.Background(), []string{c2[:7]}); got != c2 || method != cp.CommitSHAMethodRecorded {
		t.Fatalf("resolve = (%q, %q), want tip as recorded candidate match", got, method)
	}
}

func TestResolveTurnAnchor_SkipsUnreachableAndUnresolvable(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)
	ctx := context.Background()

	// s1 resolves but is not an ancestor of the fallback c2.
	if got, _ := r.resolve(ctx, []string{s1[:7]}); got != c2 {
		t.Fatalf("unreachable candidate: resolve = %q, want fallback %q", got, c2)
	}

	// "deadbeef" is valid hex but doesn't resolve to anything in this repo.
	if got, _ := r.resolve(ctx, []string{"deadbeef"}); got != c2 {
		t.Fatalf("unresolvable candidate: resolve = %q, want fallback %q", got, c2)
	}

	// nil candidates.
	if got, _ := r.resolve(ctx, nil); got != c2 {
		t.Fatalf("nil candidates: resolve = %q, want fallback %q", got, c2)
	}
}

// TestResolveTurnAnchor_RejectsRevisionSyntax proves a candidate that looks
// like git revision syntax rather than a sha (e.g. "HEAD") is rejected before
// ever reaching ResolveRevision, so it can't resolve as an expression and
// falls through to the fallback like any other unresolvable candidate.
func TestResolveTurnAnchor_RejectsRevisionSyntax(t *testing.T) {
	t.Parallel()
	repo, _, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now(), DefaultLookbackDays)
	ctx := context.Background()

	if got, _ := r.resolve(ctx, []string{"HEAD"}); got != c2 {
		t.Fatalf("revision syntax candidate: resolve = %q, want fallback %q", got, c2)
	}
	if got, _ := r.resolve(ctx, []string{"HEAD~2"}); got != c2 {
		t.Fatalf("revision syntax candidate: resolve = %q, want fallback %q", got, c2)
	}
}

func TestResolveTurnAnchor_EmptyFallback(t *testing.T) {
	t.Parallel()
	repo, c1, _, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, "", time.Now(), DefaultLookbackDays)

	if got, _ := r.resolve(context.Background(), []string{c1[:7]}); got != "" {
		t.Fatalf("empty fallback: resolve = %q, want empty", got)
	}
}

// TestResolveTurnAnchor_FallbackDoesNotResolve proves a non-empty,
// well-formed (full hex, 40 chars) fallback that simply doesn't exist in the
// repo degrades gracefully: buildAncestors' CommitObject lookup fails, the
// ancestor set stays empty, every candidate falls through, and resolve
// returns the (unresolvable) fallback string verbatim rather than panicking.
func TestResolveTurnAnchor_FallbackDoesNotResolve(t *testing.T) {
	t.Parallel()
	repo, c1, _, _ := buildAnchorTestRepo(t)
	fallback := strings.Repeat("ca", 20) // valid hex, 40 chars, not a real object
	r := newTurnAnchorResolver(repo, fallback, time.Now(), DefaultLookbackDays)

	got, _ := r.resolve(context.Background(), []string{c1[:7]})
	if got != fallback {
		t.Fatalf("resolve = %q, want unresolvable fallback %q", got, fallback)
	}
}
