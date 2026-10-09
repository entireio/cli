package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

const testDispatchRunID = "01KDISPATCHRUN0000000000000"

func newTestCloudClient(t *testing.T, baseURL, token string) *CloudClient {
	t.Helper()
	client := NewCloudClient(api.NewClientWithBaseURL(token, baseURL))
	client.pollInterval = time.Millisecond
	return client
}

// testDispatchRun is a finished run in the cell's camelCase wire shape.
func testDispatchRun(markdown string) map[string]any {
	return map[string]any{
		"id":     testDispatchRunID,
		"status": dispatchStatusComplete,
		"window": map[string]any{
			"normalizedSince": "2026-04-09T00:00:00Z",
			"normalizedUntil": "2026-04-16T00:00:00Z",
		},
		"coveredRepos":      []string{testRepoFullName},
		"repos":             []any{},
		"generatedMarkdown": markdown,
	}
}

func writeDispatchRun(t *testing.T, w http.ResponseWriter, status int, run map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(run); err != nil {
		t.Error(err)
	}
}

func TestCloudClient_CreateDispatch_Happy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		repos, ok := body["repos"].([]any)
		if !ok || len(repos) != 1 || repos[0] != testRepoSlug {
			t.Errorf("bad repos payload: %v", body["repos"])
		}
		for _, key := range []string{"generate", "jurisdiction", "orgs", "branches"} {
			if _, ok := body[key]; ok {
				t.Errorf("did not expect %s in request body: %v", key, body)
			}
		}
		if r.URL.RawQuery != "" {
			t.Errorf("did not expect a query, got %q", r.URL.RawQuery)
		}
		writeDispatchRun(t, w, http.StatusAccepted, testDispatchRun("hi"))
	}))
	defer srv.Close()

	got, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{
		Repos: []string{testRepoSlug},
		Since: "2026-04-09T00:00:00Z",
		Until: "2026-04-16T00:00:00Z",
	}, "eu")
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedMarkdown != "hi" {
		t.Fatalf("bad generated markdown: %q", got.GeneratedMarkdown)
	}
}

func TestCloudClient_CreateDispatch_PollsUntilComplete(t *testing.T) {
	t.Parallel()

	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == testDispatchEndpoint:
			run := testDispatchRun("")
			run["status"] = dispatchStatusGenerating
			writeDispatchRun(t, w, http.StatusAccepted, run)
		case r.Method == http.MethodGet && r.URL.Path == testDispatchEndpoint+"/"+testDispatchRunID:
			if reads.Add(1) < 3 {
				run := testDispatchRun("")
				run["status"] = dispatchStatusGenerating
				writeDispatchRun(t, w, http.StatusOK, run)
				return
			}
			writeDispatchRun(t, w, http.StatusOK, testDispatchRun("done"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	got, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedMarkdown != "done" {
		t.Fatalf("expected the polled markdown, got %q", got.GeneratedMarkdown)
	}
	if n := reads.Load(); n != 3 {
		t.Fatalf("expected 3 reads before completion, got %d", n)
	}
}

func TestCloudClient_CreateDispatch_FailedRunReturnsItsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			run := testDispatchRun("")
			run["status"] = dispatchStatusGenerating
			writeDispatchRun(t, w, http.StatusAccepted, run)
			return
		}
		run := testDispatchRun("")
		run["status"] = dispatchStatusFailed
		run["errorMessage"] = "generation timed out"
		writeDispatchRun(t, w, http.StatusOK, run)
	}))
	defer srv.Close()

	_, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err == nil || err.Error() != `dispatch generation failed: "generation timed out"` {
		t.Fatalf("expected the run's error message, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_StopsPollingWhenContextEnds(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		run := testDispatchRun("")
		run["status"] = dispatchStatusGenerating
		writeDispatchRun(t, w, http.StatusOK, run)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(ctx, CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err == nil || !strings.Contains(err.Error(), "still generating") {
		t.Fatalf("expected a still-generating error, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_UnknownStatusIsAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		run := testDispatchRun("hi")
		run["status"] = "queued"
		writeDispatchRun(t, w, http.StatusAccepted, run)
	}))
	defer srv.Close()

	_, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err == nil || !strings.Contains(err.Error(), `unknown status "queued"`) {
		t.Fatalf("expected an unknown-status error, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_SetsVersionedUserAgent(t *testing.T) {
	t.Parallel()

	var gotUserAgent atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent.Store(r.Header.Get("User-Agent"))
		writeDispatchRun(t, w, http.StatusAccepted, testDispatchRun("hi"))
	}))
	defer srv.Close()

	if _, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, ""); err != nil {
		t.Fatal(err)
	}
	if want := versioninfo.UserAgent(); gotUserAgent.Load() != want {
		t.Fatalf("expected User-Agent %q, got %v", want, gotUserAgent.Load())
	}
}

