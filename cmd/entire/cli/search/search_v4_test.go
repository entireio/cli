package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// TestCellV4_URLConstruction verifies the per-cell v4 primitive hits the
// query-serve route, sends repo ULIDs as repeated params, carries the identity
// token, forwards every filter param, and — like v3 — never sends types.
func TestCellV4_URLConstruction(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("id-token-123", srv.URL)
	_, err := CellV4(context.Background(), client, Config{
		Query:  "find bugs",
		Limit:  10,
		Author: "alice",
		Date:   "week",
		Branch: "main",
		Page:   2,
	}, []string{"01JREPOA", "01JREPOB"})
	if err != nil {
		t.Fatal(err)
	}

	if capturedReq.URL.Path != v4ServicePath {
		t.Errorf("path = %s, want %s", capturedReq.URL.Path, v4ServicePath)
	}
	q := capturedReq.URL.Query()
	if repos := q["repo"]; len(repos) != 2 || repos[0] != "01JREPOA" || repos[1] != "01JREPOB" {
		t.Errorf("repo params = %v, want [01JREPOA 01JREPOB] (repeated ULIDs)", repos)
	}
	if q.Get("q") != "find bugs" {
		t.Errorf("q = %s, want 'find bugs'", q.Get("q"))
	}
	if q.Get("limit") != "10" {
		t.Errorf("limit = %s, want '10'", q.Get("limit"))
	}
	if q.Get("author") != "alice" || q.Get("date") != "week" || q.Get("branch") != "main" || q.Get("page") != "2" {
		t.Errorf("filter params not forwarded: %v", q)
	}
	if q.Has("types") {
		t.Errorf("types param should not be set, got %q", q.Get("types"))
	}
	if capturedReq.Header.Get("Authorization") != "Bearer id-token-123" {
		t.Errorf("auth header = %s, want 'Bearer id-token-123'", capturedReq.Header.Get("Authorization"))
	}
}

// TestCellV4_UnfilteredOmitsRepo confirms an empty repoIDs slice sends no
// repo param — query-serve then searches every repo the token can access.
func TestCellV4_UnfilteredOmitsRepo(t *testing.T) {
	t.Parallel()

	var capturedReq *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r
		resp := Response{Results: []Result{}, Total: 0, Page: 1}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test helper response
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capturedReq.URL.Query().Has("repo") {
		t.Errorf("repo param should be omitted for an unfiltered (all-accessible) search, got %q", capturedReq.URL.Query().Get("repo"))
	}
}

// TestCellV4_ResponseDecodesLikeV3 confirms the v4 response — which
// carries extra top-level fields (accessible_repos, fanout, partial) the v3
// worker doesn't — decodes into the same Response the --json shape depends on,
// dropping the unknown fields.
func TestCellV4_ResponseDecodesLikeV3(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, `{
			"results": [
				{"type": "commit", "data": {"commitSha": "abc123", "org": "o", "repo": "r"}, "searchMeta": {"matchType": "both", "score": 1.5, "tier": 0}}
			],
			"total": 1,
			"page": 1,
			"counts": {"repos": 0, "checkpoints": 0, "commits": 1, "prs": 0, "sessions": 0},
			"accessible_repos": [{"repo": "o/r", "repo_id": "01JREPOA"}],
			"fanout": {"attempted": 1, "succeeded": 1},
			"partial": false
		}`)
	}))
	defer srv.Close()

	resp, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, []string{"01JREPOA"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Results) != 1 {
		t.Fatalf("total=%d results=%d, want 1/1", resp.Total, len(resp.Results))
	}
	if resp.Results[0].Type != TypeCommit || resp.Results[0].Commit == nil || resp.Results[0].Commit.CommitSHA != "abc123" {
		t.Errorf("commit result did not decode; got %+v", resp.Results[0])
	}
	if resp.Counts == nil || resp.Counts.Commits != 1 {
		t.Errorf("counts did not decode; got %+v", resp.Counts)
	}
}

// TestCellV4_ErrorForwarded confirms an upstream error is surfaced (no v3
// fallback at this layer).
func TestCellV4_ErrorForwarded(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeTestJSON(w, `{"error": "invalid types value"}`)
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
}

// TestCellV4_RouteNotFoundIsErrCellUnavailable confirms a route-level 404 (the
// gateway has no semantic-search route — query-serve not deployed in the cell)
// maps to the ErrCellUnavailable sentinel so fan-out callers can skip the cell
// quietly.
func TestCellV4_RouteNotFoundIsErrCellUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if !errors.Is(err, ErrCellUnavailable) {
		t.Fatalf("err = %v, want ErrCellUnavailable", err)
	}
}

// TestCellV4_RepoUnmatched404IsNotCellUnavailable covers the OTHER 404: the
// route exists and query-serve answered, but the repo filter matched nothing
// (not indexed, or the owner org isn't feature-flag enabled — entire-search
// fails closed with a JSON 404 body). That must NOT be classified as
// "query-serve isn't deployed in this cell", or an org merely missing from
// the flag reads as a missing region to the user.
func TestCellV4_RepoUnmatched404IsNotCellUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestJSON(w, `{"error": "None of the requested repos were found"}`)
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, []string{"01JREPOA"})
	if errors.Is(err, ErrCellUnavailable) {
		t.Fatalf("err = %v, want NOT ErrCellUnavailable for a repo-unmatched JSON 404", err)
	}
	if !errors.Is(err, ErrRepoFilterUnmatched) {
		t.Fatalf("err = %v, want ErrRepoFilterUnmatched", err)
	}
	if !strings.Contains(err.Error(), "None of the requested repos were found") {
		t.Errorf("err = %v, want the server's own message preserved in the wrap", err)
	}
}

// TestCellV4_UnfilteredJSON404IsCellUnavailable: a repo-filter miss requires a
// repo filter. When no repo IDs were sent (the --all-repos path), the same
// JSON-404 shape must NOT read as ErrRepoFilterUnmatched — the caller named no
// repo to blame — and instead degrades to the ErrCellUnavailable fail-safe.
func TestCellV4_UnfilteredJSON404IsCellUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestJSON(w, `{"error": "None of the requested repos were found"}`)
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, nil)
	if errors.Is(err, ErrRepoFilterUnmatched) {
		t.Fatalf("err = %v, want NOT ErrRepoFilterUnmatched when no repo filter was sent", err)
	}
	if !errors.Is(err, ErrCellUnavailable) {
		t.Fatalf("err = %v, want ErrCellUnavailable", err)
	}
}

// TestCellV4_ProblemJSON404IsCellUnavailable pins the THIRD 404 shape: the
// gateway's anonymous path writes RFC 7807 problem+json (title/status/detail
// keys, no "error" key). Nothing in that body marks query-serve as having
// answered, so it must fall through to ErrCellUnavailable rather than read as
// a repo-filter miss.
func TestCellV4_ProblemJSON404IsCellUnavailable(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"title":"Not Found","status":404,"detail":"no route for this path"}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	_, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "q"}, []string{"01JREPOA"})
	if !errors.Is(err, ErrCellUnavailable) {
		t.Fatalf("err = %v, want ErrCellUnavailable for a problem+json 404", err)
	}
	if errors.Is(err, ErrRepoFilterUnmatched) {
		t.Fatalf("err = %v, want NOT ErrRepoFilterUnmatched for a problem+json 404", err)
	}
}
