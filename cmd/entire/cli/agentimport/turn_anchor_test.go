package agentimport

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

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
	r := newTurnAnchorResolver(repo, c2, time.Now())

	got, fromCandidate := r.resolve(context.Background(), []string{c1[:7], c2[:7]})
	if got != c2 {
		t.Fatalf("resolve = %q, want last candidate (full) %q", got, c2)
	}
	// The winning candidate happens to equal the fallback tip — resolve must
	// still report it as a candidate match, not a fallback (the caller's
	// "fell back" debug log keys off this).
	if !fromCandidate {
		t.Fatal("resolve reported fallback for a turn whose candidate matched")
	}
}

// TestResolveTurnAnchor_ReportsFallback proves the fromCandidate return is
// false when the anchor genuinely came from the fallback (unreachable
// candidate), so the caller's debug log fires only for real fallbacks.
func TestResolveTurnAnchor_ReportsFallback(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now())

	got, fromCandidate := r.resolve(context.Background(), []string{s1[:7]})
	if got != c2 || fromCandidate {
		t.Fatalf("resolve = (%q, %v), want fallback %q with fromCandidate=false", got, fromCandidate, c2)
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

	r := newTurnAnchorResolver(repo, tip, time.Now())
	got, fromCandidate := r.resolve(context.Background(), []string{oldHash.String()[:7]})
	if got != tip || fromCandidate {
		t.Fatalf("resolve = (%q, %v), want fallback %q: pre-cutoff commit must miss the bounded walk", got, fromCandidate, tip)
	}
}

// TestResolveTurnAnchor_MaxWalkCapBoundsWalk proves the commit-count cap: with
// maxWalk forced to 1, only the tip is collected, so an older (but recent and
// reachable) candidate misses the set and falls back.
func TestResolveTurnAnchor_MaxWalkCapBoundsWalk(t *testing.T) {
	t.Parallel()
	repo, c1, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now())
	r.maxWalk = 1

	got, fromCandidate := r.resolve(context.Background(), []string{c1[:7]})
	if got != c2 || fromCandidate {
		t.Fatalf("resolve = (%q, %v), want fallback %q: capped walk must not collect c1", got, fromCandidate, c2)
	}
	// The tip itself was collected before the cap hit, so it still anchors.
	if got, fromCandidate := r.resolve(context.Background(), []string{c2[:7]}); got != c2 || !fromCandidate {
		t.Fatalf("resolve = (%q, %v), want tip as candidate match", got, fromCandidate)
	}
}

func TestResolveTurnAnchor_SkipsUnreachableAndUnresolvable(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, time.Now())
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
	r := newTurnAnchorResolver(repo, c2, time.Now())
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
	r := newTurnAnchorResolver(repo, "", time.Now())

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
	r := newTurnAnchorResolver(repo, fallback, time.Now())

	got, _ := r.resolve(context.Background(), []string{c1[:7]})
	if got != fallback {
		t.Fatalf("resolve = %q, want unresolvable fallback %q", got, fallback)
	}
}
