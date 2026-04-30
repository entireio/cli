package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stubCloudDispatchAuth swaps the package-level auth/secure-URL hooks to
// produce a test token and bypass the HTTPS guard so tests can talk to a
// plain-HTTP httptest.NewServer. Tests that need to verify the HTTPS guard
// itself should not call this helper.
func stubCloudDispatchAuth(t *testing.T) {
	t.Helper()
	oldLookup := lookupCurrentToken
	oldRequire := requireSecureDispatchURL
	lookupCurrentToken = func() (string, error) { return testCloudDispatchToken, nil }
	requireSecureDispatchURL = func(string) error { return nil }
	t.Cleanup(func() {
		lookupCurrentToken = oldLookup
		requireSecureDispatchURL = oldRequire
	})
}

func TestServerMode_HappyPath(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		repos, ok := body["repos"].([]any)
		if !ok || len(repos) != 1 || repos[0] != testRepoFullName {
			t.Fatalf("unexpected repos payload: %v", body)
		}
		if _, ok := body["repo"]; ok {
			t.Fatalf("did not expect repo payload: %v", body)
		}
		if body["until"] != "2026-04-15T18:30:00Z" {
			t.Fatalf("unexpected until payload: %v", body["until"])
		}
		if body["generate"] != true {
			t.Fatalf("expected generate=true payload, got %v", body["generate"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"window": map[string]any{
				"normalized_since": "2026-04-09T00:00:00Z",
				"normalized_until": "2026-04-16T00:00:00Z",
			},
			"covered_repos": []string{testRepoFullName},
			"repos":         []any{},
			"totals": map[string]any{
				"checkpoints":           0,
				"used_checkpoint_count": 0,
				"branches":              0,
				"files_touched":         0,
			},
			"warnings": map[string]any{
				"access_denied_count": 0,
				"pending_count":       0,
				"failed_count":        0,
				"unknown_count":       0,
				"uncategorized_count": 0,
			},
			"generated_markdown": testDispatchGeneratedHello,
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer mock.Close()

	stubCloudDispatchAuth(t)
	oldNow := nowUTC
	nowUTC = func() time.Time { return time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)
	t.Chdir(dir)

	got, err := Run(context.Background(), Options{
		Mode:     ModeServer,
		Since:    "7d",
		Until:    "2026-04-15T18:30:00Z",
		Branches: []string{"main"},
		Voice:    "neutral",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedText != testDispatchGeneratedHello {
		t.Fatalf("bad text: %q", got.GeneratedText)
	}
}

func TestServerMode_ExplicitReposDoNotRequireCurrentRepo(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		repos, ok := body["repos"].([]any)
		if !ok || len(repos) != 2 || repos[0] != testRepoFullName || repos[1] != "entireio/entire.io" {
			t.Fatalf("unexpected repos payload: %v", body)
		}
		if _, ok := body["repo"]; ok {
			t.Fatalf("did not expect repo payload: %v", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"window": map[string]any{
				"normalized_since": "2026-04-09T00:00:00Z",
				"normalized_until": "2026-04-16T00:00:00Z",
			},
			"covered_repos":      []string{testRepoFullName, "entireio/entire.io"},
			"repos":              []any{},
			"generated_markdown": testDispatchGeneratedHello,
			"totals": map[string]any{
				"checkpoints":           0,
				"used_checkpoint_count": 0,
				"branches":              0,
				"files_touched":         0,
			},
			"warnings": map[string]any{
				"access_denied_count": 0,
				"pending_count":       0,
				"failed_count":        0,
				"unknown_count":       0,
				"uncategorized_count": 0,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer mock.Close()

	stubCloudDispatchAuth(t)
	oldNow := nowUTC
	nowUTC = func() time.Time { return time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)

	got, err := Run(context.Background(), Options{
		Mode:      ModeServer,
		RepoPaths: []string{testRepoFullName, "entireio/entire.io"},
		Since:     "7d",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected dispatch result")
	}
	if len(got.CoveredRepos) != 2 {
		t.Fatalf("expected covered repos to propagate, got %v", got.CoveredRepos)
	}
}

func TestAPIToDispatch_DerivesRepoURLs(t *testing.T) {
	t.Parallel()

	got := apiToDispatch(&CreateDispatchResponse{
		Repos: []APIRepo{
			{FullName: testRepoFullName},
			{FullName: "bad/repo)"},
		},
	})
	if len(got.Repos) != 2 {
		t.Fatalf("expected two repos, got %+v", got.Repos)
	}
	if got.Repos[0].URL != testRepoURL {
		t.Fatalf("unexpected valid repo URL: %q", got.Repos[0].URL)
	}
	if got.Repos[1].URL != "" {
		t.Fatalf("expected unsafe repo URL to be omitted, got %q", got.Repos[1].URL)
	}
}

func TestServerMode_RequiresGeneratedMarkdown(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemote(t, dir)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"window": map[string]any{
				"normalized_since": "2026-04-09T00:00:00Z",
				"normalized_until": "2026-04-16T00:00:00Z",
			},
			"covered_repos": []string{testRepoFullName},
			"repos":         []any{},
			"totals": map[string]any{
				"checkpoints":           0,
				"used_checkpoint_count": 0,
				"branches":              0,
				"files_touched":         0,
			},
			"warnings": map[string]any{
				"access_denied_count": 0,
				"pending_count":       0,
				"failed_count":        0,
				"unknown_count":       0,
				"uncategorized_count": 0,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer mock.Close()

	stubCloudDispatchAuth(t)
	oldNow := nowUTC
	nowUTC = func() time.Time { return time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowUTC = oldNow })

	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)
	t.Chdir(dir)

	_, err := Run(context.Background(), Options{
		Mode:  ModeServer,
		Since: "7d",
	})
	if err == nil {
		t.Fatal("expected error when server response omits generated markdown")
	}
	if err.Error() != "dispatch generation returned no markdown" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServerMode_NormalizesWindowAndSanitizesVoice(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["since"] != "2026-04-09T00:00:00Z" {
			t.Fatalf("unexpected normalized since payload: %v", body["since"])
		}
		if body["until"] != "2026-04-16T00:01:00Z" {
			t.Fatalf("unexpected normalized until payload: %v", body["until"])
		}
		voice, ok := body["voice"].(string)
		if !ok {
			t.Fatalf("expected voice string payload, got %T", body["voice"])
		}
		if voice != "calm\nand steady" {
			t.Fatalf("unexpected sanitized voice payload: %q", voice)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"window": map[string]any{
				"normalized_since": "2026-04-09T00:00:00Z",
				"normalized_until": "2026-04-16T00:01:00Z",
			},
			"covered_repos":      []string{testRepoFullName},
			"repos":              []any{},
			"generated_markdown": testDispatchGeneratedHello,
			"totals": map[string]any{
				"checkpoints":           0,
				"used_checkpoint_count": 0,
				"branches":              0,
				"files_touched":         0,
			},
			"warnings": map[string]any{
				"access_denied_count": 0,
				"pending_count":       0,
				"failed_count":        0,
				"unknown_count":       0,
				"uncategorized_count": 0,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer mock.Close()

	stubCloudDispatchAuth(t)

	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)

	got, err := Run(context.Background(), Options{
		Mode:      ModeServer,
		RepoPaths: []string{testRepoFullName},
		Since:     "2026-04-09T00:00:29Z",
		Until:     "2026-04-16T00:00:31Z",
		Voice:     " calm\u0000\nand\u202E steady\u200B ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GeneratedText != testDispatchGeneratedHello {
		t.Fatalf("unexpected generated text: %q", got.GeneratedText)
	}
}

// TestServerMode_InsecureHTTPAuthBypassesSecureURLCheck confirms that setting
// Options.InsecureHTTPAuth=true skips the real requireSecureDispatchURL check
// and lets dispatch talk to an http:// base URL. This is the local-dev escape
// hatch that matches the --insecure-http-auth flag on login/trail. The test
// deliberately does not stub requireSecureDispatchURL — the flag alone must
// bypass the guard.
func TestServerMode_InsecureHTTPAuthBypassesSecureURLCheck(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"window": map[string]any{
				"normalized_since": "2026-04-09T00:00:00Z",
				"normalized_until": "2026-04-16T00:00:00Z",
			},
			"covered_repos":      []string{testRepoFullName},
			"repos":              []any{},
			"generated_markdown": testDispatchGeneratedHello,
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer mock.Close()

	oldLookup := lookupCurrentToken
	oldNow := nowUTC
	lookupCurrentToken = func() (string, error) { return testCloudDispatchToken, nil }
	nowUTC = func() time.Time { return time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		lookupCurrentToken = oldLookup
		nowUTC = oldNow
	})

	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)

	got, err := Run(context.Background(), Options{
		Mode:             ModeServer,
		RepoPaths:        []string{testRepoFullName},
		Since:            "7d",
		InsecureHTTPAuth: true,
	})
	if err != nil {
		t.Fatalf("expected insecure override to bypass the HTTPS guard, got %v", err)
	}
	if got.GeneratedText != testDispatchGeneratedHello {
		t.Fatalf("unexpected generated text: %q", got.GeneratedText)
	}
}

// TestServerMode_RejectsPlainHTTPBaseURL pins the production guarantee that
// bearer tokens are never sent to an http:// base URL. It deliberately does
// not call stubCloudDispatchAuth — the real requireSecureDispatchURL must
// fire. If a future refactor drops the check, this test breaks before the
// leak reaches users.
func TestServerMode_RejectsPlainHTTPBaseURL(t *testing.T) {
	oldLookup := lookupCurrentToken
	lookupCurrentToken = func() (string, error) { return testCloudDispatchToken, nil }
	t.Cleanup(func() { lookupCurrentToken = oldLookup })

	t.Setenv("ENTIRE_API_BASE_URL", "http://dispatch.example.invalid")

	_, err := Run(context.Background(), Options{
		Mode:      ModeServer,
		RepoPaths: []string{testRepoFullName},
		Since:     "7d",
	})
	if err == nil {
		t.Fatal("expected error when dispatch base URL is http://")
	}
	if !strings.Contains(err.Error(), api.ErrInsecureHTTP.Error()) {
		t.Fatalf("expected ErrInsecureHTTP, got %v", err)
	}
}
