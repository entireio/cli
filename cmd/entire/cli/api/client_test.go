package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerTransport_InjectsAuthHeader(t *testing.T) {
	t.Parallel()

	var gotAuth string
	var gotUA string
	var gotAccept string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &bearerTransport{
		token: "test-token-123",
		base:  http.DefaultTransport,
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotAuth != "Bearer test-token-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer test-token-123")
	}
	if gotUA != "entire-cli" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "entire-cli")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json")
	}
}

func TestBearerTransport_PreservesExistingAcceptHeader(t *testing.T) {
	t.Parallel()

	var gotAccept string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &bearerTransport{
		token: "tok",
		base:  http.DefaultTransport,
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotAccept != "text/plain" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/plain")
	}
}

func TestBearerTransport_DoesNotMutateOriginalRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &bearerTransport{
		token: "tok",
		base:  http.DefaultTransport,
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("original request Authorization = %q, want empty (should not be mutated)", got)
	}
}

func TestClient_Get(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer my-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	c := NewClient("my-token")
	c.baseURL = server.URL

	resp, err := c.Get(context.Background(), "/api/v1/test")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestClient_Post_JSON(t *testing.T) {
	t.Parallel()

	var gotBody map[string]string
	var gotContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)  //nolint:errcheck // test handler
		json.Unmarshal(body, &gotBody) //nolint:errcheck // test handler
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	resp, err := c.Post(context.Background(), "/api/v1/things", map[string]string{"name": "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["name"] != "test" {
		t.Errorf("body name = %q, want %q", gotBody["name"], "test")
	}
}

func TestClient_Post_NilBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "" {
			t.Errorf("Content-Type should be empty for nil body, got %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClient("tok")
	c.baseURL = server.URL

	resp, err := c.Post(context.Background(), "/api/v1/action", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCheckResponse_Success(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
	}
	if err := CheckResponse(resp); err != nil {
		t.Errorf("CheckResponse(200) = %v, want nil", err)
	}
}

func TestCheckResponse_ErrorWithJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error": "insufficient permissions"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	resp, err := http.Get(server.URL) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	err = CheckResponse(resp)
	if err == nil {
		t.Fatal("CheckResponse(403) = nil, want error")
	}
	if got := err.Error(); got != "API error: insufficient permissions (status 403)" {
		t.Errorf("error = %q", got)
	}
}

func TestCheckResponse_ErrorWithPlainText(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error")) //nolint:errcheck // test handler
	}))
	defer server.Close()

	resp, err := http.Get(server.URL) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	err = CheckResponse(resp)
	if err == nil {
		t.Fatal("CheckResponse(500) = nil, want error")
	}
	if got := err.Error(); got != "API error: internal server error (status 500)" {
		t.Errorf("error = %q", got)
	}
}

func TestDecodeJSONResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id": "abc", "status": "ok"}`)) //nolint:errcheck // test handler
	}))
	defer server.Close()

	resp, err := http.Get(server.URL) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := DecodeJSON(resp, &result); err != nil {
		t.Fatal(err)
	}

	if result.ID != "abc" {
		t.Errorf("ID = %q, want %q", result.ID, "abc")
	}
	if result.Status != "ok" {
		t.Errorf("Status = %q, want %q", result.Status, "ok")
	}
}
