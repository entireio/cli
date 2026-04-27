package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCloudClient_CreateDispatch_Happy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/dispatches/generate" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["repos"] == nil {
			t.Fatalf("bad body: %v", body)
		}
		repos, ok := body["repos"].([]any)
		if !ok || len(repos) != 1 || repos[0] != "entireio/cli" {
			t.Fatalf("bad repos payload: %v", body["repos"])
		}
		if _, ok := body["repo"]; ok {
			t.Fatalf("did not expect repo in request body: %v", body)
		}
		if _, ok := body["wait"]; ok {
			t.Fatalf("did not expect wait in request body: %v", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"window":{"normalized_since":"2026-04-09T00:00:00Z","normalized_until":"2026-04-16T00:00:00Z"},"covered_repos":["entireio/cli"],"repos":[],"totals":{"checkpoints":0,"used_checkpoint_count":0,"branches":0,"files_touched":0},"warnings":{"access_denied_count":0,"pending_count":0,"failed_count":0,"unknown_count":0,"uncategorized_count":0},"generated_markdown":"hi"}`)) //nolint:errcheck // test fixture response
	}))
	defer srv.Close()

	client := NewCloudClient(CloudConfig{BaseURL: srv.URL, Token: "t"})
	got, err := client.CreateDispatch(ctx, CreateDispatchRequest{
		Repos:    []string{"entireio/cli"},
		Since:    "2026-04-09T00:00:00Z",
		Until:    "2026-04-16T00:00:00Z",
		Generate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedMarkdown != "hi" {
		t.Fatalf("bad generated markdown: %q", got.GeneratedMarkdown)
	}
}

func TestCloudClient_CreateDispatch_OmitsBranchesAndOrgsFromPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["orgs"]; ok {
			t.Fatalf("did not expect orgs in payload: %v", body)
		}
		if _, ok := body["branches"]; ok {
			t.Fatalf("did not expect branches in payload: %v", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"window":{"normalized_since":"2026-04-09T00:00:00Z","normalized_until":"2026-04-16T00:00:00Z"},"covered_repos":["entireio/cli"],"repos":[],"generated_markdown":"hi"}`)) //nolint:errcheck // test fixture response
	}))
	defer srv.Close()

	client := NewCloudClient(CloudConfig{BaseURL: srv.URL, Token: "t"})
	_, err := client.CreateDispatch(ctx, CreateDispatchRequest{
		Repos:    []string{"entireio/cli"},
		Since:    "2026-04-09T00:00:00Z",
		Until:    "2026-04-16T00:00:00Z",
		Generate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCloudClient_CreateDispatch_Unauthorized(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewCloudClient(CloudConfig{BaseURL: srv.URL, Token: ""})
	_, err := client.CreateDispatch(ctx, CreateDispatchRequest{Repos: []string{"x/y"}})
	if err == nil || !strings.Contains(err.Error(), "entire login") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestNewCloudClient_DefaultHTTPClientUsesLongTimeout(t *testing.T) {
	t.Parallel()

	client := NewCloudClient(CloudConfig{BaseURL: "http://example.com", Token: "t"})
	if client.http == nil {
		t.Fatal("expected http client")
	}
	if client.http.Timeout != 120*time.Second {
		t.Fatalf("expected default http timeout %s, got %s", 120*time.Second, client.http.Timeout)
	}
}

func TestNewCloudClient_ConfiguredTimeoutStillApplies(t *testing.T) {
	t.Parallel()

	timeout := 45 * time.Second
	client := NewCloudClient(CloudConfig{
		BaseURL: "http://example.com",
		Token:   "t",
		Timeout: timeout,
	})
	if client.http == nil {
		t.Fatal("expected http client")
	}
	if client.http.Timeout != timeout {
		t.Fatalf("expected configured timeout %s, got %s", timeout, client.http.Timeout)
	}
}

func TestCloudClient_CreateDispatch_EscapesErrorBody(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		if _, err := w.Write([]byte("\x1b[31mboom\x1b[0m")); err != nil {
			t.Fatal(err)
		}
	}))
	defer srv.Close()

	client := NewCloudClient(CloudConfig{BaseURL: srv.URL, Token: "t"})
	_, err := client.CreateDispatch(ctx, CreateDispatchRequest{Repos: []string{"x/y"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "dispatch service returned status 502") {
		t.Fatalf("expected simplified status error, got %v", err)
	}
	if strings.Contains(err.Error(), "/api/v1/dispatches/generate") {
		t.Fatalf("did not expect endpoint path in user-facing error, got %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote("\x1b[31mboom\x1b[0m")) {
		t.Fatalf("expected quoted error body, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_IgnoresUnknownResponseFields(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"window":{"normalized_since":"2026-04-09T00:00:00Z","normalized_until":"2026-04-16T00:00:00Z"},"covered_repos":["entireio/cli"],"repos":[],"generated_markdown":"hi","unexpected":true}`)) //nolint:errcheck // test fixture response
	}))
	defer srv.Close()

	client := NewCloudClient(CloudConfig{BaseURL: srv.URL, Token: "t"})
	got, err := client.CreateDispatch(ctx, CreateDispatchRequest{
		Repos:    []string{"entireio/cli"},
		Since:    "2026-04-09T00:00:00Z",
		Until:    "2026-04-16T00:00:00Z",
		Generate: true,
	})
	if err != nil {
		t.Fatalf("expected forward-compatible decode, got error: %v", err)
	}
	if got.GeneratedMarkdown != "hi" {
		t.Fatalf("expected known fields to decode, got %q", got.GeneratedMarkdown)
	}
}

func TestCloudClient_CreateDispatch_AcceptsBranchesResponseField(t *testing.T) {
	t.Parallel()

	client := NewCloudClient(CloudConfig{
		BaseURL: "http://example.com",
		Token:   "t",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"window":{"normalized_since":"2026-04-09T00:00:00Z","normalized_until":"2026-04-16T00:00:00Z"},"covered_repos":["entireio/cli"],"branches":["main","release"],"repos":[],"generated_markdown":"hi","totals":{"checkpoints":0,"used_checkpoint_count":0,"branches":2,"files_touched":0},"warnings":{"access_denied_count":0,"pending_count":0,"failed_count":0,"unknown_count":0,"uncategorized_count":0}}`,
					)),
				}, nil
			}),
		},
	})
	got, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{
		Repos:    []string{"entireio/cli"},
		Since:    "2026-04-09T00:00:00Z",
		Until:    "2026-04-16T00:00:00Z",
		Generate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Branches.Values, []string{"main", "release"}) {
		t.Fatalf("unexpected branches response: %+v", got.Branches)
	}
	if got.Branches.All {
		t.Fatalf("did not expect all-branches sentinel: %+v", got.Branches)
	}
}

func TestCloudClient_CreateDispatch_AcceptsAllBranchesSentinelInResponseField(t *testing.T) {
	t.Parallel()

	client := NewCloudClient(CloudConfig{
		BaseURL: "http://example.com",
		Token:   "t",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"window":{"normalized_since":"2026-04-09T00:00:00Z","normalized_until":"2026-04-16T00:00:00Z"},"covered_repos":["entireio/cli"],"branches":"all","repos":[],"generated_markdown":"hi","totals":{"checkpoints":0,"used_checkpoint_count":0,"branches":2,"files_touched":0},"warnings":{"access_denied_count":0,"pending_count":0,"failed_count":0,"unknown_count":0,"uncategorized_count":0}}`,
					)),
				}, nil
			}),
		},
	})
	got, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{
		Repos:    []string{"entireio/cli"},
		Since:    "2026-04-09T00:00:00Z",
		Until:    "2026-04-16T00:00:00Z",
		Generate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Branches.All {
		t.Fatalf("expected all-branches sentinel, got %+v", got.Branches)
	}
	if len(got.Branches.Values) != 0 {
		t.Fatalf("did not expect explicit branch values, got %+v", got.Branches)
	}
}

func TestCloudClient_CreateDispatch_AcceptsVoiceResponseField(t *testing.T) {
	t.Parallel()

	client := NewCloudClient(CloudConfig{
		BaseURL: "http://example.com",
		Token:   "t",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusCreated,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"window":{"normalized_since":"2026-04-09T00:00:00Z","normalized_until":"2026-04-16T00:00:00Z"},"covered_repos":["entireio/cli"],"branches":["main"],"voice":"calm and direct","repos":[],"generated_markdown":"hi","totals":{"checkpoints":0,"used_checkpoint_count":0,"branches":1,"files_touched":0},"warnings":{"access_denied_count":0,"pending_count":0,"failed_count":0,"unknown_count":0,"uncategorized_count":0}}`,
					)),
				}, nil
			}),
		},
	})
	got, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{
		Repos:    []string{"entireio/cli"},
		Since:    "2026-04-09T00:00:00Z",
		Until:    "2026-04-16T00:00:00Z",
		Generate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Voice == nil || *got.Voice != "calm and direct" {
		t.Fatalf("unexpected voice response: %v", got.Voice)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
