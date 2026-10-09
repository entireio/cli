package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// stubCloudDispatchAuth swaps the cell-client seam for a client aimed at
// ENTIRE_API_BASE_URL (read when Run builds the client, so tests may set it
// after stubbing) with the test token and "us" as the home jurisdiction. Tests
// that exercise the real cell routing should not call this helper.
func stubCloudDispatchAuth(t *testing.T) {
	t.Helper()
	old := newDispatchCellClient
	newDispatchCellClient = func(context.Context, bool, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL(testCloudDispatchToken, api.BaseURL()), "us", nil
	}
	t.Cleanup(func() { newDispatchCellClient = old })
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
		// The origin-derived default names its forge.
		repos, ok := body["repos"].([]any)
		if !ok || len(repos) != 1 || repos[0] != testRepoSlug {
			t.Fatalf("unexpected repos payload: %v", body)
		}
		if _, ok := body["repo"]; ok {
			t.Fatalf("did not expect repo payload: %v", body)
		}
		if body["until"] != "2026-04-15T18:30:00Z" {
			t.Fatalf("unexpected until payload: %v", body["until"])
		}
		if _, ok := body["generate"]; ok {
			t.Fatalf("did not expect generate payload: %v", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status": dispatchStatusComplete,
			"window": map[string]any{
				"normalizedSince": "2026-04-09T00:00:00Z",
				"normalizedUntil": "2026-04-16T00:00:00Z",
			},
			"coveredRepos":      []string{testRepoFullName},
			"repos":             []any{},
			"generatedMarkdown": testDispatchGeneratedHello,
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
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status": dispatchStatusComplete,
			"window": map[string]any{
				"normalizedSince": "2026-04-09T00:00:00Z",
				"normalizedUntil": "2026-04-16T00:00:00Z",
			},
			"coveredRepos":      []string{testRepoFullName, "entireio/entire.io"},
			"repos":             []any{},
			"generatedMarkdown": testDispatchGeneratedHello,
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

	got := apiToDispatch(&APIRun{
		Repos: []APIRepo{
			{FullName: testRepoFullName},
			{FullName: "bad/repo)"},
			{FullName: testRepoSlug},
			{FullName: "et/myproject/service"},
		},
	})
	if len(got.Repos) != 4 {
		t.Fatalf("expected four repos, got %+v", got.Repos)
	}
	if got.Repos[0].URL != testRepoURL {
		t.Fatalf("unexpected valid repo URL: %q", got.Repos[0].URL)
	}
	if got.Repos[1].URL != "" {
		t.Fatalf("expected unsafe repo URL to be omitted, got %q", got.Repos[1].URL)
	}
	if got.Repos[2].FullName != testRepoSlug || got.Repos[2].URL != testRepoURL {
		t.Fatalf("a gh/-prefixed echo keeps its name and links to github.com, got %+v", got.Repos[2])
	}
	if got.Repos[3].FullName != "et/myproject/service" || got.Repos[3].URL != "" {
		t.Fatalf("a native repo keeps its name and gets no github.com link, got %+v", got.Repos[3])
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
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status": dispatchStatusComplete,
			"window": map[string]any{
				"normalizedSince": "2026-04-09T00:00:00Z",
				"normalizedUntil": "2026-04-16T00:00:00Z",
			},
			"coveredRepos": []string{testRepoFullName},
			"repos":        []any{},
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
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status": dispatchStatusComplete,
			"window": map[string]any{
				"normalizedSince": "2026-04-09T00:00:00Z",
				"normalizedUntil": "2026-04-16T00:01:00Z",
			},
			"coveredRepos":      []string{testRepoFullName},
			"repos":             []any{},
			"generatedMarkdown": testDispatchGeneratedHello,
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

// TestServerMode_NativeOriginNamesItsForge pins the fix for `entire dispatch`
// in an Entire-native checkout: with no --repos, the origin remote
// entire://<cell>/et/<project>/<repo> must become the et/ slug the server
// already accepts, not an error claiming dispatch supports GitHub only.
func TestServerMode_NativeOriginNamesItsForge(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemoteURL(t, dir, "entire://aws-us-east-2.entire.io/et/entirehq/entire-api")

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
		if !ok || len(repos) != 1 || repos[0] != "et/entirehq/entire-api" {
			t.Fatalf("expected the native origin to dispatch as its et/ slug, got repos payload: %v", body["repos"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"status":            dispatchStatusComplete,
			"window":            map[string]any{"normalizedSince": "2026-04-09T00:00:00Z", "normalizedUntil": "2026-04-16T00:00:00Z"},
			"coveredRepos":      []string{"entirehq/entire-api"},
			"repos":             []any{},
			"generatedMarkdown": testDispatchGeneratedHello,
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer mock.Close()

	stubCloudDispatchAuth(t)
	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)
	t.Chdir(dir)

	got, err := Run(context.Background(), Options{Mode: ModeServer, Since: "7d"})
	if err != nil {
		t.Fatalf("native checkout should dispatch via its et/ slug, got %v", err)
	}
	if got.GeneratedText != testDispatchGeneratedHello {
		t.Fatalf("unexpected generated text: %q", got.GeneratedText)
	}
}

// TestServerMode_UnknownOriginHostIsAnError: an origin on a forge Entire does
// not host is still refused, and the error names both shapes --repos takes so
// the user can address the repo explicitly.
func TestServerMode_UnknownOriginHostIsAnError(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "a.txt", "x")
	testutil.GitAdd(t, dir, "a.txt")
	testutil.GitCommit(t, dir, "initial")
	addOriginRemoteURL(t, dir, "https://gitlab.com/acme/thing.git")

	stubCloudDispatchAuth(t)
	t.Setenv("ENTIRE_API_BASE_URL", "http://127.0.0.1:9") // must never be dialed
	t.Chdir(dir)

	_, err := Run(context.Background(), Options{Mode: ModeServer, Since: "7d"})
	if err == nil || !strings.Contains(err.Error(), "gitlab.com") || !strings.Contains(err.Error(), "--repos") {
		t.Fatalf("expected an error naming the host and the --repos escape hatch, got %v", err)
	}
}

// TestServerMode_RoutesToTheRequestedCell: --jurisdiction picks the cell the
// client dials, and --insecure-http-auth reaches the cell client; neither
// travels as a query or body field.
func TestServerMode_RoutesToTheRequestedCell(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDispatchEndpoint {
			http.NotFound(w, r)
			return
		}
		if r.URL.RawQuery != "" {
			t.Errorf("did not expect a query, got %q", r.URL.RawQuery)
		}
		writeDispatchRun(t, w, http.StatusAccepted, testDispatchRun(testDispatchGeneratedHello))
	}))
	defer mock.Close()

	var gotJurisdiction string
	var gotInsecure bool
	old := newDispatchCellClient
	newDispatchCellClient = func(_ context.Context, insecure bool, jurisdiction string) (*api.Client, string, error) {
		gotJurisdiction, gotInsecure = jurisdiction, insecure
		return api.NewClientWithBaseURL(testCloudDispatchToken, mock.URL), "us", nil
	}
	t.Cleanup(func() { newDispatchCellClient = old })

	got, err := Run(context.Background(), Options{
		Mode:             ModeServer,
		RepoPaths:        []string{testRepoSlug},
		Since:            "7d",
		Jurisdiction:     "eu",
		InsecureHTTPAuth: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotJurisdiction != "eu" || !gotInsecure {
		t.Fatalf("expected the eu cell with insecure auth, got jurisdiction %q insecure %v", gotJurisdiction, gotInsecure)
	}
	if got.GeneratedText != testDispatchGeneratedHello {
		t.Fatalf("bad text: %q", got.GeneratedText)
	}
}

// TestServerMode_RepoNotFoundAtHomeNamesHome: with no --jurisdiction the
// home cell answered, so the error carries the home jurisdiction.
func TestServerMode_RepoNotFoundAtHomeNamesHome(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"repository not found: gh/entireio/cli"}`)) //nolint:errcheck // test fixture response
	}))
	defer mock.Close()

	stubCloudDispatchAuth(t)
	t.Setenv("ENTIRE_API_BASE_URL", mock.URL)

	_, err := Run(context.Background(), Options{Mode: ModeServer, RepoPaths: []string{testRepoSlug}, Since: "7d"})
	var notFound *RepoNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("expected *RepoNotFoundError, got %T: %v", err, err)
	}
	if notFound.FailedJurisdiction() != "us" {
		t.Fatalf("expected the home jurisdiction us, got %q", notFound.FailedJurisdiction())
	}
}

// TestServerMode_RejectsPlainHTTPBaseURL pins that the bearer is never sent to
// an http:// data host. It runs the real cell routing (no stub): with no
// ENTIRE_TOKEN and an http override, the cell client refuses before dialing.
func TestServerMode_RejectsPlainHTTPBaseURL(t *testing.T) {
	t.Setenv(auth.EnvTokenVar, "")
	if err := os.Unsetenv(auth.EnvTokenVar); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_API_BASE_URL", "http://dispatch.example.invalid")

	_, err := Run(context.Background(), Options{
		Mode:      ModeServer,
		RepoPaths: []string{testRepoSlug},
		Since:     "7d",
	})
	if err == nil {
		t.Fatal("expected error when dispatch base URL is http://")
	}
	if !strings.Contains(err.Error(), api.ErrInsecureHTTP.Error()) {
		t.Fatalf("expected ErrInsecureHTTP, got %v", err)
	}
}
