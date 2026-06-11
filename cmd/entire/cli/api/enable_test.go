package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_ReportEnable_PostsRemoteAndDecodesResponse(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth string
	var gotBody EnableRepoRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"connected":true,"repo":{"full_name":"entireio/cli","github_id":42,"private":true}}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	out, err := c.ReportEnable(context.Background(), "git@github.com:entireio/cli.git")
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/cli/enable" {
		t.Errorf("path = %q, want /api/v1/cli/enable", gotPath)
	}
	if gotAuth != testBearerHeader {
		t.Errorf("Authorization = %q, want %q", gotAuth, testBearerHeader)
	}
	if gotBody.RemoteURL != "git@github.com:entireio/cli.git" {
		t.Errorf("remote_url = %q", gotBody.RemoteURL)
	}
	if !out.Connected {
		t.Errorf("connected = false, want true")
	}
	if out.Repo == nil || out.Repo.FullName != "entireio/cli" {
		t.Errorf("repo = %+v", out.Repo)
	}
}

func TestClient_ReportEnable_ReturnsInstallURLWhenNotConnected(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"connected":false,"install_url":"https://github.com/apps/entire/installations/new"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	out, err := c.ReportEnable(context.Background(), "https://github.com/secret/private.git")
	if err != nil {
		t.Fatal(err)
	}

	if out.Connected {
		t.Errorf("connected = true, want false")
	}
	if out.InstallURL != "https://github.com/apps/entire/installations/new" {
		t.Errorf("install_url = %q", out.InstallURL)
	}
}

func TestClient_ReportEnable_ReturnsErrorOnFailureStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"Unsupported or non-GitHub remote_url"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	if _, err := c.ReportEnable(context.Background(), "git@gitlab.com:foo/bar.git"); err == nil {
		t.Fatal("expected error for 400 response, got nil")
	}
}
