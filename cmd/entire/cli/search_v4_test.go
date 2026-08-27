package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/search"
)

// --- dedup ------------------------------------------------------------------

// TestDedupSemanticResults_SessionsOnly exercises the deduper's web-parity
// contract (ENT-1777): SESSION rows are the one type deduped across cells —
// by global session id, else the repo-qualified carrier checkpoint
// (server-folded legacy rows, ENT-1595) — while repo-bound types pass through
// untouched, since the fan-out routes each repo to one home placement and
// cross-cell duplicates of them are structurally impossible (ENT-1672/1776).
func TestDedupSemanticResults_SessionsOnly(t *testing.T) {
	t.Parallel()

	// Distinct legacy sessions in the same repo must all survive.
	if out := dedupSemanticResults([]search.Result{
		v4RepoScopedRow(search.TypeSession, "id-a", "backend", 0.9),
		v4RepoScopedRow(search.TypeSession, "id-b", "backend", 0.8),
	}); len(out) != 2 {
		t.Errorf("distinct ids collapsed: in=2 out=%d", len(out))
	}

	// The SAME legacy session from a repo mirrored across cells collapses,
	// first (higher-ranked) copy winning.
	out := dedupSemanticResults([]search.Result{
		v4RepoScopedRow(search.TypeSession, "id-x", "backend", 0.9),
		v4RepoScopedRow(search.TypeSession, "id-x", "backend", 0.8),
	})
	if len(out) != 1 {
		t.Errorf("mirrored session not deduped: in=2 out=%d", len(out))
	} else if out[0].Meta.Score != 0.9 {
		t.Errorf("kept score = %v, want the first/higher-ranked 0.9", out[0].Meta.Score)
	}

	// Repo casing skew across cells (git remote vs repo index) still dedupes.
	if out := dedupSemanticResults([]search.Result{
		v4RepoScopedRow(search.TypeSession, "id-x", "backend", 0.9),
		v4RepoScopedRow(search.TypeSession, "id-x", "Backend", 0.8),
	}); len(out) != 1 {
		t.Errorf("casing skew leaked a duplicate: in=2 out=%d", len(out))
	}

	// The same carrier id in DIFFERENT repos is a distinct result.
	if out := dedupSemanticResults([]search.Result{
		v4RepoScopedRow(search.TypeSession, "id-dup", "backend", 0.9),
		v4RepoScopedRow(search.TypeSession, "id-dup", "frontend", 0.8),
	}); len(out) != 2 {
		t.Errorf("cross-repo same carrier collapsed: in=2 out=%d", len(out))
	}

	// A session with a global id dedupes ACROSS repos — that is the case the
	// session-only rule exists for (a crosslinked session attached to repos
	// homed in different cells).
	if out := dedupSemanticResults([]search.Result{
		v4Session("sess-1", "backend", 0.9),
		v4Session("sess-1", "frontend", 0.8),
	}); len(out) != 1 {
		t.Errorf("crosslinked session not deduped: in=2 out=%d", len(out))
	}

	// Checkpoint and commit duplicates pass through untouched — the web does
	// not dedupe repo-bound types.
	for _, typ := range []string{search.TypeCheckpoint, search.TypeCommit} {
		if out := dedupSemanticResults([]search.Result{
			v4RepoScopedRow(typ, "id-x", "backend", 0.9),
			v4RepoScopedRow(typ, "id-x", "backend", 0.8),
		}); len(out) != 2 {
			t.Errorf("%s rows deduped: in=2 out=%d, want pass-through", typ, len(out))
		}
	}
}

// --- helpers -----------------------------------------------------------------

func fptr(f float64) *float64 { return &f }
func iptr(i int) *int         { return &i }

// v4Ckpt builds a checkpoint result with the given id and ranking metadata.
// tier < 0 means "no tier field" (the ANN-only fallback shape).
func v4Ckpt(id string, tier int, meta search.Meta) search.Result {
	if tier >= 0 {
		meta.Tier = iptr(tier)
	}
	return search.Result{
		Type:       search.TypeCheckpoint,
		Meta:       meta,
		Checkpoint: &search.CheckpointResult{ID: id},
	}
}

