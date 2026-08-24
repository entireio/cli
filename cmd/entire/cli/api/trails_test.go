package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_TrailsEnabledEscapesPathComponents(t *testing.T) {
	t.Parallel()

	var gotURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"items":[],"nextPageToken":null,"totalCount":0}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	ok, err := c.TrailsEnabled(context.Background(), "g/h", "acme?org", "repo#frag")
	if err != nil {
		t.Fatalf("TrailsEnabled: %v", err)
	}
	if !ok {
		t.Fatal("enabled = false, want true")
	}
	want := "/api/v1/changes/g%2Fh/acme%3Forg/repo%23frag?pageSize=1"
	if gotURI != want {
		t.Errorf("request URI = %q, want %q", gotURI, want)
	}
}

func TestClient_RewritesResolvedChangeReviewRoute(t *testing.T) {
	t.Parallel()
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClientWithBaseURL("tok", server.URL)
	c.SetChangeRoute("trl/one", "/api/v1/changes/gh/acme/repo/42")
	resp, err := c.Get(context.Background(), "/api/v1/changes/trl%2Fone/reviews/comments")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "/api/v1/changes/gh/acme/repo/42/reviews/comments"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestClient_ChangeRequestsUseCamelCase(t *testing.T) {
	t.Parallel()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithBaseURL("tok", server.URL)
	resp, err := client.Post(context.Background(), "/api/v1/changes/gh/acme/repo", ChangeCreateRequest{
		Title: "test", BranchName: "feature/test", BranchAction: "link",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got["branchName"] != "feature/test" || got["branchAction"] != "link" {
		t.Fatalf("body = %#v", got)
	}
	if _, ok := got["branch_name"]; ok {
		t.Fatalf("body contains snake_case: %#v", got)
	}
}

func TestClient_TrailsEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		wantOK     bool
		wantErrNil bool
		// wantPaths is every request TrailsEnabled is expected to make, in
		// order. A 404 on the canonical path retries against the legacy one
		// (see TestClient_TrailsEnabled_FallsBackToLegacyPathOn404 for the
		// other order of that fallback, where the legacy probe succeeds).
		wantPaths []string
	}{
		{"enabled (200)", http.StatusOK, `{"items":[],"nextPageToken":null,"totalCount":0}`, true, true,
			[]string{"/api/v1/changes/gh/acme/repo"}},
		{"enabled empty (200)", http.StatusOK, `{"items":[],"nextPageToken":null,"totalCount":0}`, true, true,
			[]string{"/api/v1/changes/gh/acme/repo"}},
		{"not enabled, no legacy route either (404 -> 404)", http.StatusNotFound, `{"error":"not found"}`, false, true,
			[]string{"/api/v1/changes/gh/acme/repo", "/api/v1/trails/gh/acme/repo"}},
		{"forbidden (403)", http.StatusForbidden, `{"error":"forbidden"}`, false, true,
			[]string{"/api/v1/changes/gh/acme/repo"}},
		{"gone (410)", http.StatusGone, `{"error":"gone"}`, false, true,
			[]string{"/api/v1/changes/gh/acme/repo"}},
		{"unauthorized (401)", http.StatusUnauthorized, `{"error":"unauthorized"}`, false, false,
			[]string{"/api/v1/changes/gh/acme/repo"}},
		{"server error (500)", http.StatusInternalServerError, `{"error":"boom"}`, false, false,
			[]string{"/api/v1/changes/gh/acme/repo"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPaths, gotQueries []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPaths = append(gotPaths, r.URL.Path)
				gotQueries = append(gotQueries, r.URL.RawQuery)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck // test handler
			}))
			defer server.Close()

			c := NewClient("tok")
			c.baseURL = server.URL

			ok, err := c.TrailsEnabled(context.Background(), "gh", "acme", "repo")
			if (err == nil) != tt.wantErrNil {
				t.Fatalf("err = %v, wantErrNil = %v", err, tt.wantErrNil)
			}
			if ok != tt.wantOK {
				t.Errorf("enabled = %v, want %v", ok, tt.wantOK)
			}
			if len(gotPaths) != len(tt.wantPaths) {
				t.Fatalf("requests = %v, want %v", gotPaths, tt.wantPaths)
			}
			for i, wantPath := range tt.wantPaths {
				if gotPaths[i] != wantPath {
					t.Errorf("request %d path = %q, want %q", i, gotPaths[i], wantPath)
				}
				if gotQueries[i] != "pageSize=1" {
					t.Errorf("request %d query = %q, want pageSize=1", i, gotQueries[i])
				}
			}
		})
	}
}

// The other order of the 404 fallback: the canonical path 404s (server not
// yet on the 1b route), but the legacy path still answers, so a CLI released
// ahead of the server rollout must not cache the repo as disabled.
func TestClient_TrailsEnabled_FallsBackToLegacyPathOn404(t *testing.T) {
	t.Parallel()

	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		if r.URL.Path == "/api/v1/trails/gh/acme/repo" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"items":[],"nextPageToken":null,"totalCount":0}`)) //nolint:errcheck // test handler
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	ok, err := c.TrailsEnabled(context.Background(), "gh", "acme", "repo")
	if err != nil {
		t.Fatalf("TrailsEnabled: %v", err)
	}
	if !ok {
		t.Error("enabled = false, want true (legacy route still serves this repo)")
	}
	want := []string{"/api/v1/changes/gh/acme/repo", "/api/v1/trails/gh/acme/repo"}
	if len(gotPaths) != len(want) || gotPaths[0] != want[0] || gotPaths[1] != want[1] {
		t.Errorf("requests = %v, want %v", gotPaths, want)
	}
}
