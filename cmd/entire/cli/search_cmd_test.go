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

func TestGroupReposByCell(t *testing.T) {
	t.Parallel()

	repos := []coreapi.RepoIndexEntry{
		{Cell: "aws-us-east-2", Jurisdiction: "us", FullName: "acme/web"},
		{Cell: "aws-us-east-2", Jurisdiction: "us", FullName: "acme/api"},
		{Cell: "aws-eu-west-1", Jurisdiction: "eu", FullName: "acme/docs"},
		{Cell: "", Jurisdiction: "us", FullName: "acme/empty-cell"},
	}

	groups := groupReposByCell(repos)

	if len(groups) != 2 {
		t.Fatalf("groupReposByCell returned %d groups, want 2", len(groups))
	}

	byCell := make(map[string]string)
	for _, g := range groups {
		byCell[g.cell] = g.jurisdiction
	}

	if j, ok := byCell["aws-us-east-2"]; !ok || j != "us" {
		t.Errorf("missing or wrong jurisdiction for aws-us-east-2: got %q", j)
	}
	if j, ok := byCell["aws-eu-west-1"]; !ok || j != "eu" {
		t.Errorf("missing or wrong jurisdiction for aws-eu-west-1: got %q", j)
	}
}

func TestGroupReposByCell_Empty(t *testing.T) {
	t.Parallel()

	groups := groupReposByCell(nil)
	if len(groups) != 0 {
		t.Fatalf("groupReposByCell(nil) returned %d groups, want 0", len(groups))
	}
}

func TestMergeSearchResults(t *testing.T) {
	t.Parallel()

	cells := []cellGroup{
		{cell: "aws-us-east-2", jurisdiction: "us"},
		{cell: "aws-eu-west-1", jurisdiction: "eu"},
	}

	results := []codeSearchCellResult{
		{
			resp: &codesearch.SearchResponse{
				Query: "handleRequest",
				Stats: codesearch.Stats{TotalMatches: 3, TotalFiles: 2, ReposSearched: 1, DurationMs: 10},
				Results: []codesearch.Result{
					{Repo: "acme/web", Path: "main.go", Line: 1},
				},
				RepoStats: []codesearch.RepoStats{{Repo: "acme/web", MatchCount: 3}},
			},
		},
		{
			resp: &codesearch.SearchResponse{
				Query: "handleRequest",
				Stats: codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1, DurationMs: 20},
				Results: []codesearch.Result{
					{Repo: "acme/docs", Path: "handler.go", Line: 5},
				},
				RepoStats: []codesearch.RepoStats{{Repo: "acme/docs", MatchCount: 1}},
			},
		},
	}

	merged := mergeSearchResults(context.Background(), cells, results)

	if merged.Stats.TotalMatches != 4 {
		t.Errorf("TotalMatches = %d, want 4", merged.Stats.TotalMatches)
	}
	if merged.Stats.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3", merged.Stats.TotalFiles)
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
	if len(merged.RepoStats) != 2 {
		t.Fatalf("len(RepoStats) = %d, want 2", len(merged.RepoStats))
	}
}

func TestMergeSearchResults_CellError(t *testing.T) {
	t.Parallel()

	cells := []cellGroup{
		{cell: "aws-us-east-2", jurisdiction: "us"},
		{cell: "aws-eu-west-1", jurisdiction: "eu"},
	}

	results := []codeSearchCellResult{
		{
			resp: &codesearch.SearchResponse{
				Query:   "test",
				Stats:   codesearch.Stats{TotalMatches: 2, TotalFiles: 1, ReposSearched: 1, DurationMs: 5},
				Results: []codesearch.Result{{Repo: "acme/web", Path: "f.go", Line: 1}},
			},
		},
		{
			err: errors.New("cell timed out"),
		},
	}

	merged := mergeSearchResults(context.Background(), cells, results)

	// The failed cell is skipped; results from the successful cell are preserved.
	if merged.Stats.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2 (failed cell skipped)", merged.Stats.TotalMatches)
	}
	if len(merged.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1 (failed cell skipped)", len(merged.Results))
	}
}