// v4Commit builds a tier-1 commit result (every merge test uses tier 1 for
// commits; tier variation is exercised via checkpoints).
func v4Commit(sha string, meta search.Meta) search.Result {
	meta.Tier = iptr(1)
	return search.Result{
		Type:   search.TypeCommit,
		Meta:   meta,
		Commit: &search.CommitResult{CommitSHA: sha},
	}
}

// v4RepoScopedRow builds a result whose dedup identity is repo-qualified: a
// server-folded legacy session (ENT-1595, empty sessionId), a raw checkpoint, or
// a commit — all under org acme, with the given repo. id is the checkpoint id /
// commit SHA the row carries.
func v4RepoScopedRow(typ, id, repo string, score float64) search.Result {
	r := search.Result{Type: typ, Meta: search.Meta{Score: score}}
	switch typ {
	case search.TypeSession:
		r.Session = &search.SessionResult{SessionID: "", MatchedCheckpointID: id, Org: "acme", Repo: repo}
	case search.TypeCheckpoint:
		r.Checkpoint = &search.CheckpointResult{ID: id, Org: "acme", Repo: repo}
	case search.TypeCommit:
		r.Commit = &search.CommitResult{CommitSHA: id, Org: "acme", Repo: repo}
	}
	return r
}

// v4Session builds a session row with a GLOBAL session id (the primary dedup
// key) in the given repo.
func v4Session(sessionID, repo string, score float64) search.Result {
	return search.Result{
		Type: search.TypeSession,
		Meta: search.Meta{Score: score, Tier: iptr(1)},
		Session: &search.SessionResult{
			SessionID: sessionID, Org: "acme", Repo: repo,
		},
	}
}

// v4RepoRow builds a repo-type result via the wire format, since repo rows
// have no typed struct (payload lives in rawData).
func v4RepoRow(t *testing.T, id string, score float64) search.Result {
	t.Helper()
	raw := `{"type":"repo","data":{"id":"` + id + `","name":"x"},"searchMeta":{"score":` + jsonFloat(score) + `}}`
	var r search.Result
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("building repo row: %v", err)
	}
	return r
}

func jsonFloat(f float64) string {
	b, _ := json.Marshal(f) //nolint:errcheck,errchkjson // float64 cannot fail to marshal
	return string(b)
}

func v4CellOK(resp *search.Response) cellCallResult[*search.Response] {
	return cellCallResult[*search.Response]{value: resp}
}

func v4CellErr(err error) cellCallResult[*search.Response] {
	return cellCallResult[*search.Response]{err: err}
}

func v4ResultIDs(t *testing.T, results []search.Result) []string {
	t.Helper()
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].ResultID()
	}
	return ids
}

// --- merge: tier ordering ------------------------------------------------------

