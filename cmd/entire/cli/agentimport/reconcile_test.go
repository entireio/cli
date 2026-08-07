package agentimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	cp "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// commitAt creates a commit with an explicit committer time, so the tests can
// place commits inside or outside a turn's match window and outside the scan's
// date cutoff deterministically.
func commitAt(t *testing.T, repo *git.Repository, repoDir, content, msg string, when time.Time) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, repoDir, "f.txt", content)
	if _, err := wt.Add("f.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "Test", Email: "test@test.com", When: when}
	hash, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func headHash(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash()
}

// v1Tip returns the entire/checkpoints/v1 branch tip, or the zero hash when the
// branch doesn't exist. Tests compare it before/after a run to prove a
// converged reconcile writes nothing at all.
func v1Tip(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(cp.DefaultV1Refs().Primary, true)
	if err != nil {
		return plumbing.ZeroHash
	}
	return ref.Hash()
}

const turnOneAt = "2026-06-20T00:00:00Z"

// Fixture line UUIDs. Every reconcile fixture below is a single-turn session,
// so the turn UUID is fixed and the derived checkpoint ID follows from the
// session name alone.
const (
	fxTurnUUID   = "u1"
	fxCommitUUID = "tr1"
)

// claudeTurnLine renders the single user-prompt line every reconcile fixture
// below starts with. Everything about it is fixed: linking looks only at the
// timestamp (for heuristic windows) and the UUID (for the derived checkpoint
// ID), and each fixture repo places its commits relative to turnOneAt.
func claudeTurnLine() string {
	return `{"type":"user","uuid":"` + fxTurnUUID + `","timestamp":"` + turnOneAt +
		`","message":{"role":"user","content":"` + fxFirst + `"}}`
}

// claudeCommitLine renders the toolUseResult.gitOperation record the Claude
// importer reads commit SHAs from.
func claudeCommitLine(sha string) string {
	return `{"type":"user","uuid":"` + fxCommitUUID +
		`","toolUseResult":{"gitOperation":{"commit":{"sha":"` + sha + `","kind":"committed"}}}}`
}

// writeClaudeFixture writes a Claude transcript made of the given raw lines.
func writeClaudeFixture(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustParseTime parses an RFC3339 fixture timestamp.
func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestCollectCommitsMissingSessionData_SkipsTrailered(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	now := time.Now()
	plain := commitAt(t, repo, repoDir, "plain", "plain work", now.Add(-time.Hour))
	trailered := commitAt(t, repo, repoDir, "linked", "linked work\n\nEntire-Checkpoint: aabbccddeeff", now)

	missing, err := collectCommitsMissingSessionData(context.Background(), repo,
		[]plumbing.Hash{headHash(t, repo)}, now.Add(-30*24*time.Hour), ancestorWalkMaxCommits)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := missing[trailered]; ok {
		t.Fatal("a commit carrying an Entire-Checkpoint trailer must not be reported as missing session data")
	}
	rec, ok := missing[plain]
	if !ok {
		t.Fatalf("plain commit missing from scan: %+v", missing)
	}
	if rec.Subject != "plain work" {
		t.Fatalf("record subject = %q, want the commit's first line", rec.Subject)
	}
	if rec.SHA != plain.String() {
		t.Fatalf("record SHA = %q, want %q", rec.SHA, plain)
	}
}

// TestCollectCommitsMissingSessionData_MultiTipDedupe proves a commit reachable
// from two tips is recorded once, and that a commit reachable ONLY from the
// HEAD tip (an unmerged feature branch) is still scanned — that second tip is
// the whole reason the CLI resolves three of them.
func TestCollectCommitsMissingSessionData_MultiTipDedupe(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	now := time.Now()
	base := headHash(t, repo)
	mainTip := commitAt(t, repo, repoDir, "main", "main work", now.Add(-2*time.Hour))

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash: base, Branch: plumbing.NewBranchReferenceName("feature"), Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	featureTip := commitAt(t, repo, repoDir, "feature", "feature work", now.Add(-time.Hour))

	missing, err := collectCommitsMissingSessionData(context.Background(), repo,
		[]plumbing.Hash{mainTip, featureTip, mainTip}, now.Add(-30*24*time.Hour), ancestorWalkMaxCommits)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []plumbing.Hash{base, mainTip, featureTip} {
		if _, ok := missing[want]; !ok {
			t.Fatalf("commit %s missing from multi-tip scan: %+v", want, missing)
		}
	}
	// base is reachable from both tips but is one map entry — the map itself is
	// the dedupe, so a repeated tip cannot inflate CommitsScanned.
	if len(missing) != 3 {
		t.Fatalf("scan collected %d commits, want 3 (base + both tips, deduped)", len(missing))
	}
}

func TestCollectCommitsMissingSessionData_BoundsWalk(t *testing.T) {
	t.Parallel()
	repo, repoDir := initRepoWithCommit(t)
	now := time.Now()
	old := commitAt(t, repo, repoDir, "old", "ancient work", now.Add(-365*24*time.Hour))
	recent := commitAt(t, repo, repoDir, fxRecent, "recent work", now)
	tips := []plumbing.Hash{headHash(t, repo)}

	byDate, err := collectCommitsMissingSessionData(context.Background(), repo, tips,
		now.Add(-30*24*time.Hour), ancestorWalkMaxCommits)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byDate[old]; ok {
		t.Fatal("commit older than the cutoff must not be scanned")
	}
	if _, ok := byDate[recent]; !ok {
		t.Fatal("commit inside the window must be scanned")
	}

	byCap, err := collectCommitsMissingSessionData(context.Background(), repo, tips,
		now.Add(-365*2*24*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(byCap) != 1 {
		t.Fatalf("maxWalk=1 collected %d commits, want 1", len(byCap))
	}
}

func TestMatchHeuristic_UnambiguousOnly(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	turns := []Turn{
		{UUID: "u1", CreatedAt: base},
		{UUID: "u2", CreatedAt: base.Add(time.Hour)},
	}
	// Synthetic, distinct hashes: matchHeuristic never touches the repo, it
	// only keys the missing-commit map.
	hashOf := func(nibble string) plumbing.Hash {
		return plumbing.NewHash(strings.Repeat(nibble, 40))
	}

	t.Run("one commit per turn window is proposed", func(t *testing.T) {
		t.Parallel()
		inFirst := hashOf("1")
		missing := map[plumbing.Hash]*CommitRecord{
			inFirst: {SHA: inFirst.String(), When: base.Add(10 * time.Minute)},
		}
		got := matchHeuristic(turns, missing)
		if len(got) != 1 {
			t.Fatalf("want 1 proposal, got %+v", got)
		}
		if got[0].TurnUUID != "u1" || got[0].Method != cp.CommitSHAMethodHeuristic {
			t.Fatalf("proposal = %+v, want turn u1 labeled heuristic", got[0])
		}
	})

	t.Run("two commits in one window leaves both unmatched", func(t *testing.T) {
		t.Parallel()
		a, b := hashOf("2"), hashOf("3")
		missing := map[plumbing.Hash]*CommitRecord{
			a: {SHA: a.String(), When: base.Add(10 * time.Minute)},
			b: {SHA: b.String(), When: base.Add(20 * time.Minute)},
		}
		if got := matchHeuristic(turns, missing); len(got) != 0 {
			t.Fatalf("ambiguous window must propose nothing, got %+v", got)
		}
	})

	t.Run("a commit two windows claim is unmatched", func(t *testing.T) {
		t.Parallel()
		// The grace period runs past the next turn's start, so a commit just
		// after turn 2 begins falls inside BOTH turn windows.
		overlap := hashOf("4")
		missing := map[plumbing.Hash]*CommitRecord{
			overlap: {SHA: overlap.String(), When: base.Add(time.Hour + time.Minute)},
		}
		if got := matchHeuristic(turns, missing); len(got) != 0 {
			t.Fatalf("overlapping windows must propose nothing, got %+v", got)
		}
	})

	t.Run("a turn without a timestamp never matches", func(t *testing.T) {
		t.Parallel()
		undated := []Turn{{UUID: "u1"}}
		c := hashOf("5")
		missing := map[plumbing.Hash]*CommitRecord{c: {SHA: c.String(), When: base}}
		if got := matchHeuristic(undated, missing); len(got) != 0 {
			t.Fatalf("undated turn must propose nothing, got %+v", got)
		}
	})
}

// reconcileFixtureRepo builds a repo whose default branch holds one commit and
// whose HEAD sits on a feature branch with a second, unmerged commit — the
// shape reconcile exists for. It returns the repo, its dir, the default-branch
// tip (the link anchor) and the feature commit.
func reconcileFixtureRepo(t *testing.T) (repo *git.Repository, repoDir string, anchor, feature plumbing.Hash) {
	t.Helper()
	repo, repoDir = initRepoWithCommit(t)
	anchor = headHash(t, repo)

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash: anchor, Branch: plumbing.NewBranchReferenceName("feature"), Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	feature = commitAt(t, repo, repoDir, "feature", "feature work", mustParseTime(t, turnOneAt).Add(time.Minute))
	return repo, repoDir, anchor, feature
}

// reconcileOpts builds the Run options these tests share: reconciliation on,
// both tips scanned, and a Now late enough that the fixture timestamps sit
// inside the lookback window.
func reconcileOpts(repoDir, transcriptDir string, anchor plumbing.Hash, tips []plumbing.Hash, acceptHeuristics bool) Options {
	return Options{
		RepoRoot: repoDir, OverridePath: transcriptDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LinkCommitSHA: anchor.String(),
		Reconcile:     &ReconcileOptions{Enabled: true, AcceptHeuristics: acceptHeuristics},
		ScanTips:      tips,
	}
}

// TestRun_ReconcileLinksRecordedCommit proves the core promise: a turn whose
// transcript recorded a commit that is NOT reachable from the link anchor (it
// lives on an unmerged feature branch) is linked to that exact commit because
// the scan found it, and the link is labeled "recorded".
func TestRun_ReconcileLinksRecordedCommit(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-rec.jsonl",
		claudeTurnLine(),
		claudeCommitLine(feature.String()[:7]),
	)

	res, err := Run(context.Background(), repo, claudeImporter{},
		reconcileOpts(repoDir, claudeDir, anchor, []plumbing.Hash{anchor, feature}, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Report == nil {
		t.Fatal("reconcile run produced no report")
	}
	if res.LinksRecorded != 1 {
		t.Fatalf("LinksRecorded = %d, want 1 (%+v)", res.LinksRecorded, res.Report)
	}
	if len(res.Report.Links) != 1 {
		t.Fatalf("want 1 reported link, got %+v", res.Report.Links)
	}
	link := res.Report.Links[0]
	if link.CommitSHA != feature.String() || link.Method != cp.CommitSHAMethodRecorded || link.Action != ActionWritten {
		t.Fatalf("link = %+v, want the feature commit recorded+written", link)
	}
	// The anchor commit itself had no turn recording it, so it stays unmatched.
	if len(res.Report.UnmatchedCommits) != 1 || res.Report.UnmatchedCommits[0].SHA != anchor.String() {
		t.Fatalf("unmatched = %+v, want only the anchor commit", res.Report.UnmatchedCommits)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cid := DeriveCheckpointID("sess-rec", fxTurnUUID)
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != feature.String() || md.CommitSHAMethod != cp.CommitSHAMethodRecorded {
		t.Fatalf("session metadata link = (%q, %q), want (%q, recorded)", md.CommitSHA, md.CommitSHAMethod, feature)
	}
	summary, err := stores.Persistent.Read(context.Background(), cid)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CommitSHA != feature.String() || summary.CommitSHAMethod != cp.CommitSHAMethodRecorded {
		t.Fatalf("root summary link = (%q, %q), want (%q, recorded)", summary.CommitSHA, summary.CommitSHAMethod, feature)
	}
}

// TestRun_ReconcileRebasedAwaySHAFallsBack proves the set-membership gate:
// a recorded SHA that resolves as an object but was never scanned (it lives
// outside every tip's history, as a rebased-away commit does) is NOT linked.
func TestRun_ReconcileRebasedAwaySHAFallsBack(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-gone.jsonl",
		claudeTurnLine(),
		claudeCommitLine(feature.String()[:7]),
	)

	// Only the default-branch tip is scanned, so the feature commit — still a
	// resolvable object — is outside the scanned set.
	res, err := Run(context.Background(), repo, claudeImporter{},
		reconcileOpts(repoDir, claudeDir, anchor, []plumbing.Hash{anchor}, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.LinksRecorded != 0 || len(res.Report.Links) != 0 {
		t.Fatalf("unscanned commit must not be linked: %+v", res.Report)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), DeriveCheckpointID("sess-gone", fxTurnUUID), 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != anchor.String() || md.CommitSHAMethod != cp.CommitSHAMethodFallback {
		t.Fatalf("link = (%q, %q), want the fallback anchor %q", md.CommitSHA, md.CommitSHAMethod, anchor)
	}
}

// TestRun_ReconcileHeuristicNeedsAcceptFlag proves a time-window match is
// reported but not written without --accept-heuristics, and written with it.
func TestRun_ReconcileHeuristicNeedsAcceptFlag(t *testing.T) {
	t.Parallel()

	// The transcript records no commit at all, so only the time window can
	// connect the turn to the feature commit (made one minute into the turn).
	setup := func(t *testing.T, sessionName string) (*git.Repository, Options, plumbing.Hash, plumbing.Hash) {
		t.Helper()
		repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
		claudeDir := t.TempDir()
		writeClaudeFixture(t, claudeDir, sessionName+".jsonl", claudeTurnLine())
		return repo, reconcileOpts(repoDir, claudeDir, anchor, []plumbing.Hash{anchor, feature}, false), anchor, feature
	}

	t.Run("reported as a candidate by default", func(t *testing.T) {
		t.Parallel()
		repo, opts, anchor, feature := setup(t, "sess-cand")
		res, err := Run(context.Background(), repo, claudeImporter{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.LinksHeuristic != 0 || len(res.Report.Links) != 0 {
			t.Fatalf("heuristic match must not be written without --accept-heuristics: %+v", res.Report)
		}
		if len(res.Report.Candidates) != 1 || res.Report.Candidates[0].CommitSHA != feature.String() {
			t.Fatalf("candidates = %+v, want the feature commit", res.Report.Candidates)
		}
		if res.Report.Candidates[0].Action != ActionProposed {
			t.Fatalf("candidate action = %q, want %q", res.Report.Candidates[0].Action, ActionProposed)
		}

		stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		md, err := stores.Persistent.ReadSessionMetadata(context.Background(), DeriveCheckpointID("sess-cand", fxTurnUUID), 0)
		if err != nil {
			t.Fatal(err)
		}
		if md.CommitSHA != anchor.String() || md.CommitSHAMethod != cp.CommitSHAMethodFallback {
			t.Fatalf("stored link = (%q, %q), want the untouched fallback anchor", md.CommitSHA, md.CommitSHAMethod)
		}
	})

	t.Run("written with --accept-heuristics", func(t *testing.T) {
		t.Parallel()
		repo, opts, _, feature := setup(t, "sess-acc")
		opts.Reconcile.AcceptHeuristics = true
		res, err := Run(context.Background(), repo, claudeImporter{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		if res.LinksHeuristic != 1 || len(res.Report.Candidates) != 0 {
			t.Fatalf("want 1 accepted heuristic link and no candidates, got %+v", res.Report)
		}

		stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		md, err := stores.Persistent.ReadSessionMetadata(context.Background(), DeriveCheckpointID("sess-acc", fxTurnUUID), 0)
		if err != nil {
			t.Fatal(err)
		}
		if md.CommitSHA != feature.String() || md.CommitSHAMethod != cp.CommitSHAMethodHeuristic {
			t.Fatalf("stored link = (%q, %q), want (%q, heuristic)", md.CommitSHA, md.CommitSHAMethod, feature)
		}
	})
}

// TestRun_ReconcileIsIdempotent proves a second reconcile run converges: every
// link reports as unchanged, no counters move, and the v1 branch tip does not
// budge — the store's no-op guard means not even an empty commit is created.
func TestRun_ReconcileIsIdempotent(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-idem.jsonl",
		claudeTurnLine(),
		claudeCommitLine(feature.String()[:7]),
	)
	opts := reconcileOpts(repoDir, claudeDir, anchor, []plumbing.Hash{anchor, feature}, false)

	if _, err := Run(context.Background(), repo, claudeImporter{}, opts); err != nil {
		t.Fatal(err)
	}
	tipAfterFirst := v1Tip(t, repo)

	res, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsImported != 0 || res.TurnsSkipped != 1 {
		t.Fatalf("re-run should import nothing: %+v", res)
	}
	if res.LinksRecorded != 0 || res.Backfilled != 0 {
		t.Fatalf("converged re-run must do no link work: %+v", res)
	}
	if len(res.Report.Links) != 1 || res.Report.Links[0].Action != ActionUnchanged {
		t.Fatalf("links = %+v, want a single unchanged link", res.Report.Links)
	}
	if got := v1Tip(t, repo); got != tipAfterFirst {
		t.Fatalf("v1 tip moved on a converged re-run: %s -> %s", tipAfterFirst, got)
	}
}

// TestRun_ReconcileBackfillsAfterPlainImport proves the backfill path: a plain
// import stamps the fallback anchor, and a later --reconcile run upgrades that
// same checkpoint in place to the recorded link.
func TestRun_ReconcileBackfillsAfterPlainImport(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-back.jsonl",
		claudeTurnLine(),
		claudeCommitLine(feature.String()[:7]),
	)
	cid := DeriveCheckpointID("sess-back", fxTurnUUID)

	plain := Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LinkCommitSHA: anchor.String(),
	}
	if _, err := Run(context.Background(), repo, claudeImporter{}, plain); err != nil {
		t.Fatal(err)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != anchor.String() || md.CommitSHAMethod != cp.CommitSHAMethodFallback {
		t.Fatalf("plain import link = (%q, %q), want the fallback anchor", md.CommitSHA, md.CommitSHAMethod)
	}

	res, err := Run(context.Background(), repo, claudeImporter{},
		reconcileOpts(repoDir, claudeDir, anchor, []plumbing.Hash{anchor, feature}, false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Backfilled != 1 || res.LinksRecorded != 1 {
		t.Fatalf("want one backfilled recorded link, got %+v", res)
	}
	if len(res.Report.Links) != 1 || res.Report.Links[0].Action != ActionBackfilled {
		t.Fatalf("links = %+v, want a single backfilled link", res.Report.Links)
	}

	md, err = stores.Persistent.ReadSessionMetadata(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != feature.String() || md.CommitSHAMethod != cp.CommitSHAMethodRecorded {
		t.Fatalf("backfilled session link = (%q, %q), want (%q, recorded)", md.CommitSHA, md.CommitSHAMethod, feature)
	}
	summary, err := stores.Persistent.Read(context.Background(), cid)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CommitSHA != feature.String() || summary.CommitSHAMethod != cp.CommitSHAMethodRecorded {
		t.Fatalf("backfilled summary link = (%q, %q), want (%q, recorded)", summary.CommitSHA, summary.CommitSHAMethod, feature)
	}
}

// TestRun_ReconcileDryRunWritesNothing proves --dry-run --reconcile produces a
// full plan without touching the store.
func TestRun_ReconcileDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-dry.jsonl",
		claudeTurnLine(),
		claudeCommitLine(feature.String()[:7]),
	)
	opts := reconcileOpts(repoDir, claudeDir, anchor, []plumbing.Hash{anchor, feature}, false)
	opts.DryRun = true

	res, err := Run(context.Background(), repo, claudeImporter{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Report.Links) != 1 || res.Report.Links[0].Action != ActionDryRun {
		t.Fatalf("links = %+v, want a single dry-run link", res.Report.Links)
	}
	if res.Report.Links[0].CommitSHA != feature.String() {
		t.Fatalf("dry-run link SHA = %q, want %q", res.Report.Links[0].CommitSHA, feature)
	}
	if v1Tip(t, repo) != plumbing.ZeroHash {
		t.Fatal("dry run must not create the v1 branch")
	}
}

// TestLinkImproves_RefusesDowngrade pins the caller-side half of the downgrade
// guard: a heuristic link never replaces a recorded one, and an identical link
// is never rewritten. The store enforces the same rule independently.
func TestLinkImproves_RefusesDowngrade(t *testing.T) {
	t.Parallel()
	const shaA, shaB = "aaaa", "bbbb"
	cases := []struct {
		name                    string
		storedSHA, storedMethod string
		sha, method             string
		want                    bool
	}{
		{"recorded over fallback", shaA, cp.CommitSHAMethodFallback, shaB, cp.CommitSHAMethodRecorded, true},
		{"recorded over legacy unlabeled", shaA, "", shaB, cp.CommitSHAMethodRecorded, true},
		{"recorded over a different recorded", shaA, cp.CommitSHAMethodRecorded, shaB, cp.CommitSHAMethodRecorded, true},
		{"recorded over the same recorded", shaA, cp.CommitSHAMethodRecorded, shaA, cp.CommitSHAMethodRecorded, false},
		{"heuristic over recorded", shaA, cp.CommitSHAMethodRecorded, shaB, cp.CommitSHAMethodHeuristic, false},
		{"heuristic over heuristic", shaA, cp.CommitSHAMethodHeuristic, shaB, cp.CommitSHAMethodHeuristic, false},
		{"heuristic over fallback", shaA, cp.CommitSHAMethodFallback, shaB, cp.CommitSHAMethodHeuristic, true},
		{"heuristic into an empty link", "", "", shaB, cp.CommitSHAMethodHeuristic, true},
		{"fallback is never a link", shaA, cp.CommitSHAMethodFallback, shaB, cp.CommitSHAMethodFallback, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := linkImproves(tc.storedSHA, tc.storedMethod, tc.sha, tc.method); got != tc.want {
				t.Fatalf("linkImproves(%q,%q,%q,%q) = %v, want %v",
					tc.storedSHA, tc.storedMethod, tc.sha, tc.method, got, tc.want)
			}
		})
	}
}

// TestRun_ReconcileRefusesHeuristicDowngradeOfRecordedLink is the end-to-end
// counterpart: once a checkpoint holds a recorded link, a later
// --accept-heuristics run pointing at a different commit leaves it alone.
func TestRun_ReconcileRefusesHeuristicDowngradeOfRecordedLink(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, feature := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-guard.jsonl",
		claudeTurnLine(),
		claudeCommitLine(feature.String()[:7]),
	)
	cid := DeriveCheckpointID("sess-guard", fxTurnUUID)
	tips := []plumbing.Hash{anchor, feature}

	if _, err := Run(context.Background(), repo, claudeImporter{},
		reconcileOpts(repoDir, claudeDir, anchor, tips, false)); err != nil {
		t.Fatal(err)
	}

	// Second pass: the transcript no longer records the commit (as if the
	// gitOperation record were lost), so only a heuristic match is available.
	writeClaudeFixture(t, claudeDir, "sess-guard.jsonl", claudeTurnLine())
	res, err := Run(context.Background(), repo, claudeImporter{},
		reconcileOpts(repoDir, claudeDir, anchor, tips, true))
	if err != nil {
		t.Fatal(err)
	}
	if res.Backfilled != 0 {
		t.Fatalf("a heuristic match must not overwrite a recorded link: %+v", res)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), cid, 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHAMethod != cp.CommitSHAMethodRecorded || md.CommitSHA != feature.String() {
		t.Fatalf("stored link = (%q, %q), want the original recorded link to survive", md.CommitSHA, md.CommitSHAMethod)
	}
}

// TestRun_WithoutReconcileProducesNoReport proves the feature is inert when
// off: no report, no counters, and the fallback anchor is still stamped and
// labeled so old and new imports are distinguishable.
func TestRun_WithoutReconcileProducesNoReport(t *testing.T) {
	t.Parallel()
	repo, repoDir, anchor, _ := reconcileFixtureRepo(t)
	claudeDir := t.TempDir()
	writeClaudeFixture(t, claudeDir, "sess-off.jsonl", claudeTurnLine())

	res, err := Run(context.Background(), repo, claudeImporter{}, Options{
		RepoRoot: repoDir, OverridePath: claudeDir,
		Now:           time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC),
		LinkCommitSHA: anchor.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Report != nil {
		t.Fatalf("non-reconcile run must produce no report, got %+v", res.Report)
	}

	stores, err := cp.Open(context.Background(), repo, cp.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	md, err := stores.Persistent.ReadSessionMetadata(context.Background(), DeriveCheckpointID("sess-off", fxTurnUUID), 0)
	if err != nil {
		t.Fatal(err)
	}
	if md.CommitSHA != anchor.String() || md.CommitSHAMethod != cp.CommitSHAMethodFallback {
		t.Fatalf("link = (%q, %q), want the fallback anchor labeled as such", md.CommitSHA, md.CommitSHAMethod)
	}
}