func TestCloudClient_CreateDispatch_Unauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestCloudClient(t, srv.URL, "").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err == nil || !strings.Contains(err.Error(), "entire login") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_EscapesErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		if _, err := w.Write([]byte("\x1b[31mboom\x1b[0m")); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	_, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "dispatch service returned status 502") {
		t.Fatalf("expected simplified status error, got %v", err)
	}
	if strings.Contains(err.Error(), testDispatchEndpoint) {
		t.Fatalf("did not expect endpoint path in user-facing error, got %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Quote("\x1b[31mboom\x1b[0m")) {
		t.Fatalf("expected quoted error body, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_IgnoresUnknownResponseFields(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		run := testDispatchRun("hi")
		run["unexpected"] = true
		writeDispatchRun(t, w, http.StatusAccepted, run)
	}))
	defer srv.Close()

	got, err := newTestCloudClient(t, srv.URL, "t").CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{testRepoSlug}}, "")
	if err != nil {
		t.Fatalf("expected forward-compatible decode, got error: %v", err)
	}
	if got.GeneratedMarkdown != "hi" {
		t.Fatalf("expected known fields to decode, got %q", got.GeneratedMarkdown)
	}
}

func TestCloudClient_CreateDispatch_RepoNotFoundNamesTargetJurisdiction(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"gateway wording":   `{"error":"Repository not found or not available in its region: entirehq/ferrata, entirehq/entire-plans"}`,
		"cell wording":      `{"error":"repository not found: entirehq/ferrata, entirehq/entire-plans"}`,
		"huma detail shape": `{"title":"Not Found","status":404,"detail":"repository not found: entirehq/ferrata, entirehq/entire-plans"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(body)) //nolint:errcheck // test fixture response
			}))
			defer srv.Close()

			client := newTestCloudClient(t, srv.URL, "t")
			// The request is forge-qualified; the gateway still echoes bare names.
			_, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{"gh/entirehq/ferrata", "gh/entirehq/entire-plans", "gh/entirehq/present"}}, "au")
			var notFound *RepoNotFoundError
			if !errors.As(err, &notFound) {
				t.Fatalf("expected *RepoNotFoundError, got %T: %v", err, err)
			}
			if notFound.Jurisdiction != "au" {
				t.Fatalf("expected jurisdiction au, got %q", notFound.Jurisdiction)
			}
			if len(notFound.Repos) != 2 || notFound.Repos[0] != "gh/entirehq/ferrata" || notFound.Repos[1] != "gh/entirehq/entire-plans" {
				t.Fatalf("unexpected repos: %v", notFound.Repos)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "In AU: repository not found: gh/entirehq/ferrata, gh/entirehq/entire-plans. Pick a jurisdiction") {
				t.Fatalf("expected our own jurisdiction-prefixed sentence regardless of gateway wording, got %q", msg)
			}
			if !strings.Contains(msg, "--jurisdiction <slug>") || !strings.Contains(msg, "mirror it there") {
				t.Fatalf("expected remediation hint, got %q", msg)
			}
			if !api.IsHTTPErrorStatus(err, http.StatusNotFound) {
				t.Fatalf("expected the *api.HTTPError to stay in the chain, got %v", err)
			}
		})
	}
}

func TestCloudClient_CreateDispatch_RepoNotFoundIgnoresGatewayProse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Repository not found in jurisdiction us: entirehq/ferrata"}`)) //nolint:errcheck // test fixture response
	}))
	defer srv.Close()

	client := newTestCloudClient(t, srv.URL, "t")
	_, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{"gh/entirehq/ferrata"}}, "us")
	if err == nil || !strings.HasPrefix(err.Error(), "In US: repository not found: gh/entirehq/ferrata. Pick a jurisdiction") {
		t.Fatalf("expected a single jurisdiction label, got %v", err)
	}
	// Unparseable message: fall back to the gateway's own sentence.
	unparsed := &RepoNotFoundError{Jurisdiction: "us", Message: "repository not found"}
	if !strings.HasPrefix(unparsed.Error(), "In US: repository not found. Pick") {
		t.Fatalf("expected message fallback, got %q", unparsed.Error())
	}
}