// TestMergeSemanticV4Responses_TierOrdering verifies the cross-cell interleave
// applies the shared ordering contract: repos first, then tier 0 and tier 1
// EACH by rerank score desc (ENT-1425/ENT-1431), tier-2 dropped — regardless
// of which cell each result came from. The BM25/Score values are set
// to the OPPOSITE order from the rerank scores so the assertion fails if the
// merge ever regresses to sorting by BM25 (tier 0) or Score (tier 1).
func TestMergeSemanticV4Responses_TierOrdering(t *testing.T) {
	t.Parallel()

	cellA := &search.Response{Results: []search.Result{
		// tier-1: high Score but LOW rerank → must sort below b-t1 (higher rerank).
		v4Ckpt("a-t1-rr-lo", 1, search.Meta{Score: 0.9, RerankScore: fptr(0.20)}),
		// tier-0: high BM25 but LOW rerank → must sort below b-t0 (higher rerank).
		v4Ckpt("a-t0-rr-lo", 0, search.Meta{BM25Score: fptr(9.0), RerankScore: fptr(0.20)}),
		// tier-2 alongside upper tiers in the same cell → promoted.
		v4Ckpt("a-t2", 2, search.Meta{ANNScore: fptr(0.30)}),
	}, Total: 3}
	cellB := &search.Response{Results: []search.Result{
		v4RepoRow(t, "repo-1", 0.9),
		v4Ckpt("b-t0-rr-hi", 0, search.Meta{BM25Score: fptr(3.0), RerankScore: fptr(0.80)}),
		v4Ckpt("b-t1-rr-hi", 1, search.Meta{Score: 0.1, RerankScore: fptr(0.90)}),
		v4Ckpt("b-t2", 2, search.Meta{ANNScore: fptr(0.10)}),
	}, Total: 4}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Tier-2 rows (a-t2, b-t2) are dropped — the retired ANN-only fallback
	// never merges, matching the BFF.
	want := []string{"repo-1", "b-t0-rr-hi", "a-t0-rr-lo", "b-t1-rr-hi", "a-t1-rr-lo"}
	got := v4ResultIDs(t, resp.Results)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged order = %v, want %v", got, want)
	}
	if resp.Total != 5 {
		t.Errorf("total = %d, want 5 (derived from the final list)", resp.Total)
	}
	if resp.Page != 1 {
		t.Errorf("page = %d, want 1", resp.Page)
	}
}

// TestMergeSemanticV4Responses_Tier0MixedCapabilityFallback verifies the tier-0
// fallback used when not every row was reranked (a cell predating tier-0
// reranking): rows interleave positionally by their rank within their own cell
// (each cell's #1, then each cell's #2, …), BM25 breaking ties between same-rank
// rows — never a global rerank sort that would demote the un-scored cell's hits.
func TestMergeSemanticV4Responses_Tier0MixedCapabilityFallback(t *testing.T) {
	t.Parallel()

	// cellA is a current cell (rows carry a rerank score); cellB predates tier-0
	// reranking (no rerank score) — so the whole band uses the positional fallback.
	cellA := &search.Response{Results: []search.Result{
		v4Ckpt("a0", 0, search.Meta{BM25Score: fptr(1.0), RerankScore: fptr(0.90)}), // cellRank 0
		v4Ckpt("a1", 0, search.Meta{BM25Score: fptr(2.0), RerankScore: fptr(0.80)}), // cellRank 1
	}, Total: 2}
	cellB := &search.Response{Results: []search.Result{
		v4Ckpt("b0", 0, search.Meta{BM25Score: fptr(5.0)}), // cellRank 0, no rerank score
		v4Ckpt("b1", 0, search.Meta{BM25Score: fptr(4.0)}), // cellRank 1, no rerank score
	}, Total: 2}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}

	// rank 0: b0 (BM25 5) before a0 (BM25 1); rank 1: b1 (BM25 4) before a1 (BM25 2).
	want := []string{"b0", "a0", "b1", "a1"}
	got := v4ResultIDs(t, resp.Results)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged order = %v, want %v", got, want)
	}
}

// TestMergeSemanticV4Responses_EqualRerankTieBreaks verifies the secondary keys
// when rerank scores tie: tier 0 falls back to BM25 desc and tier 1 to retrieval
// Score desc, matching the BFF's `|| bm25Of` / `|| scoreOf` tie-breaks.
func TestMergeSemanticV4Responses_EqualRerankTieBreaks(t *testing.T) {
	t.Parallel()

	cell := &search.Response{Results: []search.Result{
		// tier 0, equal rerank → BM25 desc decides: t0-hi before t0-lo.
		v4Ckpt("t0-lo", 0, search.Meta{RerankScore: fptr(0.50), BM25Score: fptr(2.0)}),
		v4Ckpt("t0-hi", 0, search.Meta{RerankScore: fptr(0.50), BM25Score: fptr(8.0)}),
		// tier 1, equal rerank → Score desc decides: t1-hi before t1-lo.
		v4Ckpt("t1-lo", 1, search.Meta{RerankScore: fptr(0.30), Score: 0.1}),
		v4Ckpt("t1-hi", 1, search.Meta{RerankScore: fptr(0.30), Score: 0.9}),
	}, Total: 4}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(cell),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"t0-hi", "t0-lo", "t1-hi", "t1-lo"}
	got := v4ResultIDs(t, resp.Results)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged order = %v, want %v", got, want)
	}
}

