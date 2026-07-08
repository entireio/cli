package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/codesearch"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/internal/coreapi"
)

// test constants used across code-search tests.
const (
	testRepoID1       = "01ABC"
	testRepoID2       = "02DEF"
	testCellEU        = "aws-eu-west-1"
	testClusterSlugUS = "us-prod"
)

// TestSearchCmd_AccessibleModeRequiresQuery verifies that accessible mode
// is treated like --json: a query is required when ACCESSIBLE=1.
// Note: this test modifies process-global state (env var), so it must NOT
// use t.Parallel().
func TestSearchCmd_AccessibleModeRequiresQuery(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--json"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no query with --json + ACCESSIBLE=1")
	}

	want := "query required when using --json, accessible mode, or piped output"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err.Error(), want)
	}
}

func TestSearchCmd_HelpMentionsRepoFlagAndInlineFilters(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"search", "-h"})

	if err := root.Execute(); err != nil {
		t.Fatalf("help command failed: %v", err)
	}

	help := buf.String()
	if !strings.Contains(help, "--repo") {
		t.Fatalf("help missing --repo flag:\n%s", help)
	}
	if !strings.Contains(help, "inline filters") {
		t.Fatalf("help missing inline filter note:\n%s", help)
	}
	if !strings.Contains(help, "repo:*") {
		t.Fatalf("help missing repo:* inline example:\n%s", help)
	}
}

func TestWriteSearchJSON_ZeroLimitFallsBackToDefaultPageSize(t *testing.T) {
	t.Parallel()

	resp := &search.Response{
		Results: testResults(),
		Total:   2,
		Page:    1,
	}

	var buf bytes.Buffer
	if err := writeSearchJSON(&buf, resp, 0, 1); err != nil {
		t.Fatalf("writeSearchJSON returned error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"limit": 10`) {
		t.Fatalf("output missing default limit fallback:\n%s", output)
	}
	if !strings.Contains(output, `"total_pages": 1`) {
		t.Fatalf("output missing total_pages:\n%s", output)
	}
}

func TestCodeSearchEnabled_EnvGate(t *testing.T) {
	// Modifies process-global env, no t.Parallel().
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"true", false},
		{"1", true},
	} {
		t.Setenv("ENTIRE_CODE_SEARCH", tc.val)
		if got := codeSearchEnabled(); got != tc.want {
			t.Errorf("ENTIRE_CODE_SEARCH=%q: codeSearchEnabled() = %v, want %v", tc.val, got, tc.want)
		}
	}
}

func TestSearchCmd_CodeFlagGated(t *testing.T) {
	// --code without ENTIRE_CODE_SEARCH should fail with gate message.
	t.Setenv("ENTIRE_CODE_SEARCH", "")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "test query"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --code used without ENTIRE_CODE_SEARCH")
	}
	if !strings.Contains(err.Error(), "not yet available") {
		t.Errorf("error = %q, want containing 'not yet available'", err.Error())
	}
	if strings.Contains(err.Error(), "ENTIRE_CODE_SEARCH") {
		t.Errorf("gate error should not mention env var, got: %q", err.Error())
	}
}

func TestSearchCmd_CodeFlagRequiresQuery(t *testing.T) {
	t.Setenv("ENTIRE_CODE_SEARCH", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --code used without query")
	}
	if !strings.Contains(err.Error(), "query required for code search") {
		t.Errorf("error = %q, want containing 'query required'", err.Error())
	}
}

func TestSearchCmd_CaseSensitiveWithoutCode(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"search", "--case-sensitive", "--json", "test"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --case-sensitive used without --code")
	}
	if !strings.Contains(err.Error(), "--case-sensitive can only be used with --code") {
		t.Errorf("error = %q, want containing '--case-sensitive can only be used with --code'", err.Error())
	}
}

func TestWriteCodeSearchText(t *testing.T) {
	t.Parallel()

	resp := &codesearch.SearchResponse{
		Stats: codesearch.Stats{TotalMatches: 2, TotalFiles: 1, ReposSearched: 1, DurationMs: 15},
		Results: []codesearch.Result{
			{Repo: "entireio/cli", Path: "main.go", Line: 10, ContextLine: "func main() {"},
			{Repo: "entireio/cli", Path: "main.go", Line: 42, ContextLine: "\tfmt.Println(\"hello\")"},
		},
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp)

	output := buf.String()
	if !strings.Contains(output, "entireio/cli:main.go:10: func main() {") {
		t.Errorf("output missing first result:\n%s", output)
	}
	if !strings.Contains(output, "2 matches across 1 files") {
		t.Errorf("output missing summary line:\n%s", output)
	}
}