func TestCloudClient_CreateDispatch_RepoNotFoundAtHomeSaysSo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"repository not found: entirehq/ferrata"}`)) //nolint:errcheck // test fixture response
	}))
	defer srv.Close()

	client := newTestCloudClient(t, srv.URL, "t")
	_, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{"entirehq/ferrata"}}, "")
	if err == nil || !strings.HasPrefix(err.Error(), "In your home jurisdiction: repository not found: entirehq/ferrata.") {
		t.Fatalf("expected home-jurisdiction wording, got %v", err)
	}
}

func TestCloudClient_CreateDispatch_Other404StaysGeneric(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no checkpoints in window"}`)) //nolint:errcheck // test fixture response
	}))
	defer srv.Close()

	client := newTestCloudClient(t, srv.URL, "t")
	_, err := client.CreateDispatch(context.Background(), CreateDispatchRequest{Repos: []string{"x/y"}}, "")
	if err == nil || !strings.Contains(err.Error(), "dispatch service returned status 404") {
		t.Fatalf("expected the generic status error, got %v", err)
	}
	if !api.IsHTTPErrorStatus(err, http.StatusNotFound) {
		t.Fatalf("expected the shared *api.HTTPError underneath, got %v", err)
	}
	var notFound *RepoNotFoundError
	if errors.As(err, &notFound) {
		t.Fatalf("an empty-window 404 is not a repo-not-found error: %v", err)
	}
}

func TestParseNotFoundRepos(t *testing.T) {
	t.Parallel()

	requested := []string{"gh/A/B", "gh/c/d", "gh/e/f", "gh/g/h"}
	got := parseNotFoundRepos("Repository not found or not available in its region: gh/a/b, gh/c/d ,, gh/e/f, gh/a/b, gh/evil/injected", requested)
	if len(got) != 3 || got[0] != "gh/A/B" || got[1] != "gh/c/d" || got[2] != "gh/e/f" {
		t.Fatalf("expected only requested repos, in the request's spelling, got %v", got)
	}
	if got := parseNotFoundRepos("repository not found", requested); got != nil {
		t.Fatalf("expected nil for message without a repo list, got %v", got)
	}
	if got := parseNotFoundRepos("repository not found: check the repo is onboarded", requested); got != nil {
		t.Fatalf("prose after the colon must not become repo lookups, got %v", got)
	}
}

func TestParseNotFoundRepos_MatchesAcrossForgeSpellings(t *testing.T) {
	t.Parallel()

	requested := []string{"gh/a/b", "gh/c/d", "et/e/f", "et/g/h"}
	// A gateway may echo bare or prefixed; a bare echo is GitHub, never native.
	got := parseNotFoundRepos("repository not found: a/b, gh/C/D, et/e/f, g/h", requested)
	if len(got) != 3 || got[0] != "gh/a/b" || got[1] != "gh/c/d" || got[2] != "et/e/f" {
		t.Fatalf("expected forge-aware matches in the request's spelling, got %v", got)
	}

	// A request never leaves the CLI bare, so a bare request matches nothing.
	if got = parseNotFoundRepos("repository not found: gh/a/b, a/b", []string{"a/b"}); got != nil {
		t.Fatalf("expected a bare request to match no echo, got %v", got)
	}
}