// TestMergeSemanticV4Responses_PagePassthrough confirms the requested page is
// reflected in the merged response (the TUI's fetch-more pages server-side).
func TestMergeSemanticV4Responses_PagePassthrough(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 3, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("a", 1, search.Meta{Score: 0.9}),
		}, Total: 21}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Page != 3 {
		t.Errorf("page = %d, want the requested page 3", resp.Page)
	}
}

// TestMergeSemanticV4Responses_TierlessAndTier2Dropped verifies rows outside
// tiers 0/1 — the retired ANN-only fallback (tier 2) and rows with no tier at
// all — never merge, even when they are all a cell returned. The BFF drops
// them identically (ENT-1777).
func TestMergeSemanticV4Responses_TierlessAndTier2Dropped(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("tierless", -1, search.Meta{ANNScore: fptr(0.01)}),
			v4Ckpt("tier2", 2, search.Meta{ANNScore: fptr(0.02)}),
		}, Total: 2}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 0 {
		t.Errorf("results = %v, want none (tier-2/tier-less rows dropped)", v4ResultIDs(t, resp.Results))
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0 (derived from the final list)", resp.Total)
	}
}

// TestMergeSemanticV4Responses_PerTypeWindowAndLowerBounds verifies the BFF's
// per-type window: at most `limit` rows of each facet survive, overflowing
// facets are flagged in truncated_types, and counts_lower_bound mirrors them —
// counts describe what the caller can see, never the cells' corpus counts.
func TestMergeSemanticV4Responses_PerTypeWindowAndLowerBounds(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 1, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4RepoRow(t, "repo-1", 0.9),
			v4Ckpt("ck-a", 1, search.Meta{Score: 0.9}),
			v4Ckpt("ck-b", 1, search.Meta{Score: 0.8}),
			v4Commit("sha-a", search.Meta{Score: 0.7}),
			v4Commit("sha-b", search.Meta{Score: 0.6}),
		}, Total: 5, Counts: &search.TypeCounts{Repos: 1, Checkpoints: 999, Commits: 999}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v4ResultIDs(t, resp.Results)
	want := []string{"repo-1", "ck-a", "sha-a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("results = %v, want %v (one per facet at limit 1)", got, want)
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3 — derived from the final list, never the cells' corpus counts", resp.Total)
	}
	if resp.Counts.Checkpoints != 1 || resp.Counts.Commits != 1 || resp.Counts.Repos != 1 {
		t.Errorf("counts = %+v, want 1/1/1 derived", resp.Counts)
	}
	for _, facet := range []string{"checkpoints", "commits", "repos"} {
		wantFlag := facet != "repos"
		if resp.TruncatedTypes[facet] != wantFlag {
			t.Errorf("truncated_types[%s] = %v, want %v", facet, resp.TruncatedTypes[facet], wantFlag)
		}
		if resp.CountsLowerBound[facet] != wantFlag {
			t.Errorf("counts_lower_bound[%s] = %v, want %v", facet, resp.CountsLowerBound[facet], wantFlag)
		}
	}
}