func TestWriteCodeSearchJSON(t *testing.T) {
	t.Parallel()

	resp := &codesearch.SearchResponse{
		Query:     "handleRequest",
		Stats:     codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1, DurationMs: 5},
		RepoStats: []codesearch.RepoStats{{Repo: "r", MatchCount: 1, FileCount: 1}},
		Results:   []codesearch.Result{{Repo: "r", Path: "f.go", Line: 1, ContextLine: "package main"}},
	}

	var buf bytes.Buffer
	if err := writeCodeSearchJSON(&buf, resp); err != nil {
		t.Fatalf("writeCodeSearchJSON error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"query": "handleRequest"`) {
		t.Errorf("output missing query echo:\n%s", output)
	}
	if !strings.Contains(output, `"total": 1`) {
		t.Errorf("output missing total:\n%s", output)
	}
	if !strings.Contains(output, `"path": "f.go"`) {
		t.Errorf("output missing result path:\n%s", output)
	}
	if !strings.Contains(output, `"repo_stats"`) {
		t.Errorf("output missing repo_stats:\n%s", output)
	}
}

func TestWriteCodeSearchText_TruncatesLongLines(t *testing.T) {
	t.Parallel()

	longLine := strings.Repeat("x", 300)
	resp := &codesearch.SearchResponse{
		Stats:   codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1, DurationMs: 1},
		Results: []codesearch.Result{{Repo: "r", Path: "f.go", Line: 1, ContextLine: longLine}},
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp)

	output := buf.String()
	if strings.Contains(output, longLine) {
		t.Error("expected long context_line to be truncated")
	}
	if !strings.Contains(output, "…") {
		t.Error("expected truncated line to end with ellipsis")
	}
	// The prefix + 200 chars + ellipsis should be present.
	truncated := strings.Repeat("x", maxContextLineLen)
	if !strings.Contains(output, truncated+"…") {
		t.Error("expected exactly maxContextLineLen characters before ellipsis")
	}
}

func TestWriteCodeSearchText_Empty(t *testing.T) {
	t.Parallel()

	resp := &codesearch.SearchResponse{
		Stats: codesearch.Stats{},
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp)

	if !strings.Contains(buf.String(), "No code search results found") {
		t.Errorf("expected empty results message, got:\n%s", buf.String())
	}
}

func TestMergeSearchResults(t *testing.T) {
	t.Parallel()

	results := []cellCallResult[*codesearch.SearchResponse]{
		{
			group: cellGroup{cell: "aws-us-east-2", jurisdiction: "us"},
			value: &codesearch.SearchResponse{
				Query: "handleRequest",
				Stats: codesearch.Stats{TotalMatches: 3, TotalFiles: 2, ReposSearched: 1, DurationMs: 10},
				Results: []codesearch.Result{
					{Repo: "acme/web", Path: "main.go", Line: 1, Score: 0.5},
				},
				RepoStats: []codesearch.RepoStats{{Repo: "acme/web", MatchCount: 3}},
			},
		},
		{
			group: cellGroup{cell: testCellEU, jurisdiction: "eu"},
			value: &codesearch.SearchResponse{
				Query: "handleRequest",
				Stats: codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1, DurationMs: 20},
				Results: []codesearch.Result{
					{Repo: "acme/docs", Path: "handler.go", Line: 5, Score: 0.9},
				},
				RepoStats: []codesearch.RepoStats{{Repo: "acme/docs", MatchCount: 1}},
			},
		},
	}

	merged, err := mergeSearchResults(context.Background(), 0, results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if merged.Stats.TotalMatches != 4 {
		t.Errorf("TotalMatches = %d, want 4 (summed from cells)", merged.Stats.TotalMatches)
	}
	if merged.Stats.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3 (summed from cells)", merged.Stats.TotalFiles)
	}
	if merged.Stats.ReposSearched != 2 {
		t.Errorf("ReposSearched = %d, want 2", merged.Stats.ReposSearched)
	}
	if merged.Stats.DurationMs != 20 {
		t.Errorf("DurationMs = %v, want 20 (slowest cell)", merged.Stats.DurationMs)
	}
	if len(merged.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(merged.Results))
	}
	if merged.Results[0].Repo != "acme/docs" {
		t.Errorf("Results[0].Repo = %q, want acme/docs (higher score)", merged.Results[0].Repo)
	}
	if len(merged.RepoStats) != 2 {
		t.Fatalf("len(RepoStats) = %d, want 2", len(merged.RepoStats))
	}
}

