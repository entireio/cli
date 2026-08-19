package search

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConfig_ScopeSlugs verifies the shared scope predicate both backends
// derive their repo scoping from. The precedence rule that matters most: an
// explicit repo filter always wins over --all-repos (the more specific filter
// scopes the search) — v3 and v4 must agree on this.
func TestConfig_ScopeSlugs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		cfg          Config
		wantSlugs    []string
		wantAllRepos bool
	}{
		{"all-repos flag", Config{AllRepos: true}, nil, true},
		{"repo:* filter", Config{Repos: []string{AllReposFilter}}, nil, true},
		{"explicit repo", Config{Repos: []string{"o/r"}}, []string{"o/r"}, false},
		{"current-repo default", Config{Owner: "o", Repo: "r"}, []string{"o/r"}, false},
		{"explicit filter wins over --all-repos", Config{AllRepos: true, Repos: []string{"o/r"}}, []string{"o/r"}, false},
		{"explicit filter wins over repo:*", Config{Repos: []string{"o/r", AllReposFilter}}, []string{"o/r"}, false},
		{"explicit filters win over current repo", Config{Repos: []string{"a/b", "c/d"}, Owner: "o", Repo: "r"}, []string{"a/b", "c/d"}, false},
		{"no scope", Config{}, nil, false},
		{"owner without repo is no scope", Config{Owner: "o"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			slugs, allRepos := tt.cfg.ScopeSlugs()
			if strings.Join(slugs, ",") != strings.Join(tt.wantSlugs, ",") || allRepos != tt.wantAllRepos {
				t.Errorf("ScopeSlugs() = (%v, %v), want (%v, %v)", slugs, allRepos, tt.wantSlugs, tt.wantAllRepos)
			}
		})
	}
}

// TestResultID_RawDataFallback verifies repo/pr rows — which have no typed
// struct — expose the raw payload's id, so cross-cell dedup can identify the
// same logical result from two cells.
func TestResultID_RawDataFallback(t *testing.T) {
	t.Parallel()

	var repoRow Result
	if err := json.Unmarshal([]byte(`{"type":"repo","data":{"id":"01JREPO","name":"x"},"searchMeta":{"score":1}}`), &repoRow); err != nil {
		t.Fatal(err)
	}
	if got := repoRow.ResultID(); got != "01JREPO" {
		t.Errorf("repo ResultID() = %q, want the rawData id \"01JREPO\"", got)
	}

	var noID Result
	if err := json.Unmarshal([]byte(`{"type":"pr","data":{"title":"no id"},"searchMeta":{"score":1}}`), &noID); err != nil {
		t.Fatal(err)
	}
	if got := noID.ResultID(); got != "" {
		t.Errorf("pr-without-id ResultID() = %q, want \"\"", got)
	}

	// Typed results are unaffected.
	ck := Result{Type: TypeCheckpoint, Checkpoint: &CheckpointResult{ID: "ck1"}}
	if got := ck.ResultID(); got != "ck1" {
		t.Errorf("checkpoint ResultID() = %q, want \"ck1\"", got)
	}
}

// TestResultAccessors_RawDataFallback verifies repo/pr rows expose identifying
// fields from the raw payload, so trimmed views (e.g. --compact) don't collapse
// them to just {id, type, score}.
func TestResultAccessors_RawDataFallback(t *testing.T) {
	t.Parallel()

	const repoName = "backend"

	var repoRow Result
	if err := json.Unmarshal([]byte(`{"type":"repo","data":{"id":"01JREPO","name":"backend","org":"acme","createdAt":"2026-01-02T00:00:00Z"},"searchMeta":{"score":1}}`), &repoRow); err != nil {
		t.Fatal(err)
	}
	if got := repoRow.ResultTitle(); got != repoName {
		t.Errorf("repo ResultTitle() = %q, want \"backend\"", got)
	}
	if got := repoRow.ResultRepo(); got != repoName {
		t.Errorf("repo ResultRepo() = %q, want \"backend\"", got)
	}
	if got := repoRow.ResultOrg(); got != "acme" {
		t.Errorf("repo ResultOrg() = %q, want \"acme\"", got)
	}
	if got := repoRow.ResultCreatedAt(); got != "2026-01-02T00:00:00Z" {
		t.Errorf("repo ResultCreatedAt() = %q", got)
	}

	// A row carrying only an owner-qualified fullName splits into org + bare
	// repo, so org+"/"+repo joins never double the owner (acme/acme/backend).
	var qualifiedRow Result
	if err := json.Unmarshal([]byte(`{"type":"repo","data":{"id":"01JQUAL","fullName":"acme/backend"},"searchMeta":{"score":1}}`), &qualifiedRow); err != nil {
		t.Fatal(err)
	}
	if got := qualifiedRow.ResultOrg(); got != "acme" {
		t.Errorf("fullName-only ResultOrg() = %q, want \"acme\"", got)
	}
	if got := qualifiedRow.ResultRepo(); got != repoName {
		t.Errorf("fullName-only ResultRepo() = %q, want bare \"backend\"", got)
	}

	var prRow Result
	if err := json.Unmarshal([]byte(`{"type":"pr","data":{"id":"pr-9","title":"Fix login retry","repo":"backend","userLogin":"alice","headBranch":"fix/login"},"searchMeta":{"score":1}}`), &prRow); err != nil {
		t.Fatal(err)
	}
	if got := prRow.ResultTitle(); got != "Fix login retry" {
		t.Errorf("pr ResultTitle() = %q, want \"Fix login retry\"", got)
	}
	if got := prRow.ResultRepo(); got != repoName {
		t.Errorf("pr ResultRepo() = %q, want \"backend\"", got)
	}
	if got := prRow.ResultAuthor(); got != testAuthor {
		t.Errorf("pr ResultAuthor() = %q, want \"alice\"", got)
	}
	if got := prRow.ResultBranch(); got != "fix/login" {
		t.Errorf("pr ResultBranch() = %q, want \"fix/login\"", got)
	}
	// Fields absent from the payload stay empty.
	if got := prRow.ResultCreatedAt() + prRow.ResultOrg(); got != "" {
		t.Errorf("pr accessors for absent fields = %q, want all empty", got)
	}
}

// TestResultAccessors_TypedRowsNeverReadRawPayload pins the gate: for typed
// rows (checkpoint/commit/session) an empty typed field stays empty even when
// the raw payload carries a same-named key — the raw fallback is reserved for
// types without a typed struct, so backend field additions can't silently
// change what the TUI or compact output renders.
func TestResultAccessors_TypedRowsNeverReadRawPayload(t *testing.T) {
	t.Parallel()

	var sessionRow Result
	if err := json.Unmarshal([]byte(`{"type":"session","data":{"sessionId":"s1","displayName":"","author":"alice@example.com","title":"stray","branch":null},"searchMeta":{"score":1}}`), &sessionRow); err != nil {
		t.Fatal(err)
	}
	if got := sessionRow.ResultAuthor(); got != "" {
		t.Errorf("session ResultAuthor() = %q, want \"\" (raw author must be suppressed)", got)
	}
	if got := sessionRow.ResultTitle(); got != "" {
		t.Errorf("session ResultTitle() = %q, want \"\" (raw title must be suppressed)", got)
	}
	if got := sessionRow.ResultBranch(); got != "" {
		t.Errorf("session ResultBranch() = %q, want \"\"", got)
	}
}
