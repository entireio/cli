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

// TestCellV4_LargeResponseDecodesFully is the regression test for the 1 MiB
// response cap: a busy cell returns a valid JSON body larger than the old
// io.LimitReader(resp.Body, 1<<20) limit (rows carry full transcript text).
// The old cap truncated the body mid-JSON, failed the decode, and dropped the
// whole region from the cross-cell merge. The response must now decode intact.
func TestCellV4_LargeResponseDecodesFully(t *testing.T) {
	t.Parallel()

	// A single checkpoint whose prompt alone exceeds the old 1 MiB cap.
	bigPrompt := strings.Repeat("transcript ", (1<<20)/11+4096)
	payload, err := json.Marshal(&Response{
		Results: []Result{{
			Type:       TypeCheckpoint,
			Meta:       Meta{Score: 1.0, Tier: iptrTest(0)},
			Checkpoint: &CheckpointResult{ID: "big", Prompt: bigPrompt},
		}},
		Total: 1,
		Page:  1,
	})
	if err != nil {
		t.Fatalf("building payload: %v", err)
	}
	if len(payload) <= 1<<20 {
		t.Fatalf("test payload is %d bytes, must exceed the old 1 MiB cap to exercise the bug", len(payload))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, string(payload))
	}))
	defer srv.Close()

	resp, err := CellV4(context.Background(), api.NewClientWithBaseURL("tok", srv.URL), Config{Query: "transcript"}, nil)
	if err != nil {
		t.Fatalf("large response should decode, got error: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Checkpoint == nil {
		t.Fatalf("results did not decode: %+v", resp.Results)
	}
	if got := resp.Results[0].Checkpoint.Prompt; got != bigPrompt {
		t.Errorf("prompt truncated: got %d bytes, want %d", len(got), len(bigPrompt))
	}
}

func iptrTest(i int) *int { return &i }

// TestReadCappedBody confirms the read guard: bodies at or under the cap are
// returned intact, and an over-cap body is an explicit error rather than a
// silently truncated slice fed to the JSON decoder.
func TestReadCappedBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		size    int
		cap     int64
		wantErr bool
	}{
		{"under cap", 50, 100, false},
		{"exactly at cap", 100, 100, false},
		{"one over cap", 101, 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, err := readCappedBody(strings.NewReader(strings.Repeat("a", tt.size)), tt.cap)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("size %d cap %d: want an overflow error, got nil", tt.size, tt.cap)
				}
				if !strings.Contains(err.Error(), "exceeded") {
					t.Errorf("error = %q, want it to mention the exceeded read limit", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("size %d cap %d: unexpected error: %v", tt.size, tt.cap, err)
			}
			if len(body) != tt.size {
				t.Errorf("read %d bytes, want %d", len(body), tt.size)
			}
		})
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