func TestMergeSearchResults_Truncation(t *testing.T) {
	t.Parallel()

	results := []cellCallResult[*codesearch.SearchResponse]{
		{
			group: cellGroup{cell: "aws-us-east-2", jurisdiction: "us"},
			value: &codesearch.SearchResponse{
				Results: []codesearch.Result{
					{Repo: "a", Path: "1.go", Score: 0.9},
					{Repo: "a", Path: "2.go", Score: 0.7},
				},
				Stats: codesearch.Stats{TotalMatches: 2},
			},
		},
		{
			group: cellGroup{cell: testCellEU, jurisdiction: "eu"},
			value: &codesearch.SearchResponse{
				Results: []codesearch.Result{
					{Repo: "b", Path: "3.go", Score: 0.8},
					{Repo: "b", Path: "4.go", Score: 0.6},
				},
				Stats: codesearch.Stats{TotalMatches: 2},
			},
		},
	}

	merged, err := mergeSearchResults(context.Background(), 3, results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(merged.Results) != 3 {
		t.Fatalf("len(Results) = %d, want 3 (truncated to limit)", len(merged.Results))
	}
	if merged.Results[0].Score != 0.9 || merged.Results[1].Score != 0.8 || merged.Results[2].Score != 0.7 {
		t.Errorf("results not sorted by score: %v, %v, %v",
			merged.Results[0].Score, merged.Results[1].Score, merged.Results[2].Score)
	}
}

func TestMergeSearchResults_PartialCellError(t *testing.T) {
	t.Parallel()

	results := []cellCallResult[*codesearch.SearchResponse]{
		{
			group: cellGroup{cell: "aws-us-east-2", jurisdiction: "us"},
			value: &codesearch.SearchResponse{
				Query:   "test",
				Stats:   codesearch.Stats{TotalMatches: 2, TotalFiles: 1, ReposSearched: 1, DurationMs: 5},
				Results: []codesearch.Result{{Repo: "acme/web", Path: "f.go", Line: 1}},
			},
		},
		{
			group: cellGroup{cell: testCellEU, jurisdiction: "eu"},
			err:   errors.New("cell timed out"),
		},
	}

	merged, err := mergeSearchResults(context.Background(), 0, results)
	if err != nil {
		t.Fatalf("partial failure should not error: %v", err)
	}

	if merged.Stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2 (from successful cell)", merged.Stats.TotalMatches)
	}
	if len(merged.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1 (failed cell skipped)", len(merged.Results))
	}
	if len(merged.FailedJurisdictions) != 1 || merged.FailedJurisdictions[0] != testCellEU {
		t.Errorf("FailedJurisdictions = %v, want [aws-eu-west-1]", merged.FailedJurisdictions)
	}
}

func TestMergeSearchResults_DeduplicatesOverlappingCells(t *testing.T) {
	t.Parallel()

	dup := codesearch.Result{Repo: "acme/web", Path: "main.go", Line: 10, Column: 5, Score: 0.9}
	cellVal := func() *codesearch.SearchResponse {
		return &codesearch.SearchResponse{
			Results:   []codesearch.Result{dup},
			Stats:     codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1},
			RepoStats: []codesearch.RepoStats{{Repo: "acme/web", MatchCount: 1, FileCount: 1}},
		}
	}
	results := []cellCallResult[*codesearch.SearchResponse]{
		{group: cellGroup{cell: "", jurisdiction: ""}, value: cellVal()},
		{group: cellGroup{cell: "aws-us-east-2", jurisdiction: "us"}, value: cellVal()},
	}

	merged, err := mergeSearchResults(context.Background(), 0, results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1 (duplicate removed)", len(merged.Results))
	}
	// Stats must not double-count the overlapping match either.
	if merged.Stats.TotalMatches != 1 {
		t.Errorf("TotalMatches = %d, want 1 (overlapping cells must not double-count)", merged.Stats.TotalMatches)
	}
	if merged.Stats.ReposSearched != 1 {
		t.Errorf("ReposSearched = %d, want 1 (one logical repo)", merged.Stats.ReposSearched)
	}
	if len(merged.RepoStats) != 1 || merged.RepoStats[0].MatchCount != 1 {
		t.Errorf("RepoStats = %+v, want one entry with MatchCount 1", merged.RepoStats)
	}
}