// TestMergeSemanticV4Responses_FallbackDroppedWhenUpperTiersExist documents
// that a cell whose page is entirely tier-2 contributes nothing — tier-2 is
// dropped unconditionally — and its corpus Total is never advertised: totals
// and counts derive from the final merged list alone (ENT-1777).
func TestMergeSemanticV4Responses_FallbackDroppedWhenUpperTiersExist(t *testing.T) {
	t.Parallel()

	upper := &search.Response{
		Results: []search.Result{v4Ckpt("good", 1, search.Meta{Score: 0.7})},
		Total:   1,
		Counts:  &search.TypeCounts{Checkpoints: 1},
	}
	fallbackOnly := &search.Response{
		Results: []search.Result{v4Ckpt("ann-only", 2, search.Meta{ANNScore: fptr(0.2)})},
		Total:   500,
		Counts:  &search.TypeCounts{Checkpoints: 500},
	}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(upper), v4CellOK(fallbackOnly),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v4ResultIDs(t, resp.Results)
	if len(got) != 1 || got[0] != "good" {
		t.Errorf("results = %v, want only [good] (all-tier-2 cell's fallback dropped)", got)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1 — the discarded cell's 500 unreachable matches must not be advertised", resp.Total)
	}
	if resp.Counts.Checkpoints != 1 {
		t.Errorf("counts.Checkpoints = %d, want 1", resp.Counts.Checkpoints)
	}
}

// TestMergeSemanticV4Responses_RepoOnlyPageCountsJustRepos covers a cell whose
// page mixes a repo hit with tier-2-only rows while another cell has tier 0/1:
// the tier-2 rows are dropped by the merge, so only the repo rows may count
// toward Total/Counts (trail finding 019f807e-60a3).
func TestMergeSemanticV4Responses_RepoOnlyPageCountsJustRepos(t *testing.T) {
	t.Parallel()

	upper := &search.Response{
		Results: []search.Result{v4Ckpt("good", 1, search.Meta{Score: 0.7})},
		Total:   1,
		Counts:  &search.TypeCounts{Checkpoints: 1},
	}
	repoPlusFallback := &search.Response{
		Results: []search.Result{
			v4RepoRow(t, "repo-1", 0.9),
			v4Ckpt("ann-only", 2, search.Meta{ANNScore: fptr(0.2)}),
		},
		Total:  50,
		Counts: &search.TypeCounts{Repos: 1, Checkpoints: 49},
	}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(upper), v4CellOK(repoPlusFallback),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v4ResultIDs(t, resp.Results)
	want := []string{"repo-1", "good"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("results = %v, want %v (repo row merged, ann-only dropped)", got, want)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 (1 upper + 1 repo row; the 49 dropped tier-2 matches are unreachable)", resp.Total)
	}
	if resp.Counts.Checkpoints != 1 || resp.Counts.Repos != 1 {
		t.Errorf("counts = %+v, want checkpoints=1 repos=1", resp.Counts)
	}
}

// --- merge: dedup ---------------------------------------------------------------

// TestMergeSemanticV4Responses_DedupAdjustsTotalsAndCounts verifies a session
// mirrored across cells (a crosslinked session, the one legitimate cross-cell
// duplicate) is kept once — first/higher-ranked copy wins — and that totals
// and counts, derived from the final list, reflect the deduped set.
func TestMergeSemanticV4Responses_DedupAdjustsTotalsAndCounts(t *testing.T) {
	t.Parallel()

	cellA := &search.Response{
		Results: []search.Result{
			v4Session("sess-dup", "backend", 0.9),
			v4Commit("sha1", search.Meta{Score: 0.6}),
		},
		Total:  2,
		Counts: &search.TypeCounts{Sessions: 1, Commits: 1},
	}
	cellB := &search.Response{
		Results: []search.Result{
			v4Session("sess-dup", "frontend", 0.4),
		},
		Total:  1,
		Counts: &search.TypeCounts{Sessions: 1},
	}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %v, want 2 (session deduped)", v4ResultIDs(t, resp.Results))
	}
	// The kept copy is cell A's higher-ranked one.
	kept := resp.Results[0]
	if kept.Type != search.TypeSession || kept.Meta.Score != 0.9 {
		t.Errorf("kept session = %+v, want the first/higher-ranked 0.9 copy", kept.Meta)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 (3 rows - 1 session dupe, derived)", resp.Total)
	}
	if resp.Counts.Sessions != 1 || resp.Counts.Commits != 1 {
		t.Errorf("counts = %+v, want sessions=1 commits=1", resp.Counts)
	}
}

