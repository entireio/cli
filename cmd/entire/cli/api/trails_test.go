package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepoChangesPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, repoID, want string
	}{
		{"placement ID", "repo-placement-id", "/api/v1/repos/repo-placement-id/changes"},
		{"trimmed and escaped", " \trepo/id?#frag \n", "/api/v1/repos/repo%2Fid%3F%23frag/changes"},
		{"empty", "", ""},
		{"whitespace", " \t\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, err := RepoChangesPath(tc.repoID)
			if tc.want == "" {
				require.ErrorContains(t, err, "resolve the repository's processing placement")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, path)
		})
	}
}

func TestClient_TrailsEnabledMissingRepoIDMakesNoRequest(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("missing repository ID must not send a request")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewClientWithBaseURL("tok", server.URL)
	for _, repoID := range []string{"", " \t\n"} {
		enabled, err := client.TrailsEnabled(t.Context(), repoID)
		require.ErrorContains(t, err, "repository ID is required")
		require.False(t, enabled)
	}
}

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

	ok, err := c.TrailsEnabled(context.Background(), " \trepo/id?#frag \n")
	if err != nil {
		t.Fatalf("TrailsEnabled: %v", err)
	}
	if !ok {
		t.Fatal("enabled = false, want true")
	}
	want := "/api/v1/repos/repo%2Fid%3F%23frag/changes?per_page=1"
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
	c.SetTrailRoute("trl/one", "/api/v1/repos/repo-id/changes/42")
	resp, err := c.Get(context.Background(), "/api/v1/trails/trl%2Fone/reviews/comments")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := "/api/v1/repos/repo-id/changes/42/reviews/comments"; got != want {
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
	ready := true
	resp, err := client.Patch(context.Background(), "/api/v1/repos/repo-id/changes/42", TrailUpdateRequest{
		ReadyForReview: &ready,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got["ready_for_review"] != true {
		t.Fatalf("body = %#v", got)
	}
	for _, key := range []string{"readyForReview"} {
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
		{"enabled no content (204)", http.StatusNoContent, "", true, true},
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

			ok, err := c.TrailsEnabled(context.Background(), "repo-placement-id")
			if (err == nil) != tt.wantErrNil {
				t.Fatalf("err = %v, wantErrNil = %v", err, tt.wantErrNil)
			}
			if ok != tt.wantOK {
				t.Errorf("enabled = %v, want %v", ok, tt.wantOK)
			}
			if gotPath != "/api/v1/repos/repo-placement-id/changes" {
				t.Errorf("path = %q, want /api/v1/repos/repo-placement-id/changes", gotPath)
			}
			if gotQuery != "per_page=1" {
				t.Errorf("query = %q, want per_page=1", gotQuery)
			}
		})
	}
}

func TestClient_RepoTrailsEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		status                          int
		body                            string
		wantEnabled, wantKnown, wantErr bool
	}{
		{"on", http.StatusOK, `{"id":"r","trails_enabled":true}`, true, true, false},
		{"off", http.StatusOK, `{"id":"r","trails_enabled":false}`, false, true, false},
		{"not reported", http.StatusOK, `{"id":"r"}`, false, false, false},
		{"malformed", http.StatusOK, `{"trails_enabled":`, false, false, true},
		{"not found", http.StatusNotFound, `{"error":"not found"}`, false, false, true},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck // test handler
			}))
			defer server.Close()

			enabled, known, err := NewClientWithBaseURL("tok", server.URL).RepoTrailsEnabled(context.Background(), "01ARZ3NDEKTSV4RRFFQ69G5FAX")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if enabled != tt.wantEnabled || known != tt.wantKnown {
				t.Errorf("(enabled, known) = (%v, %v), want (%v, %v)", enabled, known, tt.wantEnabled, tt.wantKnown)
			}
			if gotPath != "/api/v1/repos/01ARZ3NDEKTSV4RRFFQ69G5FAX" {
				t.Errorf("path = %q", gotPath)
			}
		})
	}
}
