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
// and returns the repo plus the full SHAs of C1, C2, and S1. C2 and S1 are
// committed seconds apart from C1 and each other, so a commit window can cover
// one of them alone. turnAnchorResolver never consults HEAD/current branch, so
// the repo is left checked out on the side branch after this helper runs —
// that's fine for these tests.
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
	base := time.Now().Truncate(time.Second)

	// C2 on the default branch.
	c2 = writeAndCommitAt(t, wt, repoDir, "c2", "second", base.Add(10*time.Second))

	// side branch off C1, with a commit S1 that the default branch never merges.
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:   plumbing.NewHash(c1),
		Branch: plumbing.NewBranchReferenceName("side"),
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	s1 = writeAndCommitAt(t, wt, repoDir, "s1", "side commit", base.Add(20*time.Second))

	return repo, c1, c2, s1
}

// testCommitterEmail is the committer of every commit these tests make.
const testCommitterEmail = "test@test.com"

func writeAndCommit(t *testing.T, wt *git.Worktree, repoDir, content, msg string) {
	t.Helper()
	// When must be a real timestamp: the anchor resolver's bounded walk
	// stops at commits older than its date cutoff, and a zero-value When
	// (year 1) would halt the walk at the first commit.
	writeAndCommitAt(t, wt, repoDir, content, msg, time.Now())
}

// writeAndCommitAt commits with author and committer time when and returns
// the commit's full SHA.
func writeAndCommitAt(t *testing.T, wt *git.Worktree, repoDir, content, msg string, when time.Time) string {
	t.Helper()
	return writeAndCommitAsAt(t, wt, repoDir, content, msg, testCommitterEmail, when)
}

// writeAndCommitAsAt is writeAndCommitAt by committer email.
func writeAndCommitAsAt(t *testing.T, wt *git.Worktree, repoDir, content, msg, email string, when time.Time) string {
	t.Helper()
	testutil.WriteFile(t, repoDir, "f.txt", content)
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "Test", Email: email, When: when}
	hash, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

// committedAt returns sha's committer time as git stored it, in whole seconds.
func committedAt(t *testing.T, repo *git.Repository, sha string) time.Time {
	t.Helper()
	c, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		t.Fatal(err)
	}
	return c.Committer.When
}

func TestResolveTurnAnchor_PicksLastReachableCandidate(t *testing.T) {
	t.Parallel()
	repo, c1, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, testCommitterEmail, time.Now())

	got, fromCandidate := r.resolve(context.Background(), []string{c1[:7], c2[:7]}, nil)
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

// TestResolveTurnAnchor_CommitWindows proves a turn whose recorded SHAs anchor
// nothing anchors to the newest default-branch commit made inside one of its
// commit windows, and falls back when no ancestor was committed in one.
func TestResolveTurnAnchor_CommitWindows(t *testing.T) {
	t.Parallel()
	repo, c1, c2, s1 := buildAnchorTestRepo(t)
	c1At, c2At, s1At := committedAt(t, repo, c1), committedAt(t, repo, c2), committedAt(t, repo, s1)
	around := func(at time.Time) TimeWindow {
		return TimeWindow{Start: at.Add(-time.Second), End: at.Add(time.Second)}
	}
	r := newTurnAnchorResolver(repo, c2, testCommitterEmail, time.Now())

	tests := []struct {
		name              string
		candidates        []string
		windows           []TimeWindow
		want              string
		wantFromCandidate bool
	}{
		{"window over an ancestor", nil, []TimeWindow{around(c1At)}, c1, true},
		{"recorded SHA wins over a window", []string{c1[:7]}, []TimeWindow{around(c2At)}, c1, true},
		{"unresolvable SHA falls through to a window", []string{"deadbeef"}, []TimeWindow{around(c1At)}, c1, true},
		{"window over a non-ancestor", nil, []TimeWindow{around(s1At)}, c2, false},
		{"two commits in one window", nil, []TimeWindow{{Start: c1At, End: c2At}}, c2, true},
		{"newest across windows", nil, []TimeWindow{around(c2At), around(c1At)}, c2, true},
		{"window over no commit", nil, []TimeWindow{{Start: c1At.Add(3 * time.Second), End: c1At.Add(5 * time.Second)}}, c2, false},
	}
	for _, tt := range tests {
		got, fromCandidate := r.resolve(context.Background(), tt.candidates, tt.windows)
		if got != tt.want || fromCandidate != tt.wantFromCandidate {
			t.Errorf("%s: resolve = (%q, %v), want (%q, %v)", tt.name, got, fromCandidate, tt.want, tt.wantFromCandidate)
		}
	}
}

// TestResolveTurnAnchor_WindowPrefersChildInSameSecond: a rebase replays
// several commits within one second, and the turn's end state is the last of
// them, so a window over that second must anchor to the child, not its parent.
func TestResolveTurnAnchor_WindowPrefersChildInSameSecond(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Truncate(time.Second).Add(10 * time.Second)
	writeAndCommitAt(t, wt, repoDir, "parent", "parent", at)
	child := writeAndCommitAt(t, wt, repoDir, "child", "child", at)
	tip := writeAndCommitAt(t, wt, repoDir, "tip", "tip", at.Add(10*time.Second))
	r := newTurnAnchorResolver(repo, tip, testCommitterEmail, time.Now())

	got, _ := r.resolve(context.Background(), nil, []TimeWindow{{Start: at, End: at}})
	if got != child {
		t.Fatalf("resolve = %q, want child %q", got, child)
	}
}

