package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_RevokeCurrentToken_SendsDeleteWithBearer(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	if err := c.RevokeCurrentToken(context.Background()); err != nil {
		t.Fatalf("RevokeCurrentToken() error = %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/auth/tokens/current" {
		t.Errorf("path = %q, want /api/v1/auth/tokens/current", gotPath)
	}
	if gotAuth != testBearerHeader {
		t.Errorf("Authorization = %q, want %q", gotAuth, testBearerHeader)
	}
}

func TestClient_RevokeCurrentToken_ReturnsHTTPErrorOn401(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Not authenticated"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	err := c.RevokeCurrentToken(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !IsHTTPErrorStatus(err, http.StatusUnauthorized) {
		t.Fatalf("IsHTTPErrorStatus(err, 401) = false; err = %v", err)
	}
	var apiErr *HTTPError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err does not wrap *HTTPError: %v", err)
	}
	if apiErr.Message != "Not authenticated" {
		t.Errorf("HTTPError.Message = %q, want %q", apiErr.Message, "Not authenticated")
	}
}

func TestClient_ListTokens_DecodesResponse(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tokens":[` + //nolint:errcheck // test handler
			`{"id":"tok-1","user_id":"u-1","name":"laptop","scope":"cli","expires_at":"2027-01-01T00:00:00Z","last_used_at":"2026-04-01T00:00:00Z","created_at":"2026-01-01T00:00:00Z"},` +
			`{"id":"tok-2","user_id":"u-1","name":"desktop","scope":"cli","expires_at":"2027-01-01T00:00:00Z","last_used_at":null,"created_at":"2026-02-01T00:00:00Z"}` +
			`]}`))
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	tokens, err := c.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens() error = %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v1/auth/tokens" {
		t.Errorf("path = %q, want /api/v1/auth/tokens", gotPath)
	}
	if gotAuth != testBearerHeader {
		t.Errorf("Authorization = %q, want %q", gotAuth, testBearerHeader)
	}

	if len(tokens) != 2 {
		t.Fatalf("len(tokens) = %d, want 2", len(tokens))
	}
	if tokens[0].ID != "tok-1" || tokens[0].Name != "laptop" {
		t.Errorf("tokens[0] = %+v", tokens[0])
	}
	if tokens[0].LastUsedAt == nil || *tokens[0].LastUsedAt != "2026-04-01T00:00:00Z" {
		t.Errorf("tokens[0].LastUsedAt = %v, want non-nil pointer to 2026-04-01", tokens[0].LastUsedAt)
	}
	if tokens[1].LastUsedAt != nil {
		t.Errorf("tokens[1].LastUsedAt = %v, want nil", tokens[1].LastUsedAt)
	}
}

func TestClient_ListTokens_ReturnsHTTPErrorOn401(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Not authenticated"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	_, err := c.ListTokens(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !IsHTTPErrorStatus(err, http.StatusUnauthorized) {
		t.Fatalf("IsHTTPErrorStatus(err, 401) = false; err = %v", err)
	}
}

func TestClient_RevokeToken_SendsDeleteWithEscapedID(t *testing.T) {
	t.Parallel()

	var gotMethod, gotEscapedPath, gotDecodedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotEscapedPath = r.URL.EscapedPath()
		gotDecodedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	// Use an id that needs URL escaping to verify we don't blindly concat.
	if err := c.RevokeToken(context.Background(), "abc/def 1"); err != nil {
		t.Fatalf("RevokeToken() error = %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if want := "/api/v1/auth/tokens/abc%2Fdef%201"; gotEscapedPath != want {
		t.Errorf("escaped path = %q, want %q", gotEscapedPath, want)
	}
	if want := "/api/v1/auth/tokens/abc/def 1"; gotDecodedPath != want {
		t.Errorf("decoded path = %q, want %q", gotDecodedPath, want)
	}
}

func TestClient_RevokeToken_ReturnsErrorBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Token not found"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	err := c.RevokeToken(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "Token not found") {
		t.Errorf("error = %v, want message from body", err)
	}
	if !IsHTTPErrorStatus(err, http.StatusNotFound) {
		t.Errorf("IsHTTPErrorStatus(err, 404) = false; err = %v", err)
	}
}