func TestMergeSearchResults_MirrorPlacementsDoNotDoubleCount(t *testing.T) {
	t.Parallel()

	// A US-homed repo with an EU mirror indexes the same content, so the
	// fan-out queries both cells and each returns the SAME matches. Merged
	// results dedupe by repo+path+line; the stats must dedupe too, or the
	// summary reports "6 matches across 4 files in 2 repos" for 3 unique
	// results (and falsely claims truncation). Regression guard for the
	// mirror fan-out this trail introduced.
	matches := []codesearch.Result{
		{Repo: "acme/web", Path: "main.go", Line: 1, Column: 0, Score: 0.9},
		{Repo: "acme/web", Path: "main.go", Line: 2, Column: 0, Score: 0.8},
		{Repo: "acme/web", Path: "util.go", Line: 5, Column: 0, Score: 0.7},
	}
	cell := func(name, jur string) cellCallResult[*codesearch.SearchResponse] {
		return cellCallResult[*codesearch.SearchResponse]{
			group: cellGroup{cell: name, jurisdiction: jur},
			value: &codesearch.SearchResponse{
				Query:     "handleRequest",
				Stats:     codesearch.Stats{TotalMatches: 3, TotalFiles: 2, ReposSearched: 1, DurationMs: 10},
				RepoStats: []codesearch.RepoStats{{Repo: "acme/web", MatchCount: 3, FileCount: 2}},
				Results:   matches,
			},
		}
	}
	results := []cellCallResult[*codesearch.SearchResponse]{
		cell("aws-us-east-2", "us"),
		cell(testCellEU, "eu"),
	}

	merged, err := mergeSearchResults(context.Background(), 0, results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.Results) != 3 {
		t.Fatalf("len(Results) = %d, want 3 (mirror duplicates removed)", len(merged.Results))
	}
	if merged.Stats.TotalMatches != 3 {
		t.Errorf("TotalMatches = %d, want 3 (mirror must not double-count)", merged.Stats.TotalMatches)
	}
	if merged.Stats.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2 (mirror must not double-count)", merged.Stats.TotalFiles)
	}
	if merged.Stats.ReposSearched != 1 {
		t.Errorf("ReposSearched = %d, want 1 (one logical repo across two cells)", merged.Stats.ReposSearched)
	}
	if merged.Stats.DurationMs != 10 {
		t.Errorf("DurationMs = %v, want 10 (slowest cell preserved)", merged.Stats.DurationMs)
	}
	if len(merged.RepoStats) != 1 {
		t.Fatalf("len(RepoStats) = %d, want 1 (deduped by repo)", len(merged.RepoStats))
	}
	if merged.RepoStats[0].MatchCount != 3 || merged.RepoStats[0].FileCount != 2 {
		t.Errorf("RepoStats[0] = %+v, want representative {3,2} not summed {6,4}", merged.RepoStats[0])
	}
}

func TestResolveRepoFilters_GhPrefix(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID1, FullName: "entirehq/entire.io"},
	}
	ids, matched := resolveRepoFilters([]string{"gh/entirehq/entire.io"}, repos)
	if len(ids) != 1 || ids[0] != testRepoID1 {
		t.Fatalf("gh/ prefix: ids = %v, want [01ABC]", ids)
	}
	if len(matched) != 1 {
		t.Fatalf("gh/ prefix: matched = %d, want 1", len(matched))
	}
}

func TestResolveRepoFilters_EtPrefixNoStrip(t *testing.T) {
	t.Parallel()

	// BFF only strips gh/, not et/. "et/myproj/backend" is tried as-is
	// against full_name. It won't match "myproj/backend" — this aligns
	// with the BFF behavior.
	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID2, FullName: "myproj/backend"},
	}
	ids, _ := resolveRepoFilters([]string{"et/myproj/backend"}, repos)
	if len(ids) != 0 {
		t.Fatalf("et/ prefix should not match stripped FullName: ids = %v, want empty", ids)
	}

	// But if FullName is stored with the et/ prefix, it matches via the
	// unstripped fallback (full_name === filter).
	repos2 := []coreapi.RepoIndexEntry{
		{ID: testRepoID2, FullName: "et/myproj/backend"},
	}
	ids2, matched := resolveRepoFilters([]string{"et/myproj/backend"}, repos2)
	if len(ids2) != 1 || ids2[0] != testRepoID2 {
		t.Fatalf("et/ prefix with matching FullName: ids = %v, want [02DEF]", ids2)
	}
	if len(matched) != 1 {
		t.Fatalf("et/ prefix with matching FullName: matched = %d, want 1", len(matched))
	}
}

