package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Each instance's examples must use its own command path: the top-level alias
// is `entire search`, the canonical form under the checkpoint group is
// `entire checkpoint search`. A shared prefix would mislead one command's help.
func TestSearchCmd_ExamplesMatchCommandPath(t *testing.T) {
	t.Parallel()

	topLevel := newSearchCmd().Example
	if !strings.Contains(topLevel, "entire search ") || strings.Contains(topLevel, "checkpoint search") {
		t.Fatalf("top-level search examples must use the `entire search` prefix:\n%s", topLevel)
	}

	checkpoint := newCheckpointSearchCmd().Example
	if !strings.Contains(checkpoint, "entire checkpoint search ") {
		t.Fatalf("checkpoint search examples must use the `entire checkpoint search` prefix:\n%s", checkpoint)
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

func TestWriteSearchCompactJSON_TrimsResults(t *testing.T) {
	t.Parallel()

	resp := &search.Response{
		Results: testResults(),
		Total:   2,
		Page:    1,
	}

	var buf bytes.Buffer
	if err := writeSearchCompactJSON(&buf, resp, 0, 1); err != nil {
		t.Fatalf("writeSearchCompactJSON returned error: %v", err)
	}

	output := buf.String()
	// Identifiers, ranking, and files survive.
	for _, want := range []string{
		`"id": "a3b2c4d5e6f7"`,
		`"type": "checkpoint"`,
		`"repo": "entirehq/entire.io"`,
		`"branch": "main"`,
		`"author": "alicecodes"`,
		`"date": "2026-03-24T10:30:00Z"`,
		`"src/middleware/auth.go"`,
		`"score": 0.042`,
		`"title": "Implement auth middleware"`,
		`"snippet": "added auth middleware for JWT validation"`,
		`"matchType": "semantic"`,
		`"total_pages": 1`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("compact output missing %s:\n%s", want, output)
		}
	}
	// The full prompt must NOT be embedded (that's the whole point).
	if strings.Contains(output, "add auth middleware to protect API routes") {
		t.Errorf("compact output must not contain the full prompt:\n%s", output)
	}
}

// Repo/pr rows (reachable via --all-repos) have no typed struct; compact hits
// must still carry identifying info from the raw payload instead of collapsing
// to just {id, type, score}.
func TestWriteSearchCompactJSON_RepoAndPRRowsKeepIdentifyingFields(t *testing.T) {
	t.Parallel()

	wire := `{"results":[
		{"type":"repo","data":{"id":"01JREPO","name":"backend","org":"acme","fullName":"acme/backend","description":"Backend services","checkpointCount":18},"searchMeta":{"score":0.9}},
		{"type":"pr","data":{"id":"pr-9","title":"Fix login retry","repo":"backend","userLogin":"alice"},"searchMeta":{"score":0.5}}
	],"total":2,"page":1}`
	var resp search.Response
	if err := json.Unmarshal([]byte(wire), &resp); err != nil {
		t.Fatalf("unmarshaling wire response: %v", err)
	}

	var buf bytes.Buffer
	if err := writeSearchCompactJSON(&buf, &resp, 0, 1); err != nil {
		t.Fatalf("writeSearchCompactJSON returned error: %v", err)
	}

	output := buf.String()
	for _, want := range []string{
		`"id": "01JREPO"`,
		`"repo": "acme/backend"`,
		`"title": "backend"`,
		`"description": "Backend services"`,
		`"checkpointCount": 18`,
		`"id": "pr-9"`,
		`"title": "Fix login retry"`,
		`"author": "alice"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("compact output missing %s:\n%s", want, output)
		}
	}
	// The owner must never be doubled when the payload carries a qualified fullName.
	if strings.Contains(output, "acme/acme/") {
		t.Errorf("compact output doubled the repo owner:\n%s", output)
	}
}

// A checkpoint with no commit subject titles itself with the prompt, and the
// backend's snippet for that row is the prompt's first indexed chunk
// ("Prompt: " + the same text) — emitting both would carry the same 200 runes
// twice. The duplicate snippet is dropped; a snippet from a later chunk (not
// a title prefix) survives because it shows where the match landed.
func TestWriteSearchCompactJSON_DropsSnippetDuplicatingTitle(t *testing.T) {
	t.Parallel()

	subject := "fix login"
	longPrompt := strings.TrimSpace(strings.Repeat("word ", 50)) // 249 runes, past the 200-rune cap
	cases := []struct {
		name        string
		checkpoint  *search.CheckpointResult
		snippet     string
		wantSnippet bool
	}{
		{
			name:       "prompt-title duplicate dropped",
			checkpoint: &search.CheckpointResult{ID: "cp1", Prompt: "add rate limiting to the public API", Org: "o", Repo: "r"},
			snippet:    "Prompt: add rate limiting to the public API",
		},
		{
			name:       "duplicate survives truncation on both sides",
			checkpoint: &search.CheckpointResult{ID: "cp2", Prompt: longPrompt, Org: "o", Repo: "r"},
			snippet:    "Prompt: " + longPrompt,
		},
		{
			name:        "later-chunk snippet kept",
			checkpoint:  &search.CheckpointResult{ID: "cp3", Prompt: "add rate limiting to the public API", Org: "o", Repo: "r"},
			snippet:     "Prompt: retry the bucket refill when redis is down",
			wantSnippet: true,
		},
		{
			name:        "snippet extending a commit-subject title kept",
			checkpoint:  &search.CheckpointResult{ID: "cp4", Prompt: "fix login retries in the auth flow", CommitSubject: &subject, Org: "o", Repo: "r"},
			snippet:     "Prompt: fix login retries in the auth flow",
			wantSnippet: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &search.Response{
				Results: []search.Result{{
					Type:       search.TypeCheckpoint,
					Checkpoint: tc.checkpoint,
					Meta:       search.Meta{Snippet: tc.snippet, Score: 1},
				}},
				Total: 1,
			}
			var buf bytes.Buffer
			if err := writeSearchCompactJSON(&buf, resp, 0, 1); err != nil {
				t.Fatalf("writeSearchCompactJSON returned error: %v", err)
			}
			if got := strings.Contains(buf.String(), `"snippet"`); got != tc.wantSnippet {
				t.Errorf("snippet present = %v, want %v:\n%s", got, tc.wantSnippet, buf.String())
			}
		})
	}
}

func TestWriteSearchCompactJSON_TruncatesLongPromptTitle(t *testing.T) {
	t.Parallel()

	longPrompt := strings.Repeat("word ", 200) // ~1000 chars, no commit message fallback
	resp := &search.Response{
		Results: []search.Result{{
			Type: search.TypeCheckpoint,
			Checkpoint: &search.CheckpointResult{
				ID:     "cp1",
				Prompt: longPrompt,
				Org:    "o",
				Repo:   "r",
			},
		}},
		Total: 1,
	}

	var buf bytes.Buffer
	if err := writeSearchCompactJSON(&buf, resp, 0, 1); err != nil {
		t.Fatalf("writeSearchCompactJSON returned error: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, strings.TrimSpace(longPrompt)) {
		t.Error("expected long prompt title to be truncated")
	}
	if !strings.Contains(output, "…") {
		t.Errorf("expected truncated title to end with ellipsis:\n%s", output)
	}
}

func TestSearchCmd_CompactWithCodeRejected(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "--compact", "handleRequest"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when --compact used with --code")
	}
	if !strings.Contains(err.Error(), "--compact cannot be used with --code") {
		t.Errorf("error = %q, want containing '--compact cannot be used with --code'", err.Error())
	}
}

func TestSearchCmd_CodeFlagUngated(t *testing.T) {
	// Code search is generally available: --code must never fail with the old
	// feature-gate message (it may still fail later at auth/network).
	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "test query"})

	err := root.Execute()
	if err != nil && strings.Contains(err.Error(), "not yet available") {
		t.Errorf("--code should not be feature-gated, got: %q", err.Error())
	}
}

func TestSearchCmd_CodeFlagRequiresQuery(t *testing.T) {
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
	writeCodeSearchText(&buf, resp, newStatusStyles(&buf), false)

	output := buf.String()
	if !strings.Contains(output, "entireio/cli:main.go\n") {
		t.Errorf("output missing file header:\n%s", output)
	}
	if !strings.Contains(output, "  10: func main() {") {
		t.Errorf("output missing first result:\n%s", output)
	}
	if !strings.Contains(output, "  42: \tfmt.Println(\"hello\")") {
		t.Errorf("output missing second result:\n%s", output)
	}
	if !strings.Contains(output, "2 matches across 1 files") {
		t.Errorf("output missing summary line:\n%s", output)
	}
	if strings.Contains(output, "\x1b[") {
		t.Errorf("expected no ANSI codes for non-terminal writer:\n%s", output)
	}
}

func TestWriteCodeSearchText_GroupsByFile(t *testing.T) {
	t.Parallel()

	// Interleaved files (score-sorted input) should collapse into one header
	// per file, in first-appearance order.
	resp := &codesearch.SearchResponse{
		Stats: codesearch.Stats{TotalMatches: 3, TotalFiles: 2, ReposSearched: 1, DurationMs: 1},
		Results: []codesearch.Result{
			{Repo: "r", Path: "a.go", Line: 1, ContextLine: "one"},
			{Repo: "r", Path: "b.go", Line: 2, ContextLine: "two"},
			{Repo: "r", Path: "a.go", Line: 3, ContextLine: "three"},
		},
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp, newStatusStyles(&buf), false)

	output := buf.String()
	if got := strings.Count(output, "r:a.go\n"); got != 1 {
		t.Errorf("expected exactly 1 header for a.go, got %d:\n%s", got, output)
	}
	if aIdx, bIdx := strings.Index(output, "r:a.go"), strings.Index(output, "r:b.go"); aIdx > bIdx {
		t.Errorf("expected a.go header before b.go:\n%s", output)
	}
}

func TestWriteCodeSearchText_CapsFilesAndMatchesPerFile(t *testing.T) {
	t.Parallel()

	var results []codesearch.Result
	// First file has 5 matches — 2 over the per-file cap.
	for line := 1; line <= maxCodeSearchFileMatches+2; line++ {
		results = append(results, codesearch.Result{Repo: "r", Path: "hot.go", Line: line, ContextLine: "x"})
	}
	// More files than the file cap.
	for f := range maxCodeSearchFiles + 3 {
		results = append(results, codesearch.Result{Repo: "r", Path: fmt.Sprintf("f%02d.go", f), Line: 1, ContextLine: "y"})
	}
	resp := &codesearch.SearchResponse{
		Stats:   codesearch.Stats{TotalMatches: len(results), TotalFiles: maxCodeSearchFiles + 4, ReposSearched: 1},
		Results: results,
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp, newStatusStyles(&buf), false)
	output := buf.String()

	if got := strings.Count(output, "r:"); got != maxCodeSearchFiles {
		t.Errorf("expected %d file headers, got %d:\n%s", maxCodeSearchFiles, got, output)
	}
	if !strings.Contains(output, "+ 2 matches") {
		t.Errorf("expected '+ 2 matches' overflow for hot.go:\n%s", output)
	}
	// hot.go shows only the per-file cap: lines 1..3, not 4/5.
	if strings.Contains(output, fmt.Sprintf("  %d: x", maxCodeSearchFileMatches+1)) {
		t.Errorf("expected at most %d matches for hot.go:\n%s", maxCodeSearchFileMatches, output)
	}
}

func TestHighlightCodeMatches(t *testing.T) {
	t.Parallel()

	styles := statusStyles{colorEnabled: true}

	out := highlightCodeMatches("func HandleRequest(w)", "handlerequest", styles, false)
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("expected ANSI codes in highlighted output, got %q", out)
	}
	if !strings.HasPrefix(out, "func ") || !strings.HasSuffix(out, "(w)") {
		t.Errorf("expected unmatched text preserved around highlight, got %q", out)
	}

	if out := highlightCodeMatches("no match here", "zzz", styles, false); out != "no match here" {
		t.Errorf("expected unchanged line when no match, got %q", out)
	}

	// Case-sensitive search must not highlight case variants.
	if out := highlightCodeMatches("func HandleRequest(w)", "handlerequest", styles, true); out != "func HandleRequest(w)" {
		t.Errorf("expected no highlight for case mismatch with caseSensitive, got %q", out)
	}

	// Non-ASCII input falls back to exact matching (no case folding).
	if out := highlightCodeMatches("comment ÉTÉ ici", "été", styles, false); out != "comment ÉTÉ ici" {
		t.Errorf("expected no case-folded highlight for non-ASCII input, got %q", out)
	}
	if out := highlightCodeMatches("comment été ici", "été", styles, false); !strings.Contains(out, "\x1b[") {
		t.Errorf("expected exact non-ASCII match highlighted, got %q", out)
	}

	plain := statusStyles{colorEnabled: false}
	if out := highlightCodeMatches("func main()", "main", plain, false); out != "func main()" {
		t.Errorf("expected unchanged line when color disabled, got %q", out)
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
	writeCodeSearchText(&buf, resp, newStatusStyles(&buf), false)

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

func TestWriteCodeSearchText_HighlightsTruncatedLines(t *testing.T) {
	t.Parallel()

	// The appended "…" is non-ASCII; it must not disable case-insensitive
	// highlighting for an otherwise ASCII line.
	longLine := "FooBar " + strings.Repeat("x", 300)
	resp := &codesearch.SearchResponse{
		Query:   "foobar",
		Stats:   codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1, DurationMs: 1},
		Results: []codesearch.Result{{Repo: "r", Path: "f.go", Line: 1, ContextLine: longLine}},
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp, statusStyles{colorEnabled: true}, false)

	output := buf.String()
	if !strings.Contains(output, "…") {
		t.Errorf("expected truncated line to end with ellipsis:\n%s", output)
	}
	if !strings.Contains(output, "\x1b[") {
		t.Errorf("expected case-insensitive highlight on truncated line:\n%s", output)
	}
}

func TestWriteCodeSearchText_Empty(t *testing.T) {
	t.Parallel()

	resp := &codesearch.SearchResponse{
		Stats: codesearch.Stats{},
	}

	var buf bytes.Buffer
	writeCodeSearchText(&buf, resp, newStatusStyles(&buf), false)

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
	root := NewRootCmd()
	root.SetArgs([]string{"search", "--code", "auth repo:gh/entirehq/entire.io repo:gh/entirehq/cli"})

	err := root.Execute()
	// Will fail at auth, but should NOT fail at filter parsing.
	if err != nil && strings.Contains(err.Error(), "invalid") {
		t.Errorf("multiple repo: filters should be accepted, got: %v", err)
	}
}

func TestSearchCmd_SemanticMultipleRepoFlags(t *testing.T) {
	// Semantic search (no --code) must accept multiple repos via a repeatable
	// --repo flag (ENT-1047) — parity with code search. It fails later at
	// auth/git, but must not be rejected as an invalid/unsupported filter.
	root := NewRootCmd()
	root.SetArgs([]string{"search", "auth", "--repo", "entirehq/entire.io", "--repo", "entireio/cli"})

	err := root.Execute()
	if err != nil {
		if strings.Contains(err.Error(), "validating repo filter") {
			t.Errorf("multiple --repo flags should pass validation, got: %v", err)
		}
		if strings.Contains(err.Error(), "only one explicit repo filter") {
			t.Errorf("multiple repos should no longer be rejected, got: %v", err)
		}
	}
}

func TestSearchCmd_SemanticCommaSeparatedRepoFlag(t *testing.T) {
	// A single comma-separated --repo value must expand to multiple repos.
	root := NewRootCmd()
	root.SetArgs([]string{"search", "auth", "--repo", "entirehq/entire.io,entireio/cli"})

	err := root.Execute()
	if err != nil {
		if strings.Contains(err.Error(), "validating repo filter") {
			t.Errorf("comma-separated --repo should pass validation, got: %v", err)
		}
		if strings.Contains(err.Error(), "only one explicit repo filter") {
			t.Errorf("comma-separated repos should no longer be rejected, got: %v", err)
		}
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
