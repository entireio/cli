package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/codesearch"
	"github.com/entireio/cli/cmd/entire/cli/search"
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
		Stats:   codesearch.Stats{TotalMatches: 1, TotalFiles: 1, ReposSearched: 1, DurationMs: 5},
		Results: []codesearch.Result{{Repo: "r", Path: "f.go", Line: 1, ContextLine: "package main"}},
	}

	var buf bytes.Buffer
	if err := writeCodeSearchJSON(&buf, resp); err != nil {
		t.Fatalf("writeCodeSearchJSON error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"total": 1`) {
		t.Errorf("output missing total:\n%s", output)
	}
	if !strings.Contains(output, `"path": "f.go"`) {
		t.Errorf("output missing result path:\n%s", output)
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
