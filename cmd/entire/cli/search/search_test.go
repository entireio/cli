package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testOwner = "entirehq"
const testRepo = "entire.io"

// -- ParseGitHubRemote tests --

func TestParseGitHubRemote_SSH(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("git@github.com:entirehq/entire.io.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_HTTPS(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("https://github.com/entirehq/entire.io.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_HTTPSNoGit(t *testing.T) {
	t.Parallel()
	owner, repo, err := ParseGitHubRemote("https://github.com/entirehq/entire.io")
	if err != nil {
		t.Fatal(err)
	}
	if owner != testOwner || repo != testRepo {
		t.Errorf("got %s/%s, want %s/%s", owner, repo, testOwner, testRepo)
	}
}

func TestParseGitHubRemote_Invalid(t *testing.T) {
	t.Parallel()
	_, _, err := ParseGitHubRemote("")
	if err == nil {
		t.Error("expected error for empty URL")
	}

	_, _, err = ParseGitHubRemote("not-a-url")
	if err == nil {
		t.Error("expected error for invalid URL")
	}
}

func TestParseGitHubRemote_NonGitHubSSH(t *testing.T) {
	t.Parallel()
	_, _, err := ParseGitHubRemote("git@gitlab.com:entirehq/entire.io.git")
	if err == nil {
		t.Error("expected error for non-GitHub SSH remote")
	}
}

func TestParseGitHubRemote_NonGitHubHTTPS(t *testing.T) {
	t.Parallel()
	_, _, err := ParseGitHubRemote("https://gitlab.com/entirehq/entire.io.git")
	if err == nil {
		t.Error("expected error for non-GitHub HTTPS remote")
	}
}

// -- Search() tests --

func TestSearch_URLConstruction(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "ghp_test123",
		Owner:       "myowner",
		Repo:        "myrepo",
		Query:       "find bugs",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Path != "/search/v1/search" {
		t.Errorf("path = %s, want /search/v1/search", capturedReq.URL.Path)
	}
	if capturedReq.URL.Query().Get("q") != "find bugs" {
		t.Errorf("q = %s, want 'find bugs'", capturedReq.URL.Query().Get("q"))
	}
	if capturedReq.URL.Query().Get("repo") != "myowner/myrepo" {
		t.Errorf("repo = %s, want 'myowner/myrepo'", capturedReq.URL.Query().Get("repo"))
	}
	if capturedReq.URL.Query().Get("types") != "checkpoints" {
		t.Errorf("types = %s, want 'checkpoints'", capturedReq.URL.Query().Get("types"))
	}
	if capturedReq.URL.Query().Get("limit") != "10" {
		t.Errorf("limit = %s, want '10'", capturedReq.URL.Query().Get("limit"))
	}
	if capturedReq.Header.Get("Authorization") != "token ghp_test123" {
		t.Errorf("auth header = %s, want 'token ghp_test123'", capturedReq.Header.Get("Authorization"))
	}
	if capturedReq.Header.Get("User-Agent") != "entire-cli" {
		t.Errorf("user-agent = %s, want 'entire-cli'", capturedReq.Header.Get("User-Agent"))
	}
}

func TestSearch_ZeroLimitOmitsParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Has("limit") {
		t.Error("limit param should be omitted when zero")
	}
}

func TestSearch_ErrorJSON(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid token"}) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "bad",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if got := err.Error(); got != "search service error (401): Invalid token" {
		t.Errorf("error = %q, want 'search service error (401): Invalid token'", got)
	}
}

func TestSearch_ErrorRawBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>Bad Gateway</html>")) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error for 502")
	}
	if got := err.Error(); got != "search service returned 502: <html>Bad Gateway</html>" {
		t.Errorf("error = %q", got)
	}
}

func TestSearch_ErrorFieldOn200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{Error: "user not found in Entire"}) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err == nil {
		t.Fatal("expected error when server returns 200 with error field")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("error = %q, want message containing 'user not found'", err.Error())
	}
}

func TestSearch_SuccessWithResults(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := Response{
			Results: []Result{
				{
					Type: "checkpoint",
					Data: CheckpointResult{
						ID:        "abc123def456",
						Branch:    "main",
						Prompt:    "add auth middleware",
						Author:    "alice",
						CreatedAt: "2026-01-13T12:00:00Z",
					},
					Meta: Meta{
						Score:     0.042,
						MatchType: "both",
					},
				},
			},
			Total: 1,
			Page:  1,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	resp, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(resp.Results))
	}
	if resp.Results[0].Data.ID != "abc123def456" {
		t.Errorf("checkpoint id = %s, want abc123def456", resp.Results[0].Data.ID)
	}
	if resp.Results[0].Meta.MatchType != "both" {
		t.Errorf("matchType = %s, want both", resp.Results[0].Meta.MatchType)
	}
}