// TestResolveTurnAnchor_ReportsFallback proves the fromCandidate return is
// false when the anchor genuinely came from the fallback (unreachable
// candidate), so the caller's debug log fires only for real fallbacks.
func TestResolveTurnAnchor_ReportsFallback(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, testCommitterEmail, time.Now())

	got, fromCandidate := r.resolve(context.Background(), []string{s1[:7]}, nil)
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

	r := newTurnAnchorResolver(repo, tip, testCommitterEmail, time.Now())
	got, fromCandidate := r.resolve(context.Background(), []string{oldHash.String()[:7]}, nil)
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
	r := newTurnAnchorResolver(repo, c2, testCommitterEmail, time.Now())
	r.maxWalk = 1

	got, fromCandidate := r.resolve(context.Background(), []string{c1[:7]}, nil)
	if got != c2 || fromCandidate {
		t.Fatalf("resolve = (%q, %v), want fallback %q: capped walk must not collect c1", got, fromCandidate, c2)
	}
	// The tip itself was collected before the cap hit, so it still anchors.
	if got, fromCandidate := r.resolve(context.Background(), []string{c2[:7]}, nil); got != c2 || !fromCandidate {
		t.Fatalf("resolve = (%q, %v), want tip as candidate match", got, fromCandidate)
	}
}

func TestResolveTurnAnchor_SkipsUnreachableAndUnresolvable(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2, testCommitterEmail, time.Now())
	ctx := context.Background()

	// s1 resolves but is not an ancestor of the fallback c2.
	if got, _ := r.resolve(ctx, []string{s1[:7]}, nil); got != c2 {
		t.Fatalf("unreachable candidate: resolve = %q, want fallback %q", got, c2)
	}

	// "deadbeef" is valid hex but doesn't resolve to anything in this repo.
	if got, _ := r.resolve(ctx, []string{"deadbeef"}, nil); got != c2 {
		t.Fatalf("unresolvable candidate: resolve = %q, want fallback %q", got, c2)
	}

	// nil candidates.
	if got, _ := r.resolve(ctx, nil, nil); got != c2 {
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
	r := newTurnAnchorResolver(repo, c2, testCommitterEmail, time.Now())
	ctx := context.Background()

	if got, _ := r.resolve(ctx, []string{"HEAD"}, nil); got != c2 {
		t.Fatalf("revision syntax candidate: resolve = %q, want fallback %q", got, c2)
	}
	if got, _ := r.resolve(ctx, []string{"HEAD~2"}, nil); got != c2 {
		t.Fatalf("revision syntax candidate: resolve = %q, want fallback %q", got, c2)
	}
}

func TestResolveTurnAnchor_EmptyFallback(t *testing.T) {
	t.Parallel()
	repo, c1, _, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, "", testCommitterEmail, time.Now())

	if got, _ := r.resolve(context.Background(), []string{c1[:7]}, nil); got != "" {
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
	r := newTurnAnchorResolver(repo, fallback, testCommitterEmail, time.Now())

	got, _ := r.resolve(context.Background(), []string{c1[:7]}, nil)
	if got != fallback {
		t.Fatalf("resolve = %q, want unresolvable fallback %q", got, fallback)
	}
}

// The default branch holds other people's pushed commits too: one inside the
// turn's git call, newer than the turn's own, must not win. A window with only
// someone else's commit, or an importer with no git email, falls back.
func TestResolveTurnAnchor_WindowOnlyAdoptsImportersCommits(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	own := writeAndCommitAt(t, wt, repoDir, "own", "agent: quiet commit", base.Add(10*time.Second))
	teammate := writeAndCommitAsAt(t, wt, repoDir, "mate", "teammate: pushed", "mate@example.com", base.Add(20*time.Second))
	tip := repoHeadSHA(t, repo)
	window := []TimeWindow{{Start: base, End: base.Add(30 * time.Second)}}

	if got, ok := newTurnAnchorResolver(repo, tip, testCommitterEmail, time.Now()).resolve(context.Background(), nil, window); got != own || !ok {
		t.Errorf("own and teammate commits in window: = %s ok=%v, want own %s", got, ok, own)
	}
	onlyMate := []TimeWindow{{Start: base.Add(15 * time.Second), End: base.Add(30 * time.Second)}}
	if got, ok := newTurnAnchorResolver(repo, tip, testCommitterEmail, time.Now()).resolve(context.Background(), nil, onlyMate); got != tip || ok {
		t.Errorf("only teammate %s in window: = %s ok=%v, want the tip %s", teammate, got, ok, tip)
	}
	if got, ok := newTurnAnchorResolver(repo, tip, "", time.Now()).resolve(context.Background(), nil, window); got != tip || ok {
		t.Errorf("no importer email: = %s ok=%v, want the tip %s", got, ok, tip)
	}
}
