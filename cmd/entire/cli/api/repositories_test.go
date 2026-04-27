package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_ListRepositories_SendsSortAndDecodesResponse(t *testing.T) {
	t.Parallel()

	var gotPath, gotRawQuery, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"repositories":[` + //nolint:errcheck // test handler
			`{"full_name":"entireio/cli","checkpoint_count":12},` +
			`{"full_name":"entireio/entire.io","checkpoint_count":3}` +
			`]}`))
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	repos, err := c.ListRepositories(context.Background(), RepositorySortRecent)
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/api/v1/repositories" {
		t.Errorf("path = %q, want /api/v1/repositories", gotPath)
	}
	if gotRawQuery != "sort=recent" {
		t.Errorf("query = %q, want sort=recent", gotRawQuery)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", gotAuth)
	}

	if len(repos) != 2 {
		t.Fatalf("len(repos) = %d, want 2", len(repos))
	}
	if repos[0].FullName != "entireio/cli" || repos[0].CheckpointCount != 12 {
		t.Errorf("repos[0] = %+v", repos[0])
	}
	if repos[1].FullName != "entireio/entire.io" || repos[1].CheckpointCount != 3 {
		t.Errorf("repos[1] = %+v", repos[1])
	}
}

func TestClient_ListRepositories_OmitsQueryWhenSortEmpty(t *testing.T) {
	t.Parallel()

	var gotRawQuery string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"repositories":[]}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	if _, err := c.ListRepositories(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if gotRawQuery != "" {
		t.Errorf("query = %q, want empty", gotRawQuery)
	}
}

func TestClient_ListRepositories_ErrorResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"Failed to fetch repositories"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	_, err := c.ListRepositories(context.Background(), RepositorySortRecent)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "Failed to fetch repositories") {
		t.Errorf("error = %v, want message from body", err)
	}
}
