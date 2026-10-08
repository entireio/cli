package codesearch

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
		if r.URL.Path != "/api/v1/search/api/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if q := r.URL.Query().Get("q"); q != "handleRequest" {
			t.Errorf("unexpected query param q: %s", q)
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
	if !strings.Contains(err.Error(), "insufficient permissions") {
		t.Errorf("error = %q, want containing 'insufficient permissions'", err.Error())
	}
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected HTTPError with status 403, got %v", err)
	}
}

func TestSearch_NonJSONError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("  Bad Gateway\n")) //nolint:errcheck // test handler — trailing whitespace exercises TrimSpace
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("test-token", srv.URL)
	_, err := Search(context.Background(), client, SearchRequest{Query: "test"})
	if err == nil {
		t.Fatal("Search() expected error, got nil")
	}
	// Body text should surface (trimmed) in the error message.
	if !strings.Contains(err.Error(), "Bad Gateway") {
		t.Errorf("error = %q, want containing 'Bad Gateway'", err.Error())
	}
	// Should wrap *api.HTTPError with the correct status code.
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway {
		t.Errorf("expected HTTPError with status 502, got %v", err)
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
