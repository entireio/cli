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
