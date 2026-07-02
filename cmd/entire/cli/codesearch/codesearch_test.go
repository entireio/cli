package codesearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestSearch_Success(t *testing.T) {
	t.Parallel()

	want := SearchResponse{
		Query: "handleRequest",
		Stats: Stats{
			TotalMatches:  3,
			TotalFiles:    2,
			DurationMs:    42.5,
			ReposSearched: 1,
		},
		RepoStats: []RepoStats{
			{Repo: "entireio/cli", MatchCount: 3, FileCount: 2},
		},
		Results: []Result{
			{
				Repo:          "entireio/cli",
				Path:          "cmd/server/main.go",
				Line:          15,
				Column:        6,
				ContextBefore: []string{"", "// handleRequest processes incoming requests."},
				ContextLine:   "func handleRequest(w http.ResponseWriter, r *http.Request) {",
				ContextAfter:  []string{"\tctx := r.Context()"},
				Score:         0.95,
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/api/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Query != "handleRequest" {
			t.Errorf("unexpected query: %s", req.Query)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want) //nolint:errcheck // test handler, error irrelevant
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("test-token", srv.URL)
	got, err := Search(context.Background(), client, SearchRequest{
		Query:      "handleRequest",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if got.Stats.TotalMatches != want.Stats.TotalMatches {
		t.Errorf("TotalMatches = %d, want %d", got.Stats.TotalMatches, want.Stats.TotalMatches)
	}
	if len(got.Results) != len(want.Results) {
		t.Fatalf("len(Results) = %d, want %d", len(got.Results), len(want.Results))
	}
	if got.Results[0].Path != want.Results[0].Path {
		t.Errorf("Results[0].Path = %q, want %q", got.Results[0].Path, want.Results[0].Path)
	}
}

func TestSearch_APIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "insufficient permissions"}) //nolint:errcheck // test handler
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("test-token", srv.URL)
	_, err := Search(context.Background(), client, SearchRequest{Query: "test"})
	if err == nil {
		t.Fatal("Search() expected error, got nil")
	}
	if want := "code search error (403): insufficient permissions"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestSearch_NonJSONError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("Bad Gateway")) //nolint:errcheck // test handler
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("test-token", srv.URL)
	_, err := Search(context.Background(), client, SearchRequest{Query: "test"})
	if err == nil {
		t.Fatal("Search() expected error, got nil")
	}
}

func TestSearch_ResponseTooLarge(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write more than maxResponseBytes (8 MiB).
		buf := make([]byte, maxResponseBytes+1)
		for i := range buf {
			buf[i] = 'x'
		}
		w.Write(buf) //nolint:errcheck // test handler
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("test-token", srv.URL)
	_, err := Search(context.Background(), client, SearchRequest{Query: "test"})
	if err == nil {
		t.Fatal("Search() expected error for oversized response, got nil")
	}
	if want := "exceeds"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want containing %q", err.Error(), want)
	}
}
