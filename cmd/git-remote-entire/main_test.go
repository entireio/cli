package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitActionFromRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		method string
		path   string
		query  string
		want   string
	}{
		{"upload-pack RPC", http.MethodPost, "/et/p/r/git-upload-pack", "", "pull"},
		{"receive-pack RPC", http.MethodPost, "/et/p/r/git-receive-pack", "", "push"},
		{"info/refs pull", http.MethodGet, "/et/p/r/info/refs", "service=git-upload-pack", "pull"},
		{"info/refs push", http.MethodGet, "/et/p/r/info/refs", "service=git-receive-pack", "push"},
		{"info/refs no service", http.MethodGet, "/et/p/r/info/refs", "", ""},
		{"unrelated GET", http.MethodGet, "/et/p/r/objects/info/packs", "", ""},
		{"unrelated POST", http.MethodPost, "/et/p/r/whatever", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(context.Background(), tc.method, "https://host"+tc.path+"?"+tc.query, nil)
			if got := gitActionFromRequest(req); got != tc.want {
				t.Fatalf("gitActionFromRequest(%s %s?%s) = %q, want %q", tc.method, tc.path, tc.query, got, tc.want)
			}
		})
	}
}