func TestResolveRepoFilters_ULID(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{ID: "01JXYZ123ABC", FullName: "entirehq/cli"},
	}
	ids, _ := resolveRepoFilters([]string{"01JXYZ123ABC"}, repos)
	if len(ids) != 1 || ids[0] != "01JXYZ123ABC" {
		t.Fatalf("ULID: ids = %v, want [01JXYZ123ABC]", ids)
	}
}

func TestResolveRepoFilters_BareSlug(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID1, FullName: "entirehq/entire.io"},
	}
	ids, _ := resolveRepoFilters([]string{"entirehq/entire.io"}, repos)
	if len(ids) != 1 || ids[0] != testRepoID1 {
		t.Fatalf("bare slug: ids = %v, want [01ABC]", ids)
	}
}

func TestResolveRepoFilters_UnstrippedFallback(t *testing.T) {
	t.Parallel()

	// BFF tries full_name === filter (unstripped) as a fallback. This lets
	// a filter like "gh/owner/repo" match if FullName happens to be
	// "gh/owner/repo" (not just "owner/repo").
	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID1, FullName: "gh/entirehq/entire.io"},
	}
	ids, matched := resolveRepoFilters([]string{"gh/entirehq/entire.io"}, repos)
	if len(ids) != 1 || ids[0] != testRepoID1 {
		t.Fatalf("unstripped fallback: ids = %v, want [01ABC]", ids)
	}
	if len(matched) != 1 {
		t.Fatalf("unstripped fallback: matched = %d, want 1", len(matched))
	}
}

func TestResolveRepoFilters_IDMatchUsesRawFilter(t *testing.T) {
	t.Parallel()

	// BFF matches id === filter (raw filter, not stripped slug).
	repos := []coreapi.RepoIndexEntry{
		{ID: "gh/something", FullName: "unrelated/repo"},
	}
	ids, _ := resolveRepoFilters([]string{"gh/something"}, repos)
	if len(ids) != 1 || ids[0] != "gh/something" {
		t.Fatalf("ID match on raw filter: ids = %v, want [gh/something]", ids)
	}
}

func TestResolveRepoFilters_NoMatch(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID1, FullName: "entirehq/entire.io"},
	}
	ids, matched := resolveRepoFilters([]string{"gh/nonexistent/repo"}, repos)
	if len(ids) != 0 {
		t.Fatalf("no match: ids = %v, want empty", ids)
	}
	if len(matched) != 0 {
		t.Fatalf("no match: matched = %d, want 0", len(matched))
	}
}

func TestResolveRepoFilters_DeduplicatesSameRepo(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID1, FullName: "entirehq/entire.io"},
	}
	// Same repo via three different formats — should produce one result.
	ids, _ := resolveRepoFilters([]string{"gh/entirehq/entire.io", "entirehq/entire.io", testRepoID1}, repos)
	if len(ids) != 1 {
		t.Fatalf("dedup: len(ids) = %d, want 1", len(ids))
	}
}

func TestResolveRepoFilters_MultipleReposMixed(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{ID: testRepoID1, FullName: "entirehq/entire.io"},
		{ID: testRepoID2, FullName: "myproj/backend"},
	}
	ids, matched := resolveRepoFilters([]string{"gh/entirehq/entire.io", "myproj/backend"}, repos)
	if len(ids) != 2 {
		t.Fatalf("multiple: len(ids) = %d, want 2", len(ids))
	}
	if len(matched) != 2 {
		t.Fatalf("multiple: len(matched) = %d, want 2", len(matched))
	}
}

func TestSearchCmd_CaseSensitiveWithCodeFlagParsesCorrectly(t *testing.T) {
	// --case-sensitive with --code should be accepted (fails later at auth, not at validation).
	t.Setenv("ENTIRE_CODE_SEARCH", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "--case-sensitive", "HandleRequest"})

	err := root.Execute()
	// Will fail at auth, but should NOT fail at flag validation.
	if err != nil && strings.Contains(err.Error(), "--case-sensitive can only be used with --code") {
		t.Errorf("--case-sensitive with --code should be accepted, got: %v", err)
	}
}

