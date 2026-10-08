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
		w.Write([]byte(`{"items":[],"next_cursor":null,"total_count":0}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClientWithBaseURL("tok", server.URL)

	ok, err := c.TrailsEnabled(context.Background(), "g/h", "acme?org", "repo#frag")
	if err != nil {
		t.Fatalf("TrailsEnabled: %v", err)
	}
	if !ok {
		t.Fatal("enabled = false, want true")
	}
	want := "/api/v1/trails/g%2Fh/acme%3Forg/repo%23frag?per_page=1"
	if gotURI != want {
		t.Errorf("request URI = %q, want %q", gotURI, want)
	}
}

func TestClient_RewritesResolvedTrailReviewRoute(t *testing.T) {
	t.Parallel()
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClientWithBaseURL("tok", server.URL)
	c.SetTrailRoute("trl/one", "/api/v1/trails/gh/acme/repo/42")
	resp, err := c.Get(context.Background(), "/api/v1/trails/trl%2Fone/reviews/comments")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "/api/v1/trails/gh/acme/repo/42/reviews/comments"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestClient_TrailRequestsUseSnakeCase(t *testing.T) {
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
	resp, err := client.Post(context.Background(), "/api/v1/trails/gh/acme/repo", TrailCreateRequest{
		Title: "test", BranchName: "feature/test", BranchAction: "link",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got["branch_name"] != "feature/test" || got["branch_action"] != "link" {
		t.Fatalf("body = %#v", got)
	}
	for _, key := range []string{"branchName", "branchAction"} {
		if _, ok := got[key]; ok {
			t.Fatalf("body contains camelCase %q: %#v", key, got)
		}
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
	}{
		{"enabled (200)", http.StatusOK, `{"items":[],"next_cursor":null,"total_count":0}`, true, true},
		{"enabled empty (200)", http.StatusOK, `{"items":[],"next_cursor":null,"total_count":0}`, true, true},
		{"not enabled (404)", http.StatusNotFound, `{"error":"not found"}`, false, true},
		{"forbidden (403)", http.StatusForbidden, `{"type":"https://example.test/problems/forbidden","title":"Forbidden","status":403,"detail":"No access","code":"forbidden","request_id":"request-example"}`, false, true},
		{"gone (410)", http.StatusGone, `{"error":"gone"}`, false, true},
		{"unauthorized (401)", http.StatusUnauthorized, `{"error":"unauthorized"}`, false, false},
		{"server error (500)", http.StatusInternalServerError, `{"error":"boom"}`, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath, gotQuery string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck // test handler
			}))
			defer server.Close()

			c := NewClientWithBaseURL("tok", server.URL)

			ok, err := c.TrailsEnabled(context.Background(), "gh", "acme", "repo")
			if (err == nil) != tt.wantErrNil {
				t.Fatalf("err = %v, wantErrNil = %v", err, tt.wantErrNil)
			}
			if ok != tt.wantOK {
				t.Errorf("enabled = %v, want %v", ok, tt.wantOK)
			}
			if gotPath != "/api/v1/trails/gh/acme/repo" {
				t.Errorf("path = %q, want /api/v1/trails/gh/acme/repo", gotPath)
			}
			if gotQuery != "per_page=1" {
				t.Errorf("query = %q, want per_page=1", gotQuery)
			}
		})
	}
}