// TestMergeSemanticV4Responses_RepoRowsNotDeduped documents the web-parity
// dedupe scope (ENT-1777): only sessions dedupe. Repo rows — like every other
// repo-bound type — pass through untouched, because the fan-out routes each
// repo to its single home placement (ENT-1672/ENT-1776), so a genuine
// cross-cell duplicate is structurally impossible; a fabricated one here
// stays visible rather than being silently collapsed.
func TestMergeSemanticV4Responses_RepoRowsNotDeduped(t *testing.T) {
	t.Parallel()

	mk := func(score float64) *search.Response {
		return &search.Response{
			Results: []search.Result{
				v4RepoRow(t, "repo-dup", score),
			},
			Total:  1,
			Counts: &search.TypeCounts{Repos: 1},
		}
	}
	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(mk(0.9)), v4CellOK(mk(0.4)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 || resp.Counts.Repos != 2 || resp.Total != 2 {
		t.Errorf("results=%d counts.Repos=%d total=%d, want 2/2/2 (no repo dedupe)",
			len(resp.Results), resp.Counts.Repos, resp.Total)
	}
}

// TestMergeSemanticV4Responses_SameIDDifferentTypeNotDeduped guards the dedup
// key: a commit and a checkpoint sharing an id string are distinct results.
func TestMergeSemanticV4Responses_SameIDDifferentTypeNotDeduped(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("x", 1, search.Meta{Score: 0.9}),
			v4Commit("x", search.Meta{Score: 0.8}),
		}, Total: 2}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("results = %d, want 2 (same id, different types)", len(resp.Results))
	}
}

// --- merge: failures, limits, empties -------------------------------------------

func TestMergeSemanticV4Responses_PartialFailureMergesSurvivorsWithWarning(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(errors.New("cell down")),
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("ok", 1, search.Meta{Score: 0.5}),
		}, Total: 1}),
	})
	if err != nil {
		t.Fatalf("partial failure should merge survivors, got error: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].ResultID() != "ok" {
		t.Errorf("results = %v, want [ok]", v4ResultIDs(t, resp.Results))
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "1 of 2 regions") {
		t.Errorf("warnings = %v, want a visible partial-failure warning naming 1 of 2 regions", resp.Warnings)
	}
	if !resp.Partial || !resp.CoverageIncomplete {
		t.Errorf("partial=%v coverage_incomplete=%v, want both true on a cell failure", resp.Partial, resp.CoverageIncomplete)
	}
}

// TestMergeSemanticV4Responses_UnavailableCellsSkippedQuietly covers the
// rollout reality: cells without query-serve deployed 404 on every search.
// Those cells must not produce a user-facing warning — only real failures do,
// and the warning's denominator counts only cells that have the route.
func TestMergeSemanticV4Responses_UnavailableCellsSkippedQuietly(t *testing.T) {
	t.Parallel()

	ok := &search.Response{Results: []search.Result{
		v4Ckpt("ok", 1, search.Meta{Score: 0.5}),
	}, Total: 1}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(fmt.Errorf("cell gateway: %w", search.ErrCellUnavailable)),
		v4CellErr(fmt.Errorf("resolving cell: %w", auth.ErrNoCellForJurisdiction)),
		v4CellOK(ok),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for an undeployed cell", resp.Warnings)
	}
	if len(resp.Results) != 1 {
		t.Errorf("results = %d, want 1", len(resp.Results))
	}

	// A real failure alongside an undeployed cell warns — and counts only the
	// cells that could actually serve (1 of 2, not 2 of 3).
	resp, err = mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(search.ErrCellUnavailable),
		v4CellErr(errors.New("cell down")),
		v4CellOK(ok),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "1 of 2 regions") {
		t.Errorf("warnings = %v, want a warning naming 1 of 2 regions", resp.Warnings)
	}
}