func TestSearch_FilterParams(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
		Author:      testAuthor,
		Date:        "week",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Get("author") != testAuthor {
		t.Errorf("author = %s, want %q", capturedReq.URL.Query().Get("author"), testAuthor)
	}
	if capturedReq.URL.Query().Get("date") != "week" {
		t.Errorf("date = %s, want 'week'", capturedReq.URL.Query().Get("date"))
	}
}

func TestSearch_PageParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 2}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
		Page:        2,
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Get("page") != "2" {
		t.Errorf("page = %s, want '2'", capturedReq.URL.Query().Get("page"))
	}
}

func TestSearch_ZeroPageOmitsParam(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Has("page") {
		t.Error("page param should be omitted when zero")
	}
}

func TestSearch_EmptyFiltersOmitParams(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := Search(context.Background(), Config{
		ServiceURL:  srv.URL,
		GitHubToken: "tok",
		Owner:       "o",
		Repo:        "r",
		Query:       "q",
	})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Query().Has("author") {
		t.Error("author param should be omitted when empty")
	}
	if capturedReq.URL.Query().Has("date") {
		t.Error("date param should be omitted when empty")
	}
}

// -- HasFilters tests --

func TestConfig_HasFilters(t *testing.T) {
	t.Parallel()

	if (Config{}).HasFilters() {
		t.Error("empty config should not have filters")
	}
	if !(Config{Author: "alice"}).HasFilters() {
		t.Error("config with Author should have filters")
	}
	if !(Config{Date: "week"}).HasFilters() {
		t.Error("config with Date should have filters")
	}
	if !(Config{Author: "alice", Date: "week"}).HasFilters() {
		t.Error("config with both should have filters")
	}
}

// -- ParseSearchInput tests --

const testQuery = "auth"
const testAuthor = "alice"

func TestParseSearchInput_QueryOnly(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput("fix auth bug")
	if p.Query != "fix auth bug" {
		t.Errorf("query = %q, want 'fix auth bug'", p.Query)
	}
	if p.Author != "" || p.Date != "" {
		t.Error("expected no filters")
	}
}

func TestParseSearchInput_AuthorFilter(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(testQuery + " author:" + testAuthor)
	if p.Query != testQuery {
		t.Errorf("query = %q, want %q", p.Query, testQuery)
	}
	if p.Author != testAuthor {
		t.Errorf("author = %q, want %q", p.Author, testAuthor)
	}
}

func TestParseSearchInput_DateFilter(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(testQuery + " date:week")
	if p.Query != testQuery {
		t.Errorf("query = %q, want %q", p.Query, testQuery)
	}
	if p.Date != "week" {
		t.Errorf("date = %q, want 'week'", p.Date)
	}
}

func TestParseSearchInput_BothFilters(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(testQuery + " author:" + testAuthor + " date:month")
	if p.Query != testQuery {
		t.Errorf("query = %q, want %q", p.Query, testQuery)
	}
	if p.Author != testAuthor {
		t.Errorf("author = %q, want %q", p.Author, testAuthor)
	}
	if p.Date != "month" {
		t.Errorf("date = %q, want 'month'", p.Date)
	}
}

func TestParseSearchInput_QuotedAuthor(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput(`author:"` + testAuthor + ` smith" fix bug`)
	if p.Author != testAuthor+" smith" {
		t.Errorf("author = %q, want %q", p.Author, testAuthor+" smith")
	}
	if p.Query != "fix bug" {
		t.Errorf("query = %q, want 'fix bug'", p.Query)
	}
}

func TestParseSearchInput_FiltersOnly(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput("author:bob")
	if p.Query != "" {
		t.Errorf("query = %q, want empty", p.Query)
	}
	if p.Author != "bob" {
		t.Errorf("author = %q, want 'bob'", p.Author)
	}
}

func TestParseSearchInput_Empty(t *testing.T) {
	t.Parallel()
	p := ParseSearchInput("")
	if p.Query != "" || p.Author != "" || p.Date != "" {
		t.Error("expected all empty for empty input")
	}
}
