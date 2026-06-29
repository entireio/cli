package api

import (
	"context"
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
		w.Write([]byte(`{"trails":[]}`)) //nolint:errcheck // test handler
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
	want := "/api/v1/trails/g%2Fh/acme%3Forg/repo%23frag?limit=1"
	if gotURI != want {
		t.Errorf("request URI = %q, want %q", gotURI, want)
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
		{"enabled (200)", http.StatusOK, `{"trails":[],"total":0}`, true, true},
		{"enabled empty (200)", http.StatusOK, `{"trails":[]}`, true, true},
		{"not enabled (404)", http.StatusNotFound, `{"error":"not found"}`, false, true},
		{"forbidden (403)", http.StatusForbidden, `{"error":"forbidden"}`, false, true},
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

			c := NewClient("tok")
			c.baseURL = server.URL

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
			if gotQuery != "limit=1" {
				t.Errorf("query = %q, want limit=1", gotQuery)
			}
		})
	}
}