// TestMergeSemanticV4Responses_AllCellsUnavailable verifies the clear error
// when no queried cell has query-serve deployed at all.
func TestMergeSemanticV4Responses_AllCellsUnavailable(t *testing.T) {
	t.Parallel()

	_, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(search.ErrCellUnavailable),
		v4CellErr(search.ErrCellUnavailable),
	})
	if err == nil {
		t.Fatal("expected an error when every cell lacks query-serve")
	}
	if !strings.Contains(err.Error(), "not yet available") {
		t.Errorf("error = %q, want a 'not yet available' explanation", err.Error())
	}
}

func TestMergeSemanticV4Responses_AllCellsFail(t *testing.T) {
	t.Parallel()

	_, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(errors.New("cell a down")),
		v4CellErr(errors.New("cell b down")),
	})
	if err == nil {
		t.Fatal("expected an error when every cell failed")
	}
	if !strings.Contains(err.Error(), "semantic search") {
		t.Errorf("error = %q, want it labeled semantic search", err.Error())
	}
}

func TestMergeSemanticV4Responses_NoCells(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Results == nil || len(resp.Results) != 0 {
		t.Errorf("results = %v, want non-nil empty slice", resp.Results)
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestMergeSemanticV4Responses_LimitCapsResults(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 2, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("a", 1, search.Meta{Score: 0.9}),
			v4Ckpt("b", 1, search.Meta{Score: 0.8}),
			v4Ckpt("c", 1, search.Meta{Score: 0.7}),
		}, Total: 3}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("results = %d, want capped at limit 2", len(resp.Results))
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 — totals derive from the final list; the cap is reported via counts_lower_bound instead", resp.Total)
	}
	if !resp.CountsLowerBound["checkpoints"] || !resp.TruncatedTypes["checkpoints"] {
		t.Errorf("lower-bound flags = %v/%v, want checkpoints flagged", resp.CountsLowerBound, resp.TruncatedTypes)
	}
}