func TestSearchCmd_LimitFlagAccepted(t *testing.T) {
	// --limit with --code should parse correctly.
	t.Setenv("ENTIRE_CODE_SEARCH", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "--limit", "50", "handleRequest"})

	err := root.Execute()
	// Will fail at auth, but should NOT fail at flag parsing.
	if err != nil && strings.Contains(err.Error(), "invalid") {
		t.Errorf("--limit 50 should be accepted, got: %v", err)
	}
}

func TestSearchCmd_InlineRepoStarTreatedAsAllRepos(t *testing.T) {
	// repo:* inline should be treated as "all repos" (no filter).
	t.Setenv("ENTIRE_CODE_SEARCH", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "auth repo:*"})

	err := root.Execute()
	// Will fail at auth, but should NOT fail at query parsing.
	if err != nil && strings.Contains(err.Error(), "invalid") {
		t.Errorf("repo:* should be accepted, got: %v", err)
	}
}

func TestSearchCmd_MultipleInlineRepoFilters(t *testing.T) {
	// Multiple inline repo: filters should all be collected.
	t.Setenv("ENTIRE_CODE_SEARCH", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "auth repo:gh/entirehq/entire.io repo:gh/entirehq/cli"})

	err := root.Execute()
	// Will fail at auth, but should NOT fail at filter parsing.
	if err != nil && strings.Contains(err.Error(), "invalid") {
		t.Errorf("multiple repo: filters should be accepted, got: %v", err)
	}
}

func TestWriteCodeSearchJSON_RepoFilteredEmpty(t *testing.T) {
	t.Parallel()

	// When a repo filter matches nothing, we get an empty response.
	resp := &codesearch.SearchResponse{
		Query:   "handleRequest",
		Stats:   codesearch.Stats{},
		Results: nil,
	}

	var buf bytes.Buffer
	if err := writeCodeSearchJSON(&buf, resp); err != nil {
		t.Fatalf("writeCodeSearchJSON error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"results": []`) {
		t.Errorf("expected empty results array, got:\n%s", output)
	}
	if !strings.Contains(output, `"total": 0`) {
		t.Errorf("expected total 0, got:\n%s", output)
	}
}

func TestExtractInlineRepoFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input     string
		wantQuery string
		wantRepos []string
	}{
		{"auth", "auth", nil},
		{"auth repo:gh/entirehq/cli", "auth", []string{"gh/entirehq/cli"}},
		{"repo:gh/a/b repo:et/c/d handleRequest", "handleRequest", []string{"gh/a/b", "et/c/d"}},
		{"repo:*", "", []string{"*"}},
		// author: and branch: are NOT consumed — they stay in the query.
		{"author:foo TODO", "author:foo TODO", nil},
		{"branch:main auth repo:gh/a/b", "branch:main auth", []string{"gh/a/b"}},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			gotQuery, gotRepos := extractInlineRepoFilters(tc.input)
			if gotQuery != tc.wantQuery {
				t.Errorf("query = %q, want %q", gotQuery, tc.wantQuery)
			}
			if len(gotRepos) != len(tc.wantRepos) {
				t.Fatalf("repos = %v, want %v", gotRepos, tc.wantRepos)
			}
			for i := range gotRepos {
				if gotRepos[i] != tc.wantRepos[i] {
					t.Errorf("repos[%d] = %q, want %q", i, gotRepos[i], tc.wantRepos[i])
				}
			}
		})
	}
}

func TestSearchCmd_CodePreservesNonRepoFiltersInQuery(t *testing.T) {
	// Ensure author:foo is NOT consumed by code search query parsing.
	t.Setenv("ENTIRE_CODE_SEARCH", "1")

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "author:foo TODO"})

	err := root.Execute()
	// Will fail at auth/git, but should NOT fail with empty query.
	if err != nil && strings.Contains(err.Error(), "query required") {
		t.Errorf("author:foo should be preserved in code query, got: %v", err)
	}
}

func TestMergeSearchResults_AllCellsFail(t *testing.T) {
	t.Parallel()

	results := []cellCallResult[*codesearch.SearchResponse]{
		{group: cellGroup{cell: "aws-us-east-2", jurisdiction: "us"}, err: errors.New("us cell timed out")},
		{group: cellGroup{cell: testCellEU, jurisdiction: "eu"}, err: errors.New("eu cell timed out")},
	}

	_, err := mergeSearchResults(context.Background(), 0, results)
	if err == nil {
		t.Fatal("expected error when all cells fail")
	}
	if !strings.Contains(err.Error(), "code search failed") {
		t.Errorf("error = %q, want containing 'code search failed'", err.Error())
	}
}
