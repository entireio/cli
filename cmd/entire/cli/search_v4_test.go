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

func v4Commit(sha string, tier int, meta search.Meta) search.Result {
	if tier >= 0 {
		meta.Tier = iptr(tier)
	}
	return search.Result{
		Type:   search.TypeCommit,
		Meta:   meta,
		Commit: &search.CommitResult{CommitSHA: sha},
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
// applies query-serve's own ordering: repos first, then tier-0 by BM25 desc,
// tier-1 by rerank score desc, then promoted tier-2 by ANN asc — regardless of
// which cell each result came from.
func TestMergeSemanticV4Responses_TierOrdering(t *testing.T) {
	t.Parallel()

	cellA := &search.Response{Results: []search.Result{
		v4Ckpt("a-t1-low", 1, search.Meta{Score: 0.5}),
		v4Ckpt("a-t0-high", 0, search.Meta{BM25Score: fptr(9.0)}),
		// tier-2 alongside upper tiers in the same cell → promoted.
		v4Ckpt("a-t2", 2, search.Meta{ANNScore: fptr(0.30)}),
	}, Total: 3}
	cellB := &search.Response{Results: []search.Result{
		v4RepoRow(t, "repo-1", 0.9),
		v4Ckpt("b-t0-low", 0, search.Meta{BM25Score: fptr(3.0)}),
		v4Ckpt("b-t1-high", 1, search.Meta{Score: 0.8}),
		v4Ckpt("b-t2", 2, search.Meta{ANNScore: fptr(0.10)}),
	}, Total: 4}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"repo-1", "a-t0-high", "b-t0-low", "b-t1-high", "a-t1-low", "b-t2", "a-t2"}
	got := v4ResultIDs(t, resp.Results)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged order = %v, want %v", got, want)
	}
	if resp.Total != 7 {
		t.Errorf("total = %d, want 7", resp.Total)
	}
	if resp.Page != 1 {
		t.Errorf("page = %d, want 1", resp.Page)
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

// TestMergeSemanticV4Responses_ANNFallback verifies that when no cell produced
// tier 0/1 results, the ANN-only tail is shown, ordered ANN asc and capped at
// mergedTier2Max. Results without a tier field count as the fallback tier.
func TestMergeSemanticV4Responses_ANNFallback(t *testing.T) {
	t.Parallel()

	var results []search.Result
	for i := range mergedTier2Max + 5 {
		results = append(results, v4Ckpt(
			"c-"+string(rune('a'+i)), -1,
			search.Meta{ANNScore: fptr(float64(i) / 100)},
		))
	}
	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: results, Total: len(results)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != mergedTier2Max {
		t.Errorf("fallback results = %d, want capped at %d", len(resp.Results), mergedTier2Max)
	}
	// ANN asc: the lowest scores survive the cap, in ascending order.
	for i := 1; i < len(resp.Results); i++ {
		if *resp.Results[i-1].Meta.ANNScore > *resp.Results[i].Meta.ANNScore {
			t.Errorf("fallback not sorted ANN asc at %d", i)
		}
	}
}

// TestMergeSemanticV4Responses_FallbackCapAfterDedup guards the cap ordering:
// cross-cell duplicates in the fallback tail must not shrink the visible page
// below mergedTier2Max when enough unique results exist.
func TestMergeSemanticV4Responses_FallbackCapAfterDedup(t *testing.T) {
	t.Parallel()

	// Two cells return the same 20 ANN-only checkpoints (mirrored repo).
	mk := func() []search.Result {
		var rs []search.Result
		for i := range mergedTier2Max + 5 {
			rs = append(rs, v4Ckpt(
				"c-"+string(rune('a'+i)), -1,
				search.Meta{ANNScore: fptr(float64(i) / 100)},
			))
		}
		return rs
	}
	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: mk(), Total: mergedTier2Max + 5}),
		v4CellOK(&search.Response{Results: mk(), Total: mergedTier2Max + 5}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != mergedTier2Max {
		t.Errorf("fallback results = %d, want the full cap %d despite duplicates (dedup must run before the cap)", len(resp.Results), mergedTier2Max)
	}
}

// TestMergeSemanticV4Responses_FallbackDroppedWhenUpperTiersExist documents
// that a cell whose page is entirely tier-2 (its ANN fallback) contributes
// nothing when another cell produced tier 0/1 — matching how query-serve only
// shows the fallback tail when there is nothing better. Its Total must be
// excluded too: those matches are unreachable, so they must not be advertised.
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

// TestMergeSemanticV4Responses_DedupAdjustsTotalsAndCounts verifies a result
// mirrored across cells is kept once (first/higher-ranked wins) and that both
// the total and the per-type counts are reduced accordingly.
func TestMergeSemanticV4Responses_DedupAdjustsTotalsAndCounts(t *testing.T) {
	t.Parallel()

	cellA := &search.Response{
		Results: []search.Result{
			v4Ckpt("dup", 1, search.Meta{Score: 0.9}),
			v4Commit("sha1", 1, search.Meta{Score: 0.6}),
		},
		Total:  2,
		Counts: &search.TypeCounts{Checkpoints: 1, Commits: 1},
	}
	cellB := &search.Response{
		Results: []search.Result{
			v4Ckpt("dup", 1, search.Meta{Score: 0.4}),
		},
		Total:  1,
		Counts: &search.TypeCounts{Checkpoints: 1},
	}

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(cellA), v4CellOK(cellB),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := v4ResultIDs(t, resp.Results)
	want := []string{"dup", "sha1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("results = %v, want %v", got, want)
	}
	// The kept "dup" is cell A's higher-ranked copy.
	if resp.Results[0].Meta.Score != 0.9 {
		t.Errorf("kept dup score = %v, want the first/higher-ranked 0.9", resp.Results[0].Meta.Score)
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2 (3 rows - 1 dupe)", resp.Total)
	}
	if resp.Counts.Checkpoints != 1 || resp.Counts.Commits != 1 {
		t.Errorf("counts = %+v, want checkpoints=1 commits=1", resp.Counts)
	}
}

// TestMergeSemanticV4Responses_RepoRowsDedupAcrossCells verifies the mirrored-
// repo case the fan-out exists for: both cells return the same logical repo
// row, which must appear once (repo rows carry their id in the raw payload).
func TestMergeSemanticV4Responses_RepoRowsDedupAcrossCells(t *testing.T) {
	t.Parallel()

	mk := func(score float64) *search.Response {
		return &search.Response{
			Results: []search.Result{
				v4RepoRow(t, "repo-dup", score),
				v4Ckpt("ck-"+jsonFloat(score), 1, search.Meta{Score: score}),
			},
			Total:  2,
			Counts: &search.TypeCounts{Repos: 1, Checkpoints: 1},
		}
	}
	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(mk(0.9)), v4CellOK(mk(0.4)),
	})
	if err != nil {
		t.Fatal(err)
	}
	repoRows := 0
	for _, r := range resp.Results {
		if r.Type == search.TypeRepo {
			repoRows++
		}
	}
	if repoRows != 1 {
		t.Errorf("repo rows = %d, want 1 (mirrored repo deduped across cells)", repoRows)
	}
	if resp.Counts.Repos != 1 {
		t.Errorf("counts.Repos = %d, want 1 after dedup", resp.Counts.Repos)
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3 (4 rows - 1 repo dupe)", resp.Total)
	}
}

// TestMergeSemanticV4Responses_SameIDDifferentTypeNotDeduped guards the dedup
// key: a commit and a checkpoint sharing an id string are distinct results.
func TestMergeSemanticV4Responses_SameIDDifferentTypeNotDeduped(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("x", 1, search.Meta{Score: 0.9}),
			v4Commit("x", 1, search.Meta{Score: 0.8}),
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
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3 (limit caps the page, not the total)", resp.Total)
	}
}

func TestMergeSemanticV4Responses_NilCountsBodiesTolerated(t *testing.T) {
	t.Parallel()

	resp, err := mergeSemanticV4Responses(context.Background(), 0, 0, []cellCallResult[*search.Response]{
		v4CellOK(&search.Response{Results: []search.Result{
			v4Ckpt("a", 1, search.Meta{Score: 0.9}),
		}, Total: 1}), // no Counts
		v4CellOK(&search.Response{Results: []search.Result{
			v4Commit("sha", 1, search.Meta{Score: 0.5}),
		}, Total: 1, Counts: &search.TypeCounts{Commits: 1}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Counts == nil || resp.Counts.Commits != 1 || resp.Counts.Checkpoints != 0 {
		t.Errorf("counts = %+v, want commits=1 from the one counted body", resp.Counts)
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