func TestMergeSemanticV4Responses_NilCountsBodiesTolerated(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("a", 1, search.Meta{Score: 0.9}),
		}, Total: 1}), // no Counts
		v4CellOK(&search.Response{Results: []search.Result{
			v4Commit("sha", search.Meta{Score: 0.5}),
		}, Total: 1, Counts: &search.TypeCounts{Commits: 1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Counts == nil || resp.Counts.Commits != 1 || resp.Counts.Checkpoints != 1 {
		t.Errorf("counts = %+v, want commits=1 checkpoints=1 derived from the merged list", resp.Counts)
	}
}

// TestMergeSemanticV4Responses_FlagSynthesis mirrors the BFF's flag rules:
// truncated = any cell truncated; partial adds cell failures, cell-reported
// partial, and mixed rerank capability; coverage_incomplete excludes only the
// rerank-mix case; reranked = every cell reranked; the cells' own
// truncated_types union into the merged map.
func TestMergeSemanticV4Responses_FlagSynthesis(t *testing.T) {
	t.Parallel()

	tr, fa := true, false
	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{
			Results:        []search.Result{v4Ckpt("a", 1, search.Meta{Score: 0.9, RerankScore: fptr(0.5)})},
			Total:          1,
			Truncated:      true,
			TruncatedTypes: map[string]bool{"sessions": true},
			Reranked:       &tr,
		}),
		v4CellOK(&search.Response{
			Results:  []search.Result{v4Ckpt("b", 1, search.Meta{Score: 0.8})},
			Total:    1,
			Reranked: &fa,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated {
		t.Error("truncated: want true when any cell truncated")
	}
	if !resp.Partial || !resp.CoverageIncomplete {
		t.Errorf("partial=%v coverage=%v, want both (truncation implies both)", resp.Partial, resp.CoverageIncomplete)
	}
	if resp.Reranked == nil || *resp.Reranked {
		t.Errorf("reranked = %v, want false (not every cell reranked)", resp.Reranked)
	}
	if !resp.TruncatedTypes["sessions"] {
		t.Errorf("truncated_types = %v, want the cell's sessions flag unioned in", resp.TruncatedTypes)
	}

	// Mixed rerank capability alone: partial without coverage_incomplete.
	resp, err = mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{v4Ckpt("a", 1, search.Meta{Score: 0.9})}, Total: 1, Reranked: &tr}),
		v4CellOK(&search.Response{Results: []search.Result{v4Ckpt("b", 1, search.Meta{Score: 0.8})}, Total: 1, Reranked: &fa}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Partial || resp.CoverageIncomplete {
		t.Errorf("partial=%v coverage=%v, want partial-only for a rerank mix", resp.Partial, resp.CoverageIncomplete)
	}
}

// --- searcher construction -------------------------------------------------------

// TestNewSemanticSearcher_RejectsMultipleRepoFilters confirms the searcher
// validates repo filters up front (previously the v3 request builder's job),
// so a TUI re-search typing several repo: filters errors before any network.
func TestNewSemanticSearcher_RejectsMultipleRepoFilters(t *testing.T) {
	t.Parallel()
	searcher := newSemanticSearcher(false)
	_, err := searcher(context.Background(), search.Config{
		Query: "q",
		Repos: []string{"a/b", "c/d", "e/f"},
	})
	if err == nil {
		t.Fatal("expected a validation error before any network access")
	}
}

// TestMergeSemanticV4Responses_AllCellsRepoUnmatched verifies the error when
// every queried cell answered but none matched the repo filter (not indexed,
// or the owner org isn't flag-enabled — a typo can't reach this point, the
// slug already resolved). The old behavior lumped this in with undeployed
// cells and told the user their REGION lacked semantic search — a
// misdiagnosis that sent a flag-enrollment gap to the wrong team.
func TestMergeSemanticV4Responses_AllCellsRepoUnmatched(t *testing.T) {
	t.Parallel()

	_, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(fmt.Errorf("cell a: %w", search.ErrRepoFilterUnmatched)),
		v4CellErr(search.ErrRepoFilterUnmatched),
	})
	if err == nil {
		t.Fatal("expected an error when no cell matched the repo filter")
	}
	if strings.Contains(err.Error(), "region") {
		t.Errorf("error = %q, must not blame the region for a repo-filter miss", err.Error())
	}
	if !strings.Contains(err.Error(), "repo") || !strings.Contains(err.Error(), "enabled") {
		t.Errorf("error = %q, want it to point at the repo name, access, or semantic-search enablement", err.Error())
	}
}

// TestMergeSemanticV4Responses_RepoUnmatchedBeatsRegionMessage: when some
// cells lack query-serve AND one answered "repo unmatched", the repo message
// wins — a cell answering proves the region serves semantic search.
func TestMergeSemanticV4Responses_RepoUnmatchedBeatsRegionMessage(t *testing.T) {
	t.Parallel()

	_, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(search.ErrCellUnavailable),
		v4CellErr(search.ErrRepoFilterUnmatched),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "region") {
		t.Errorf("error = %q, must not blame the region when a cell answered", err.Error())
	}
}

// TestMergeSemanticV4Responses_RepoUnmatchedQuietWithResults: a repo-unmatched
// cell alongside a successful page is skipped quietly, like an undeployed
// cell — no partial-failure warning, results still returned.
func TestMergeSemanticV4Responses_RepoUnmatchedQuietWithResults(t *testing.T) {
	t.Parallel()

	ok := &search.Response{Results: []search.Result{
		v4Ckpt("ok", 1, search.Meta{Score: 0.5}),
	}, Total: 1}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellErr(search.ErrRepoFilterUnmatched),
		v4CellOK(ok),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for a repo-unmatched cell beside results", resp.Warnings)
	}
	if len(resp.Results) != 1 {
		t.Errorf("results = %d, want 1", len(resp.Results))
	}
}
