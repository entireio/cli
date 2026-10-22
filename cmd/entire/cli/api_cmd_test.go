package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestInferFieldValue(t *testing.T) {
	t.Parallel()
	cases := map[string]any{
		"true":  true,
		"false": false,
		"null":  nil,
		"42":    int64(42),
		"-7":    int64(-7),
		"3.14":  3.14,
		"demo":  "demo",
		"":      "",
		"v1.2":  "v1.2",
	}
	for in, want := range cases {
		if got := inferFieldValue(in); got != want {
			t.Errorf("inferFieldValue(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestBuildAPIFields(t *testing.T) {
	t.Parallel()

	got, err := buildAPIFields([]string{"name=demo"}, []string{"count=3", "enabled=true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["name"] != "demo" || got["count"] != int64(3) || got["enabled"] != true {
		t.Fatalf("fields = %#v", got)
	}

	if f, err := buildAPIFields(nil, nil); len(f) != 0 || err != nil {
		t.Fatalf("no fields = (%v, %v), want (empty, nil)", f, err)
	}
	if _, err := buildAPIFields([]string{"bogus"}, nil); err == nil {
		t.Error("expected error for -f without '='")
	}
	if _, err := buildAPIFields(nil, []string{"=novalue"}); err == nil {
		t.Error("expected error for -F with empty key")
	}
}

func TestParseAPIHeaders(t *testing.T) {
	t.Parallel()

	h, err := parseAPIHeaders([]string{"Accept: application/json", "X-Foo:bar"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.Get("Accept") != "application/json" || h.Get("X-Foo") != "bar" {
		t.Fatalf("headers = %v", h)
	}
	if _, err := parseAPIHeaders([]string{"nocolon"}); err == nil {
		t.Error("expected error for header without ':'")
	}
}

func TestBuildAPIRequestBody_MethodInference(t *testing.T) {
	t.Parallel()

	// No fields, no method → GET, no body.
	r, err := buildAPIRequestBody("/p", &apiFlags{}, nil)
	if err != nil || r.method != http.MethodGet || r.body != nil {
		t.Fatalf("bare = %+v, err %v", r, err)
	}

	// Fields, no method → POST with JSON body.
	r, err = buildAPIRequestBody("/p", &apiFlags{}, map[string]any{"a": "b"})
	if err != nil || r.method != http.MethodPost || r.body == nil {
		t.Fatalf("fields = %+v, err %v", r, err)
	}

	// Explicit -X wins over inference.
	r, err = buildAPIRequestBody("/p", &apiFlags{method: "delete"}, nil)
	if err != nil || r.method != http.MethodDelete {
		t.Fatalf("explicit method = %+v, err %v", r, err)
	}

	// GET + fields → fields go on the query string, no body.
	r, err = buildAPIRequestBody("/p", &apiFlags{method: "GET"}, map[string]any{"limit": int64(5)})
	if err != nil || r.body != nil || !strings.Contains(r.path, "limit=5") {
		t.Fatalf("GET+fields = %+v, err %v", r, err)
	}

	// --input together with fields is rejected.
	if _, err := buildAPIRequestBody("/p", &apiFlags{input: "x"}, map[string]any{"a": "b"}); err == nil {
		t.Error("expected error combining --input and fields")
	}
}

func TestAppendQuery(t *testing.T) {
	t.Parallel()

	if got := appendQuery("/p", nil); got != "/p" {
		t.Errorf("empty query = %q", got)
	}
	q, err := fieldsToQuery(map[string]any{"a": "b"})
	if err != nil {
		t.Fatalf("fieldsToQuery: %v", err)
	}
	if got := appendQuery("/p", q); got != "/p?a=b" {
		t.Errorf("fresh query = %q, want /p?a=b", got)
	}
	if got := appendQuery("/p?x=1", q); got != "/p?x=1&a=b" {
		t.Errorf("existing query = %q, want /p?x=1&a=b", got)
	}
}

func TestValidateAPIPath(t *testing.T) {
	t.Parallel()

	// Origin-relative paths are allowed.
	for _, ok := range []string{"/api/v1/clusters", "/api/v1/me/recap?repo=01K", "api/v1/x", "/"} {
		if err := validateAPIPath(ok); err != nil {
			t.Errorf("validateAPIPath(%q) = %v, want nil", ok, err)
		}
	}
	// Absolute and scheme-relative URLs must be refused — they'd redirect the
	// bearer token to another host via url.ResolveReference.
	for _, bad := range []string{
		"https://evil.example/api/v1/x",
		"http://evil.example/x",
		"//evil.example/x",
		"https:/evil",
	} {
		if err := validateAPIPath(bad); err == nil {
			t.Errorf("validateAPIPath(%q) = nil, want rejection (token-leak vector)", bad)
		}
	}
}

func TestResolveAPIClient_UnknownTarget(t *testing.T) {
	t.Parallel()
	if _, err := resolveAPIClient(context.Background(), "banana", "", false); err == nil {
		t.Error("expected error for unknown --to")
	}
}

func TestResolveAPITarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		flags      apiFlags
		toExplicit bool
		wantTo     string
		wantJuris  string
		wantErr    bool
	}{
		// No --jurisdiction: --to is passed through untouched.
		{"default", apiFlags{to: apiTargetCore}, false, apiTargetCore, "", false},
		// --jurisdiction with default --to: implies cell, slug normalized to lowercase.
		{"implied cell", apiFlags{to: apiTargetCore, jurisdiction: " US "}, false, apiTargetCell, "us", false},
		// --jurisdiction with explicit --to cell: allowed.
		{"explicit cell", apiFlags{to: apiTargetCell, jurisdiction: "eu"}, true, apiTargetCell, "eu", false},
		// --jurisdiction with explicit --to core: contradiction, rejected.
		{"contradiction", apiFlags{to: apiTargetCore, jurisdiction: "eu"}, true, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			to, j, err := resolveAPITarget(&tc.flags, tc.toExplicit)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveAPITarget(%+v) = (%q, %q, nil), want error", tc.flags, to, j)
				}
				return
			}
			if err != nil || to != tc.wantTo || j != tc.wantJuris {
				t.Fatalf("resolveAPITarget(%+v) = (%q, %q, %v), want (%q, %q, nil)", tc.flags, to, j, err, tc.wantTo, tc.wantJuris)
			}
		})
	}
}

func TestWriteAPIResponse(t *testing.T) {
	t.Parallel()

	newResp := func(status int, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Proto:      "HTTP/2.0",
			Status:     http.StatusText(status),
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	write := func(status int, body string, include bool) (out, errOut bytes.Buffer, err error) {
		resp := newResp(status, body)
		defer func() { _ = resp.Body.Close() }()
		err = writeAPIResponse(&out, &errOut, resp, include)
		return out, errOut, err
	}

	// 2xx JSON is pretty-printed (indentation added) and returns no error.
	out, _, err := write(200, `{"a":1}`, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "\n  \"a\": 1") {
		t.Fatalf("expected indented JSON, got:\n%s", out.String())
	}

	// Non-2xx still prints the body but returns a (silent) error for a non-zero exit.
	out, _, err = write(404, `{"error":"nope"}`, false)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(out.String(), "nope") {
		t.Fatalf("expected body printed on 404, got:\n%s", out.String())
	}

	// --include writes the status line + headers to errOut.
	_, errOut, err := write(200, `{}`, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(errOut.String(), "HTTP/2.0") || !strings.Contains(errOut.String(), "Content-Type: application/json") {
		t.Fatalf("expected status+headers on errOut, got:\n%s", errOut.String())
	}
}

func TestReadWithinLimit(t *testing.T) {
	t.Parallel()

	// Under and exactly at the limit: full content, no error.
	for _, tc := range []struct {
		in    string
		limit int64
	}{{"hello", 10}, {"hello", 5}, {"", 3}} {
		got, err := readWithinLimit(strings.NewReader(tc.in), tc.limit)
		if err != nil || string(got) != tc.in {
			t.Errorf("readWithinLimit(%q, %d) = (%q, %v), want (%q, nil)", tc.in, tc.limit, got, err, tc.in)
		}
	}
	// Over the limit: error rather than silent truncation.
	if _, err := readWithinLimit(strings.NewReader("hello world"), 5); err == nil {
		t.Error("readWithinLimit over limit = nil error, want limit error (must not truncate)")
	}
}

func TestIsRepoScopedAPIPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"/api/v1/repos/{repo_id}/trails", true},
		{"/api/v1/repos/{repo_id}/trails?limit=1", true},
		{"/api/v1/me/recap?repo={repo_id}", false},
		{"/api/v1/me/{repo_id}", false},
		{"/api/v1/me", false},
		{"/api/v1/search?repo={repo_id}", false},
		{"/api/v1/metadata/{repo_id}", true},
		{"/api/v1/clusters", false},
	}
	for _, tt := range tests {
		if got := isRepoScopedAPIPath(tt.path); got != tt.want {
			t.Errorf("isRepoScopedAPIPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestExpandAPIPlaceholders(t *testing.T) {
	t.Parallel()
	origin := apiOrigin{forge: "gh", owner: "acme", repo: "widget"}
	got := expandAPIPlaceholders("/api/v1/{owner}/{repo}/x?repo={repo_id}", origin, "01REPO")
	if want := "/api/v1/acme/widget/x?repo=01REPO"; got != want {
		t.Errorf("expandAPIPlaceholders = %q, want %q", got, want)
	}
}

// apiBackendCall records what resolveAPIClientAndPath asked the backend seam for.
type apiBackendCall struct {
	called       bool
	to           string
	jurisdiction string
}

// setupAPIPlaceholderRepo chdirs into a fresh repo whose origin is originURL and
// stubs both client seams, so no test request reaches a real backend.
func setupAPIPlaceholderRepo(t *testing.T, originURL string) (backend *apiBackendCall, trailCalled *bool, placementClient *api.Client) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.AddRemote(t, dir, "origin", originURL)
	t.Chdir(dir)

	backend = &apiBackendCall{}
	prevBackend := newAPIBackendClient
	newAPIBackendClient = func(_ context.Context, to, jurisdiction string, _ bool) (*api.Client, error) {
		*backend = apiBackendCall{called: true, to: to, jurisdiction: jurisdiction}
		return api.NewClientWithBaseURL("token", "https://backend.example"), nil
	}
	called := false
	trailCalled = &called
	placementClient = api.NewClientWithBaseURL("token", "https://placement-cell.example")
	prevTrail := newTrailAPIClient
	newTrailAPIClient = func(_ context.Context, _ bool, forge, owner, repo string) (*api.Client, string, error) {
		called = true
		if forge != "gh" || owner != "entirehq" || repo != "entire.io" {
			t.Errorf("newTrailAPIClient(%q, %q, %q), want gh entirehq entire.io", forge, owner, repo)
		}
		return placementClient, "mirror-us", nil
	}
	t.Cleanup(func() {
		newAPIBackendClient = prevBackend
		newTrailAPIClient = prevTrail
	})
	return backend, trailCalled, placementClient
}

// Not parallel: chdirs and swaps package-level client seams.
func TestResolveAPIClientAndPath_RepoScopedCellRequestUsesPlacementCell(t *testing.T) {
	backend, trailCalled, placementClient := setupAPIPlaceholderRepo(t, "https://github.com/entirehq/entire.io.git")

	client, path, err := resolveAPIClientAndPath(t.Context(), apiTargetCell, "", false, "/api/v1/repos/{repo_id}/trails")
	if err != nil {
		t.Fatalf("resolveAPIClientAndPath: %v", err)
	}
	if !*trailCalled {
		t.Fatal("repo-scoped cell request did not resolve the repo's placement cell")
	}
	if backend.called {
		t.Errorf("repo-scoped cell request also built a home-cell client: %+v", backend)
	}
	if client != placementClient {
		t.Error("request did not use the placement cell's client")
	}
	if want := "/api/v1/repos/mirror-us/trails"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// Not parallel: chdirs and swaps package-level client seams.
func TestResolveAPIClientAndPath_ProcessingPlacementIDOffTheRepoCell(t *testing.T) {
	tests := []struct {
		name             string
		to, jurisdiction string
		path, wantPath   string
	}{
		{
			name: "me path stays on home cell", to: apiTargetCell,
			path: "/api/v1/me/recap?repo={repo_id}", wantPath: "/api/v1/me/recap?repo=mirror-us",
		},
		{
			name: "explicit jurisdiction wins", to: apiTargetCell, jurisdiction: "eu",
			path: "/api/v1/repos/{repo_id}/trails", wantPath: "/api/v1/repos/mirror-us/trails",
		},
		{
			name: "core", to: apiTargetCore,
			path: "/api/v1/repos/{repo_id}", wantPath: "/api/v1/repos/mirror-us",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, trailCalled, _ := setupAPIPlaceholderRepo(t, "https://github.com/entirehq/entire.io.git")
			// The processing placement is not the first one listed, which is the
			// one the old first-active-mirror lookup returned.
			withFakeCellCore(t, &fakeCellCore{
				repos:    reposOutput(fourRegionFixtureProcessingInMiddle()),
				clusters: clustersWithSlugs(),
			})

			_, path, err := resolveAPIClientAndPath(t.Context(), tt.to, tt.jurisdiction, false, tt.path)
			if err != nil {
				t.Fatalf("resolveAPIClientAndPath: %v", err)
			}
			if *trailCalled {
				t.Error("request was rerouted to the repo's placement cell")
			}
			if !backend.called || backend.to != tt.to || backend.jurisdiction != tt.jurisdiction {
				t.Errorf("backend = %+v, want to=%q jurisdiction=%q", backend, tt.to, tt.jurisdiction)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

// Not parallel: chdirs and swaps package-level client seams.
func TestResolveAPIClientAndPath_NoRepoIDSkipsPlacementLookup(t *testing.T) {
	backend, trailCalled, _ := setupAPIPlaceholderRepo(t, "https://github.com/entirehq/entire.io.git")
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("placement lookup must not run")})

	_, path, err := resolveAPIClientAndPath(t.Context(), apiTargetCell, "", false, "/api/v1/{owner}/{repo}/x")
	if err != nil {
		t.Fatalf("resolveAPIClientAndPath: %v", err)
	}
	if *trailCalled || !backend.called {
		t.Errorf("trail client called = %v, backend = %+v; want the home cell only", *trailCalled, backend)
	}
	if want := "/api/v1/entirehq/entire.io/x"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// Not parallel: chdirs and swaps package-level client seams.
func TestResolveAPIClientAndPath_PlacementLookupFailureIsAnError(t *testing.T) {
	_, trailCalled, _ := setupAPIPlaceholderRepo(t, "https://github.com/entirehq/entire.io.git")
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("control plane down")})

	_, _, err := resolveAPIClientAndPath(t.Context(), apiTargetCore, "", false, "/api/v1/repos/{repo_id}")
	if err == nil || !strings.Contains(err.Error(), "resolve {repo_id}") {
		t.Fatalf("err = %v, want a {repo_id} resolution error", err)
	}
	if *trailCalled {
		t.Error("core request resolved a cell client")
	}
}

// Not parallel: chdirs and swaps package-level client seams.
func TestResolveAPIClientAndPath_RepoIDNeedsSupportedForge(t *testing.T) {
	_, trailCalled, _ := setupAPIPlaceholderRepo(t, "https://gitlab.com/acme/widget.git")

	_, _, err := resolveAPIClientAndPath(t.Context(), apiTargetCell, "", false, "/api/v1/repos/{repo_id}/trails")
	if err == nil || !strings.Contains(err.Error(), "needs a GitHub or Entire repo") {
		t.Fatalf("err = %v, want the unsupported-forge message", err)
	}
	if *trailCalled {
		t.Error("unsupported forge reached the placement lookup")
	}
}

// Not parallel: chdirs, sets env, and swaps package-level client seams.
func TestResolveAPIClientAndPath_BaseURLOverrideKeepsItsHost(t *testing.T) {
	backend, trailCalled, _ := setupAPIPlaceholderRepo(t, "https://github.com/entirehq/entire.io.git")
	t.Setenv(api.BaseURLEnvVar, "http://127.0.0.1:8082")
	withFakeCellCore(t, &fakeCellCore{
		repos:    reposOutput(fourRegionFixtureProcessingInMiddle()),
		clusters: clustersWithSlugs(),
	})

	_, path, err := resolveAPIClientAndPath(t.Context(), apiTargetCell, "", false, "/api/v1/repos/{repo_id}/trails")
	if err != nil {
		t.Fatalf("resolveAPIClientAndPath: %v", err)
	}
	if *trailCalled {
		t.Error("request was rerouted past ENTIRE_API_BASE_URL to the placement cell")
	}
	if !backend.called || backend.to != apiTargetCell {
		t.Errorf("backend = %+v, want the --to cell client that honors the override", backend)
	}
	if want := "/api/v1/repos/mirror-us/trails"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}
