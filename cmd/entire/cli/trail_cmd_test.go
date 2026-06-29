package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"
)

const (
	trailListTestAuthorAlice = "alice"
	trailListTestAuthorBob   = "bob"
)

func TestNewTrailCreateRequestUsesLinkBranchAction(t *testing.T) {
	req := newTrailCreateRequest("title", "body", "feature/x", "main", "open")

	require.Equal(t, api.TrailCreateRequest{
		Title:        "title",
		Body:         "body",
		BranchName:   "feature/x",
		BranchAction: "link",
		Base:         "main",
		Status:       "open",
	}, req)
}

func TestNewTrailCreateRequestCanBeBranchless(t *testing.T) {
	req := newTrailCreateRequest("title", "body", "", "main", "open")

	require.Equal(t, api.TrailCreateRequest{
		Title:  "title",
		Body:   "body",
		Base:   "main",
		Status: "open",
	}, req)

	encoded, err := json.Marshal(req)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "branch_name")
	require.NotContains(t, string(encoded), "branch_action")
}

func TestPrepareTrailCreateBranchSkipsBranchlessTrail(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		branch   string
		noBranch bool
	}{
		{name: "explicit no-branch", branch: "", noBranch: true},
		{name: "empty branch defensive guard", branch: "", noBranch: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state, err := prepareTrailCreateBranch(io.Discard, io.Discard, nil, tc.branch, "main", tc.noBranch)

			require.NoError(t, err)
			require.False(t, state.NeedsCreation)
			require.False(t, state.LocalCreated)
			require.False(t, state.RemotePushed)
		})
	}
}

func TestValidateTrailCreateFlagCombosRejectsBranchlessConflicts(t *testing.T) {
	t.Parallel()

	t.Run("branch", func(t *testing.T) {
		t.Parallel()
		cmd := newTrailCreateCmd()
		require.NoError(t, cmd.Flags().Set("branch", "feature/x"))

		err := validateTrailCreateFlagCombos(cmd, false, true)

		require.EqualError(t, err, "cannot combine --no-branch with --branch")
	})

	t.Run("checkout", func(t *testing.T) {
		t.Parallel()
		cmd := newTrailCreateCmd()

		err := validateTrailCreateFlagCombos(cmd, true, true)

		require.EqualError(t, err, "cannot combine --no-branch with --checkout")
	})
}

func TestTrailCreateCommandRejectsBranchlessFlagConflictsBeforeRepoLookup(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "branch",
			args:    []string{"--no-branch", "--branch", "feature/x", "--title", "Branchless"},
			wantErr: "cannot combine --no-branch with --branch",
		},
		{
			name:    "checkout",
			args:    []string{"--no-branch", "--checkout", "--title", "Branchless"},
			wantErr: "cannot combine --no-branch with --checkout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTrailCreateCmd()
			cmd.SetContext(context.Background())
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()

			require.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestResolveTrailCreateFieldsBranchlessNonInteractiveClearsBranchAndDefaultsStatus(t *testing.T) {
	t.Parallel()

	cmd := newTrailCreateCmd()
	require.NoError(t, cmd.Flags().Set("title", "  Branchless trail  "))

	title, body, base, branch, status, err := resolveTrailCreateFields(cmd, io.Discard, "  Branchless trail  ", "body", " main ", "", "", "feature/current", true)

	require.NoError(t, err)
	require.Equal(t, "Branchless trail", title)
	require.Equal(t, "body", body)
	require.Equal(t, "main", base)
	require.Empty(t, branch)
	require.Equal(t, string(trail.StatusOpen), status)
}

func TestValidateTrailCreateFieldsAllowsBranchlessEmptyBranch(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateTrailCreateFields(context.Background(), "Branchless", "", string(trail.StatusOpen), true))
	require.EqualError(t,
		validateTrailCreateFields(context.Background(), "Branch backed", "", string(trail.StatusOpen), false),
		"branch name is required")
}

func TestRunTrailCreateInteractiveBranchlessSkipsBranchPrompt(t *testing.T) {
	// No t.Parallel: runTrailCreateForm is package-global test seam.
	previous := runTrailCreateForm
	calls := 0
	runTrailCreateForm = func(*huh.Form) error {
		calls++
		return nil
	}
	t.Cleanup(func() { runTrailCreateForm = previous })

	title := "  Branchless trail  "
	body := "body"
	branch := "must-be-cleared"
	status := ""

	err := runTrailCreateInteractive(&title, &body, &branch, &status, true)

	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, "Branchless trail", title)
	require.Empty(t, branch)
	require.Equal(t, string(trail.StatusOpen), status)
}

func TestRunTrailCreateBranchlessHappyPath(t *testing.T) {
	// No t.Parallel: uses t.Chdir plus auth/tokenstore package-level test seams.
	var gotCreate map[string]any
	var gotCreateAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"exchanged-token","token_type":"Bearer","expires_in":3600}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/trails/gh/acme/repo":
			gotCreateAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotCreate); err != nil {
				t.Errorf("decode create request: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.TrailCreateResponse{
				Trail: api.TrailResource{ID: "trl_branchless", Title: "Branchless full path"},
			}); err != nil {
				t.Errorf("encode create response: %v", err)
			}
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	t.Setenv(api.BaseURLEnvVar, srv.URL)
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	service := tokenstore.CoreKeyringService(srv.URL)
	jwt := makeContextJWT(t, fmt.Sprintf(`{"iss":%q,"handle":"me","exp":%d}`, srv.URL, time.Now().Add(2*time.Hour).Unix()))
	require.NoError(t, tokenstore.Set(service, "me", tokenstore.EncodeTokenWithExpiration(jwt, 7200)))
	ctxObj := &contexts.Context{Name: "me@core", CoreURL: srv.URL, Handle: "me", KeychainService: service}
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			return ctxObj, nil
		}))

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	runGitTrailTest(t, repoDir, "remote", "add", "origin", "https://github.com/acme/repo.git")
	t.Chdir(repoDir)

	cmd := newTrailCreateCmd()
	cmd.SetContext(context.Background())
	cmd.Flags().Bool("insecure-http-auth", true, "")
	require.NoError(t, cmd.Flags().Set("insecure-http-auth", "true"))
	cmd.SetArgs([]string{"--title", "Branchless full path", "--body", "body", "--base", "main", "--no-branch"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	err := cmd.Execute()

	require.NoError(t, err)
	require.Contains(t, gotCreateAuth, "Bearer ")
	require.Equal(t, "Branchless full path", gotCreate["title"])
	require.Equal(t, "body", gotCreate["body"])
	require.Equal(t, "main", gotCreate["base"])
	require.Equal(t, string(trail.StatusOpen), gotCreate["status"])
	require.NotContains(t, gotCreate, "branch_name")
	require.NotContains(t, gotCreate, "branch_action")
	require.Contains(t, out.String(), `Created trail "Branchless full path" (ID: trl_branchless)`)
	require.NotContains(t, out.String(), "Pushed branch")
	require.Empty(t, errOut.String())
}

func TestCleanupCreatedTrailBranch(t *testing.T) {
	cases := []struct {
		name             string
		localCreated     bool
		remotePushed     bool
		checkoutBranch   bool
		wantLocalBranch  bool
		wantRemoteBranch bool
	}{
		{
			name:             "removes local branch only",
			localCreated:     true,
			remotePushed:     false,
			wantLocalBranch:  false,
			wantRemoteBranch: false,
		},
		{
			name:             "removes local and pushed remote branch",
			localCreated:     true,
			remotePushed:     true,
			wantLocalBranch:  false,
			wantRemoteBranch: false,
		},
		{
			name:             "does not delete remote when checked out branch cannot be removed locally",
			localCreated:     true,
			remotePushed:     true,
			checkoutBranch:   true,
			wantLocalBranch:  true,
			wantRemoteBranch: true,
		},
		{
			name:             "deletes remote when local was not created by cleanup owner",
			localCreated:     false,
			remotePushed:     true,
			wantLocalBranch:  true,
			wantRemoteBranch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			branch := "cleanup-test"
			localDir, originDir, repo := initTrailCleanupRepo(t)
			defer repo.Close()
			t.Chdir(localDir)

			runGitTrailTest(t, localDir, "branch", branch)
			if tc.remotePushed {
				runGitTrailTest(t, localDir, "push", "origin", branch)
			}
			if tc.checkoutBranch {
				runGitTrailTest(t, localDir, "checkout", branch)
			}

			var errBuf bytes.Buffer
			cleanupCreatedTrailBranch(repo, branch, tc.localCreated, tc.remotePushed, &errBuf)

			require.Equal(t, tc.wantLocalBranch, gitBranchExistsTrailTest(t, localDir, branch), "local branch mismatch; stderr: %s", errBuf.String())
			require.Equal(t, tc.wantRemoteBranch, gitBranchExistsTrailTest(t, originDir, branch), "remote branch mismatch; stderr: %s", errBuf.String())
			if tc.checkoutBranch {
				require.Contains(t, errBuf.String(), "not deleting remote branch")
			}
		})
	}
}

func initTrailCleanupRepo(t *testing.T) (localDir, originDir string, repo *git.Repository) {
	t.Helper()

	tmp := t.TempDir()
	localDir = filepath.Join(tmp, "local")
	originDir = filepath.Join(tmp, "origin.git")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	runGitTrailTest(t, tmp, "init", "--bare", originDir)
	repo = initOpenedTestRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "test\n")
	runGitTrailTest(t, localDir, "add", "README.md")
	runGitTrailTest(t, localDir, "commit", "-m", "initial")
	runGitTrailTest(t, localDir, "remote", "add", "origin", originDir)
	return localDir, originDir, repo
}

func runGitTrailTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
}

func gitBranchExistsTrailTest(t *testing.T, repoDir, branch string) bool {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repoDir
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 1, exitErr.ExitCode())
	return false
}

func TestRunTrailListAll_PrintsLoginHintWhenNotLoggedIn(t *testing.T) {
	// No t.Parallel: SetResolveContextForAPIForTest and
	// tokenstore.UseFileBackendForTesting mutate package-level state.
	//
	// Discovery selects a context whose keyring slot holds nothing, so the
	// per-context provider reports ErrNotLoggedIn.
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	c := &contexts.Context{Name: "me@core", CoreURL: "https://core.example", Handle: "me", KeychainService: "kc:me"}
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			return c, nil
		}))

	var out, errOut bytes.Buffer
	err := runTrailListAll(t.Context(), &out, &errOut, defaultTrailListOptions(false))
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
	if !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Errorf("error chain missing ErrNotLoggedIn: %v", err)
	}
	var silent *SilentError
	if !errors.As(err, &silent) {
		t.Errorf("error = %v, want SilentError wrap", err)
	}
	if strings.Contains(out.String(), "No trails found") {
		t.Errorf("stdout = %q, must not render logged-out state as an empty trail list", out.String())
	}
	wantHint := "Not logged in. Run 'entire login' to authenticate."
	if got := errOut.String(); !strings.Contains(got, wantHint) {
		t.Errorf("errOut = %q, want hint %q", got, wantHint)
	}
}

func TestRunTrailListAll_ValidatesOptionsBeforeAuth(t *testing.T) {
	// No t.Parallel: SetResolveContextForAPIForTest mutates package-level
	// auth state.
	//
	// Discovery must never run for invalid local options: validation has to
	// short-circuit before any auth resolution.
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			t.Fatal("discovery should not run for invalid local options")
			return nil, errors.New("unreachable")
		}))

	opts := defaultTrailListOptions(false)
	opts.Limit = 0

	var out, errOut bytes.Buffer
	err := runTrailListAll(t.Context(), &out, &errOut, opts)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("got auth error %v, want local validation error", err)
	}
	if got, want := err.Error(), "limit must be greater than 0"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Fatalf("errOut = %q, want no auth hint", errOut.String())
	}
}

func TestRunTrailListAllWithClient_ValidatesOptionsBeforeRepoLookup(t *testing.T) {
	t.Parallel()

	opts := defaultTrailListOptions(false)
	opts.Limit = 0

	var out bytes.Buffer
	err := runTrailListAllValidatedWithClient(t.Context(), &out, nil, opts)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if got, want := err.Error(), "limit must be greater than 0"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestTrailRootPrintsHelp(t *testing.T) {
	t.Parallel()
	cmd := newTrailCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute trail root: %v", err)
	}
	text := out.String()
	for _, want := range []string{"A trail ties together the context for a branch", "show", "list", "create"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help output missing %q, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Not logged in") {
		t.Fatalf("trail root should not perform auth/API work, got:\n%s", text)
	}
}

func TestTrailsBasePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		forge, owner, rp string
		want             string
	}{
		{"gh forge", "gh", "acme", "repo", "/api/v1/trails/gh/acme/repo"},
		{"et forge", "et", "acme", "repo", "/api/v1/trails/et/acme/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := trailsBasePath(tt.forge, tt.owner, tt.rp)
			if got != tt.want {
				t.Fatalf("trailsBasePath(%q, %q, %q) = %q, want %q", tt.forge, tt.owner, tt.rp, got, tt.want)
			}
		})
	}
}

func TestTrailNumberPath(t *testing.T) {
	t.Parallel()
	got := trailNumberPath("gh", "acme", "repo", 575)
	want := "/api/v1/trails/gh/acme/repo/575"
	if got != want {
		t.Fatalf("trailNumberPath = %q, want %q", got, want)
	}
	// Regression guard: the single-trail endpoint is keyed by the integer trail
	// number, never the UUID id — the server's parseTrailNumber rejects a UUID
	// (it starts with a non-[1-9] char), which previously surfaced as a 400.
	if strings.Contains(got, "-") {
		t.Fatalf("trailNumberPath must use the integer number, got %q", got)
	}
}

func TestTrailWebURL(t *testing.T) {
	t.Parallel()
	want := "https://entire.io/gh/acme/repo/trails/575"
	if got := trailWebURL("https://entire.io", "gh", "acme", "repo", 575); got != want {
		t.Fatalf("trailWebURL = %q, want %q", got, want)
	}
	// A trailing slash on the base must not double up.
	if got := trailWebURL("https://entire.io/", "gh", "acme", "repo", 575); got != want {
		t.Fatalf("trailWebURL(trailing slash) = %q, want %q", got, want)
	}
}

func TestPrintCreatedTrail(t *testing.T) {
	t.Parallel()

	// The server-provided URL is used verbatim.
	var out bytes.Buffer
	printCreatedTrail(&out, api.TrailResource{Title: "Fix it", Branch: "feat/x", ID: "abc123", Number: 575, URL: "https://entire.io/gh/acme/repo/trails/575/fix-it"}, "gh", "acme", "repo")
	text := out.String()
	if !strings.Contains(text, `Created trail "Fix it" for branch feat/x (ID: abc123)`) {
		t.Fatalf("missing create summary line, got:\n%s", text)
	}
	if !strings.Contains(text, "URL: https://entire.io/gh/acme/repo/trails/575/fix-it") {
		t.Fatalf("expected the server-provided URL, got:\n%s", text)
	}

	// Without a number, omit the URL line.
	out.Reset()
	printCreatedTrail(&out, api.TrailResource{Title: "No num", Branch: "feat/y", ID: "def456"}, "gh", "acme", "repo")
	if text := out.String(); strings.Contains(text, "URL:") {
		t.Fatalf("expected URL omitted when number and URL are absent, got:\n%s", text)
	}
}

func TestTrailDisplayURL(t *testing.T) {
	t.Parallel()

	// Server URL wins, even when a number is present.
	got := trailDisplayURL(api.TrailResource{Number: 5, URL: "https://server/url"}, "gh", "acme", "repo")
	if got != "https://server/url" {
		t.Fatalf("expected server URL, got %q", got)
	}

	// Falls back to a constructed URL for older servers that omit it.
	got = trailDisplayURL(api.TrailResource{Number: 5}, "gh", "acme", "repo")
	if !strings.HasSuffix(got, "/gh/acme/repo/trails/5") {
		t.Fatalf("expected constructed fallback URL, got %q", got)
	}

	// Nothing to show when neither is available.
	if got := trailDisplayURL(api.TrailResource{}, "gh", "acme", "repo"); got != "" {
		t.Fatalf("expected empty URL, got %q", got)
	}
}

func TestTrailDescriptionForDisplay(t *testing.T) {
	t.Parallel()
	if got := trailDescriptionForDisplay("the body", true); got != "the body" {
		t.Fatalf("non-empty body: got %q, want %q", got, "the body")
	}
	if got := trailDescriptionForDisplay("the body", false); got != "the body" {
		t.Fatalf("non-empty body (not loaded): got %q, want %q", got, "the body")
	}
	// Loaded but empty/whitespace → explicit placeholder.
	if got := trailDescriptionForDisplay("", true); got != noTrailDescription {
		t.Fatalf("loaded+empty: got %q, want %q", got, noTrailDescription)
	}
	if got := trailDescriptionForDisplay("   ", true); got != noTrailDescription {
		t.Fatalf("loaded+whitespace: got %q, want %q", got, noTrailDescription)
	}
	// Not loaded (fetch failed) → nothing (the caller already warned).
	if got := trailDescriptionForDisplay("", false); got != "" {
		t.Fatalf("not loaded+empty: got %q, want empty", got)
	}
}

func TestFetchTrailDescription_ReadsNestedBodyDocument(t *testing.T) {
	t.Parallel()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// Regression guard: body_document is nested under `trail`, and
		// `checkpoints` is a bare array the decode must ignore.
		if _, err := io.WriteString(w, `{"trail":{"number":777,"branch":"feat/x","body_document":{"text_snapshot":"the intent text"}},"checkpoints":[],"has_write_permission":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	bodyText, err := fetchTrailDescription(t.Context(), client, "gh", "acme", "repo", 777)
	if err != nil {
		t.Fatalf("fetchTrailDescription: %v", err)
	}
	if want := "/api/v1/trails/gh/acme/repo/777"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if bodyText != "the intent text" {
		t.Fatalf("bodyText = %q, want %q", bodyText, "the intent text")
	}
}

func TestResolveCreateBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		branchFlag    string
		currentBranch string
		base          string
		title         string
		titleProvided bool
		want          string
	}{
		{"explicit --branch always wins", "feat/x", "main", "main", "My Title", true, "feat/x"},
		{"feature branch uses current, not title slug", "", "alex/authz-read", "main", "Shared authz read client", true, "alex/authz-read"},
		{"on base (main) slugs the title", "", "main", "main", "Add Auth System", true, "add-auth-system"},
		{"non-standard default (develop==base) slugs the title", "", "develop", "develop", "Add Auth System", true, "add-auth-system"},
		{"feature branch, no title, uses current", "", "alex/authz-read", "main", "", false, "alex/authz-read"},
		{"on base, no title, falls back to current", "", "main", "main", "", false, "main"},
		{"detached HEAD with title slugs the title", "", "", "main", "Add Auth System", true, "add-auth-system"},
		{"detached HEAD, no title, returns empty (caller errors)", "", "", "main", "", false, ""},
		{"unsluggable title yields empty (caller errors)", "", "main", "main", "!!!", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveCreateBranch(tt.branchFlag, tt.currentBranch, tt.base, tt.title, tt.titleProvided)
			if got != tt.want {
				t.Fatalf("resolveCreateBranch(%q, %q, %q, %q, %v) = %q, want %q",
					tt.branchFlag, tt.currentBranch, tt.base, tt.title, tt.titleProvided, got, tt.want)
			}
		})
	}
}

func TestParseTrailNumberArg(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{"no arg", nil, 0, false},
		{"empty slice", []string{}, 0, false},
		{"valid number", []string{"575"}, 575, false},
		{"zero rejected", []string{"0"}, 0, true},
		{"negative rejected", []string{"-3"}, 0, true},
		{"non-numeric rejected", []string{"abc"}, 0, true},
		{"uuid rejected", []string{"019ed3c9-7fd9-72d6-bd29-1130d2b2eec4"}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTrailNumberArg(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTrailNumberArg(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("parseTrailNumberArg(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestConfirmTrailDeletion(t *testing.T) {
	t.Parallel()

	// --force proceeds without prompting (no TTY needed).
	var buf bytes.Buffer
	proceed, err := confirmTrailDeletion(t.Context(), &buf, 575, "Some title", true, false)
	if err != nil || !proceed {
		t.Fatalf("force: got (proceed=%v, err=%v), want (true, nil)", proceed, err)
	}

	// Non-interactive without --force must refuse, not delete unprompted.
	buf.Reset()
	proceed, err = confirmTrailDeletion(t.Context(), &buf, 575, "Some title", false, false)
	if err == nil {
		t.Fatalf("non-interactive without --force: expected error, got nil (proceed=%v)", proceed)
	}
	if proceed {
		t.Fatal("non-interactive without --force: must not proceed")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error should mention --force, got: %v", err)
	}

	// An already-cancelled context is a clean cancel: no prompt, no error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buf.Reset()
	proceed, err = confirmTrailDeletion(ctx, &buf, 575, "Some title", false, true)
	if err != nil || proceed {
		t.Fatalf("cancelled ctx: got (proceed=%v, err=%v), want (false, nil)", proceed, err)
	}
}

func TestDeleteTrailByNumber(t *testing.T) {
	t.Parallel()

	t.Run("deletes via the integer number path and accepts ok:true", func(t *testing.T) {
		t.Parallel()
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			if err := json.NewEncoder(w).Encode(api.TrailDeleteResponse{OK: true}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		if err := deleteTrailByNumber(t.Context(), client, "gh", "acme", "repo", 575); err != nil {
			t.Fatalf("deleteTrailByNumber: %v", err)
		}
		if gotMethod != http.MethodDelete {
			t.Fatalf("method = %q, want DELETE", gotMethod)
		}
		if want := "/api/v1/trails/gh/acme/repo/575"; gotPath != want {
			t.Fatalf("path = %q, want %q (integer number, not UUID)", gotPath, want)
		}
	})

	t.Run("treats a 2xx without ok:true as failure", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if err := json.NewEncoder(w).Encode(api.TrailDeleteResponse{OK: false}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		if err := deleteTrailByNumber(t.Context(), client, "gh", "acme", "repo", 575); err == nil {
			t.Fatal("expected error for 2xx without ok:true, got nil")
		}
	})

	t.Run("surfaces a non-2xx status", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": "Trail not found"}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		if err := deleteTrailByNumber(t.Context(), client, "gh", "acme", "repo", 999); err == nil {
			t.Fatal("expected error for 404, got nil")
		}
	})
}

// Not parallel: uses t.Chdir() to point ResolveRemoteRepo at a fake repo.
func TestResolveTrailRemote_RejectsUnsupportedForge(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@gitlab.com:acme/my-app.git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	t.Chdir(repoDir)

	_, _, _, err := resolveTrailRemote(context.Background())
	if err == nil {
		t.Fatal("expected error for gitlab.com origin, got nil")
	}
	if !strings.Contains(err.Error(), "not on a forge supported by Entire trails") {
		t.Fatalf("error message does not mention unsupported forge: %v", err)
	}
}

// TestTrailsEnabledForRepo_ReadsClonePreference verifies the prompt-path gate
// is a local clone-preference read only. The API enablement decision itself
// (2xx => enabled) is covered by api.TestClient_TrailsEnabled.
//
// Not parallel: uses t.Chdir() to point clone preferences at a fake repo.
func TestTrailsEnabledForRepo_ReadsClonePreference(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@github.com:acme/repo.git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	t.Chdir(repoDir)
	ctx := context.Background()

	if trailsEnabledForRepo(ctx) {
		t.Fatal("expected trails disabled when cache is absent")
	}
	if err := saveTrailsEnabledForRepo(ctx, false); err != nil {
		t.Fatalf("save false cache: %v", err)
	}
	if trailsEnabledForRepo(ctx) {
		t.Fatal("expected trails disabled when cache is false")
	}
	if err := saveTrailsEnabledForRepo(ctx, true); err != nil {
		t.Fatalf("save true cache: %v", err)
	}
	if !trailsEnabledForRepo(ctx) {
		t.Fatal("expected trails enabled when cache is true")
	}

	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		t.Fatalf("load prefs: %v", err)
	}
	if prefs.TrailsEnabledRepoKey != "gh/acme/repo" {
		t.Fatalf("repo key = %q, want gh/acme/repo", prefs.TrailsEnabledRepoKey)
	}

	currentAuthKey := prefs.TrailsEnabledAuthKey
	prefs.TrailsEnabledAuthKey = currentAuthKey + "-other"
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		t.Fatalf("save auth-mismatched prefs: %v", err)
	}
	if trailsEnabledForRepo(ctx) {
		t.Fatal("expected trails disabled for mismatched auth cache scope")
	}
	prefs.TrailsEnabledAuthKey = currentAuthKey
	fresh := time.Now()
	prefs.TrailsEnabledCheckedAt = &fresh
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		t.Fatalf("restore auth-matched prefs: %v", err)
	}

	stale := time.Now().Add(-trailEnablementCacheTTL - time.Minute)
	prefs.TrailsEnabledCheckedAt = &stale
	if err := settings.SaveClonePreferences(ctx, prefs); err != nil {
		t.Fatalf("save stale prefs: %v", err)
	}
	if trailsEnabledForRepo(ctx) {
		t.Fatal("expected trails disabled when cache is stale")
	}

	if err := saveTrailsEnabledForRemote(ctx, "gh", "other", "repo", true); err != nil {
		t.Fatalf("save mismatched cache: %v", err)
	}
	if trailsEnabledForRepo(ctx) {
		t.Fatal("expected trails disabled for mismatched cache scope")
	}
}

func TestTrailWatchDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		forge, owner, rp string
		number           int
		trailID, want    string
	}{
		{"with number", "gh", "acme", "repo", 5, "abc123", "trail #5 (gh/acme/repo, id abc123)"},
		{"without number", "gh", "acme", "repo", 0, "abc123", "trail abc123 (gh/acme/repo)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := trailWatchDescription(tt.forge, tt.owner, tt.rp, tt.number, tt.trailID)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrailListQueryEncodesFiltersAndLimit(t *testing.T) {
	t.Parallel()
	got := trailListQuery([]trail.Status{trail.StatusOpen, trail.StatusDraft}, "alice", 10)
	want := "?author=alice&limit=10&status=open%2Cdraft"
	if got != want {
		t.Fatalf("trailListQuery = %q, want %q", got, want)
	}
}

func TestTrailListQueryAnyStatusOmitsStatusParam(t *testing.T) {
	t.Parallel()
	got := trailListQuery(nil, "", 10)
	if got != "?limit=10" {
		t.Fatalf("trailListQuery = %q, want %q", got, "?limit=10")
	}
}

func TestTrailListQueryCapsLimitAtServerMax(t *testing.T) {
	t.Parallel()
	got := trailListQuery(nil, "", 5000)
	if !strings.Contains(got, "limit=200") {
		t.Fatalf("expected limit capped at 200, got %q", got)
	}
}

func TestTrailListQueryWithOffsetIncludesOffset(t *testing.T) {
	t.Parallel()
	got := trailListQueryWithOffset(nil, "", 10, 20)
	if !strings.Contains(got, "offset=20") {
		t.Fatalf("expected offset in query, got %q", got)
	}
}

func TestFindTrailPaginatesPastServerMax(t *testing.T) {
	t.Parallel()
	var offsets []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsetStr := r.URL.Query().Get("offset")
		offset := 0
		if offsetStr != "" {
			var err error
			offset, err = strconv.Atoi(offsetStr)
			if err != nil {
				t.Fatalf("parse offset %q: %v", offsetStr, err)
			}
		}
		offsets = append(offsets, offset)
		trails := []api.TrailResource{}
		switch offset {
		case 0:
			trails = make([]api.TrailResource, trailListServerMaxLimit)
			for i := range trails {
				trails[i] = api.TrailResource{ID: "trl_first_" + strconv.Itoa(i), Number: i + 1, Branch: "old/" + strconv.Itoa(i)}
			}
		case trailListServerMaxLimit:
			trails = []api.TrailResource{{ID: "trl_target", Number: 201, Branch: "target"}}
		}
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: trails, Total: trailListServerMaxLimit + 1}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranch(context.Background(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findTrailByBranch: %v", err)
	}
	if found == nil || found.ID != "trl_target" {
		t.Fatalf("found = %#v, want trl_target", found)
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != trailListServerMaxLimit {
		t.Fatalf("offsets = %v, want [0 %d]", offsets, trailListServerMaxLimit)
	}
}

func TestFindTrailStopsWhenServerRepeatsUnpaginatedFullPage(t *testing.T) {
	t.Parallel()
	var requests int32
	trails := make([]api.TrailResource, trailListServerMaxLimit)
	for i := range trails {
		trails[i] = api.TrailResource{ID: "trl_repeat_" + strconv.Itoa(i), Number: i + 1, Branch: "old/" + strconv.Itoa(i)}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: trails}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranch(context.Background(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findTrailByBranch: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil", found)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestFindTrailStopsAtMaxPagesWithoutTotal(t *testing.T) {
	t.Parallel()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestNumber := int(atomic.AddInt32(&requests, 1))
		trails := make([]api.TrailResource, trailListServerMaxLimit)
		for i := range trails {
			trailNumber := (requestNumber-1)*trailListServerMaxLimit + i + 1
			trails[i] = api.TrailResource{ID: "trl_" + strconv.Itoa(trailNumber), Number: trailNumber, Branch: "old/" + strconv.Itoa(trailNumber)}
		}
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: trails}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranch(context.Background(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findTrailByBranch: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil", found)
	}
	if got := atomic.LoadInt32(&requests); got != trailFindMaxPages {
		t.Fatalf("requests = %d, want %d", got, trailFindMaxPages)
	}
}

func TestBuildTrailUpdateRequestNeverIncludesBody(t *testing.T) {
	t.Parallel()
	// The body is always sent as a separate PATCH by applyTrailUpdate, so the
	// metadata request builder must never carry it — even when BodyChanged is set.
	req := buildTrailUpdateRequest(&api.TrailResource{Body: "old"}, trailUpdateInputs{
		Status: string(trail.StatusOpen), StatusChanged: true,
		BodyChanged: true, Body: "new body",
	})
	if req.Body != nil {
		t.Fatalf("Body = %q, want nil (body is sent in a separate PATCH)", *req.Body)
	}
	if req.Status == nil || *req.Status != string(trail.StatusOpen) {
		t.Fatalf("Status = %v, want %q", req.Status, trail.StatusOpen)
	}
}

func TestValidateTrailUpdateFieldsRejectsEmptyTitle(t *testing.T) {
	t.Parallel()
	if err := validateTrailUpdateFields(trailUpdateInputs{TitleChanged: true, Title: "   "}); err == nil {
		t.Fatal("expected empty title to be rejected")
	}
}

func TestValidateTrailUpdateFieldsRejectsInvalidStatus(t *testing.T) {
	t.Parallel()
	if err := validateTrailUpdateFields(trailUpdateInputs{StatusChanged: true, Status: "bogus"}); err == nil {
		t.Fatal("expected an invalid status to be rejected")
	}
	// An unchanged status is not validated even if its (unset) value is invalid.
	if err := validateTrailUpdateFields(trailUpdateInputs{Status: "bogus"}); err != nil {
		t.Fatalf("unchanged status should not be validated: %v", err)
	}
}

func TestInteractiveBodySeed(t *testing.T) {
	t.Parallel()

	// Fetch error → omit the field so a transient failure can't blank the doc.
	if seed, loaded := interactiveBodySeed(trailBody{}, errors.New("boom"), "list body"); loaded || seed != "" {
		t.Fatalf("fetch error → (%q, %v), want (\"\", false)", seed, loaded)
	}
	// Editable document present → seed from the fetched body verbatim.
	if seed, loaded := interactiveBodySeed(trailBody{text: "doc body", exists: true, editable: true}, nil, "list body"); !loaded || seed != "doc body" {
		t.Fatalf("present editable doc → (%q, %v), want (\"doc body\", true)", seed, loaded)
	}
	// Document absent but a list body exists → fall back to it, do NOT show blank.
	if seed, loaded := interactiveBodySeed(trailBody{exists: false}, nil, "list body"); !loaded || seed != "list body" {
		t.Fatalf("absent doc with list body → (%q, %v), want (\"list body\", true)", seed, loaded)
	}
	// Document absent and no list body → an empty, editable field is correct.
	if seed, loaded := interactiveBodySeed(trailBody{exists: false}, nil, ""); !loaded || seed != "" {
		t.Fatalf("absent doc no list body → (%q, %v), want (\"\", true)", seed, loaded)
	}
	// Present but not editable (server returned markdown:null) → omit the field so
	// the CLI can't flatten a body it can't losslessly round-trip.
	if seed, loaded := interactiveBodySeed(trailBody{text: "flattened", exists: true, editable: false}, nil, "list body"); loaded || seed != "" {
		t.Fatalf("non-editable doc → (%q, %v), want (\"\", false)", seed, loaded)
	}
}

func TestTrailCreateRejectsUnexpectedArgs(t *testing.T) {
	t.Parallel()
	cmd := newTrailCreateCmd()
	if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
		t.Fatalf("%s accepted an unexpected positional arg", cmd.Name())
	}
}

func TestParseTrailStatusFilterAcceptsCommaSeparatedStatuses(t *testing.T) {
	t.Parallel()
	got, err := parseTrailStatusFilter("draft, open,closed")
	if err != nil {
		t.Fatalf("parseTrailStatusFilter: %v", err)
	}
	want := []trail.Status{trail.StatusDraft, trail.StatusOpen, trail.StatusClosed}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseTrailStatusFilterRejectsInvalidStatus(t *testing.T) {
	t.Parallel()
	if _, err := parseTrailStatusFilter("open,nope"); err == nil {
		t.Fatal("expected invalid status error")
	}
	// in_progress was retired server-side and must no longer parse.
	if _, err := parseTrailStatusFilter("in_progress"); err == nil {
		t.Fatal("expected invalid status error for retired in_progress")
	}
}

func TestParseTrailStatusFilterAnySentinelMeansNoFilter(t *testing.T) {
	t.Parallel()
	got, err := parseTrailStatusFilter(trailListStatusAny)
	if err != nil {
		t.Fatalf("parseTrailStatusFilter(%q): %v", trailListStatusAny, err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil (any disables the filter)", got)
	}
}

func TestPrintTrailListDefaultRepoShapeShowsAuthor(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{
			Branch:    "feat/repo-wide",
			Status:    trail.StatusOpen,
			Author:    &trail.Author{Login: &alice},
			UpdatedAt: time.Now(),
		},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []trail.Status{trail.StatusOpen},
	})

	text := out.String()
	for _, want := range []string{"Open · 1 trail", "feat/repo-wide", trailListTestAuthorAlice} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintTrailListAuthorFilteredShapeHidesAuthor(t *testing.T) {
	t.Parallel()
	longBranch := "feature/very-long-branch-name-that-must-remain-visible"
	alice := trailListTestAuthorAlice

	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{
			Branch:    longBranch,
			Status:    trail.StatusOpen,
			Author:    &trail.Author{Login: &alice},
			UpdatedAt: time.Now().Add(-24 * time.Hour),
		},
	}, trailListDisplayOptions{
		RequestedAuthor: trailListTestAuthorAlice,
		StatusFilters:   []trail.Status{trail.StatusOpen},
	})

	text := out.String()
	if !strings.Contains(text, "alice · 1 open") {
		t.Fatalf("output should contain author/status header, got:\n%s", text)
	}
	if !strings.Contains(text, longBranch) {
		t.Fatalf("output should contain full branch %q, got:\n%s", longBranch, text)
	}
	if strings.Count(text, "alice") != 1 {
		t.Fatalf("filtered author output should not repeat author in rows, got:\n%s", text)
	}
}

func TestPrintTrailListYourTrailsRelabelsAndSurfacesGhLogin(t *testing.T) {
	t.Parallel()
	mixedCase := "Alice" // gh returned a different case than the filter
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{
			Branch:    "feat/x",
			Status:    trail.StatusOpen,
			Author:    &trail.Author{Login: &mixedCase},
			UpdatedAt: time.Now(),
		},
	}, trailListDisplayOptions{
		RequestedAuthor: "alice",
		CurrentUser:     "alice",
		StatusFilters:   []trail.Status{trail.StatusOpen},
	})

	text := out.String()
	if !strings.Contains(text, "Your trails (alice) · 1 open") {
		t.Fatalf("expected 'Your trails (alice)' header, got:\n%s", text)
	}
}

func TestPrintTrailListShowsURLColumnWhenPresent(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Number: 5, Branch: "feat/a", Status: trail.StatusOpen, URL: "https://entire.io/gh/acme/repo/trails/5", Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{StatusFilters: []trail.Status{trail.StatusOpen}})

	text := out.String()
	if !strings.Contains(text, "URL") || !strings.Contains(text, "https://entire.io/gh/acme/repo/trails/5") {
		t.Fatalf("expected a URL column with the trail url, got:\n%s", text)
	}
}

func TestPrintTrailListOmitsURLColumnWhenAbsent(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Number: 5, Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{StatusFilters: []trail.Status{trail.StatusOpen}})

	// The column header must not appear when no trail carries a URL (e.g. an
	// older server that omits the field and no local fallback was attached).
	if text := out.String(); strings.Contains(text, "URL") {
		t.Fatalf("expected URL column omitted when no trail has a url, got:\n%s", text)
	}
}

func TestPrintTrailListAnyStatusShowsStatusColumn(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	bob := trailListTestAuthorBob
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
		{Branch: "fix/b", Status: trail.StatusDraft, Author: &trail.Author{Login: &bob}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    2,
	})

	text := out.String()
	for _, want := range []string{"Recent trails · 2", "STATUS", "open", "draft", "feat/a", trailListTestAuthorAlice, "fix/b", trailListTestAuthorBob} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintTrailListSingleStatusFilterOmitsStatusColumn(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []trail.Status{trail.StatusOpen},
		TotalMatched:    1,
	})

	if text := out.String(); strings.Contains(text, "STATUS") {
		t.Fatalf("single-status list should not repeat the status as a column, got:\n%s", text)
	}
}

func TestPrintTrailDetailsOmitsWhitespacePhase(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTrailDetails(&out, &trail.Metadata{
		Title:  "Whitespace phase",
		Branch: "feat/a",
		Base:   "main",
		Status: trail.StatusOpen,
		Phase:  "   ",
	}, "", "")

	if text := out.String(); strings.Contains(text, "Phase:") {
		t.Fatalf("expected whitespace phase to be omitted, got:\n%s", text)
	}
}

func TestPrintTrailDetailsRendersURLAndDescription(t *testing.T) {
	t.Parallel()
	m := &trail.Metadata{Title: "T", Branch: "feat/a", Base: "main", Status: trail.StatusOpen}

	var out bytes.Buffer
	printTrailDetails(&out, m, "https://entire.io/gh/acme/repo/trails/5", "line one\nline two")
	text := out.String()
	if !strings.Contains(text, "URL:") || !strings.Contains(text, "https://entire.io/gh/acme/repo/trails/5") {
		t.Fatalf("expected a URL line, got:\n%s", text)
	}
	if !strings.Contains(text, "Description:") || !strings.Contains(text, "line one\nline two") {
		t.Fatalf("expected a Description block, got:\n%s", text)
	}

	// Empty URL and whitespace-only body are omitted.
	out.Reset()
	printTrailDetails(&out, m, "", "   ")
	if text := out.String(); strings.Contains(text, "URL:") || strings.Contains(text, "Description:") {
		t.Fatalf("expected URL/Description omitted for empty values, got:\n%s", text)
	}
}

func TestPrintTrailListShowsPhaseWhenPresent(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Phase: "has_code", Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []trail.Status{trail.StatusOpen},
		TotalMatched:    1,
	})

	text := out.String()
	for _, want := range []string{"PHASE", "has code"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintTrailListSingularRecentTrailWhenOne(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
	})

	text := out.String()
	if !strings.Contains(text, "Recent trail · 1") {
		t.Fatalf("expected singular 'Recent trail · 1', got:\n%s", text)
	}
	if strings.Contains(text, "Recent trails · 1") {
		t.Fatalf("did not expect plural 'trails' for count 1, got:\n%s", text)
	}
}

func TestPrintTrailListUnknownStatusRendersInStatusColumn(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	unknownStatus := trail.Status("experimental_review")
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/known", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
		{Branch: "feat/odd", Status: unknownStatus, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    2,
	})

	// A status the CLI doesn't know yet must not disappear; it renders
	// verbatim (underscores humanized) in the status column.
	text := out.String()
	for _, want := range []string{"Recent trails · 2", "experimental review", "feat/odd"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintTrailListTruncatedShowsShownOfTotal(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    5,
	})

	if text := out.String(); !strings.Contains(text, "Recent trails · 1/5") {
		t.Fatalf("expected truncated header 'Recent trails · 1/5', got:\n%s", text)
	}
}

func TestPrintTrailListTruncatedSingleStatusHeaderShowsShownOfTotal(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []trail.Status{trail.StatusOpen},
		TotalMatched:    3,
	})

	// Pluralized by the total match count, not the truncated page size.
	if text := out.String(); !strings.Contains(text, "Open · 1/3 trails") {
		t.Fatalf("expected truncated header 'Open · 1/3 trails', got:\n%s", text)
	}
}

func TestPrintTrailListFullPageKeepsPlainCounts(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusOpen, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    1,
	})

	text := out.String()
	if !strings.Contains(text, "Recent trail · 1") || strings.Contains(text, "1/1") {
		t.Fatalf("expected plain counts without slash when nothing was truncated, got:\n%s", text)
	}
}

func TestPrintTrailListEmptyDefaultStatusNamesFilterAndHints(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTrailListEmpty(&out, "", []trail.Status{trail.StatusOpen})

	text := out.String()
	for _, want := range []string{
		"No open trails found.",
		"Use --status any to see trails in other statuses.",
		"entire trail create",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintTrailListEmptyAnyStatusOmitsHint(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTrailListEmpty(&out, "", nil)

	text := out.String()
	if !strings.Contains(text, "No trails found.") {
		t.Fatalf("expected generic empty message, got:\n%s", text)
	}
	if strings.Contains(text, "--status any") {
		t.Fatalf("should not hint --status any when no status filter is active, got:\n%s", text)
	}
}

func TestPrintTrailListEmptyIncludesAuthor(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTrailListEmpty(&out, trailListTestAuthorAlice, []trail.Status{trail.StatusOpen})

	text := out.String()
	if !strings.Contains(text, "No open trails found for alice.") {
		t.Fatalf("expected author in empty message, got:\n%s", text)
	}
}

func TestFetchCurrentUserLoginReturnsLogin(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "octocat\n", nil)

	got, err := fetchCurrentUserLogin(context.Background(), r)
	if err != nil {
		t.Fatalf("fetchCurrentUserLogin: %v", err)
	}
	if got != "octocat" {
		t.Fatalf("got %q, want octocat", got)
	}
}

func TestFetchCurrentUserLoginRejectsEmptyLogin(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "\n", nil)

	if _, err := fetchCurrentUserLogin(context.Background(), r); err == nil {
		t.Fatal("expected error for empty login")
	}
}

func TestFetchCurrentUserLoginWrapsGhError(t *testing.T) {
	t.Parallel()
	r := newFakeRunner()
	r.set("gh", []string{"api", "user", "--jq", ".login"}, "", errors.New("gh: not authenticated"))

	_, err := fetchCurrentUserLogin(context.Background(), r)
	if err == nil {
		t.Fatal("expected error")
	}
	// Surface the hint about the --author <login> fallback.
	if !strings.Contains(err.Error(), "--author <login>") {
		t.Fatalf("error should mention the --author fallback hint, got: %v", err)
	}
}

func TestResolveTrailBodyInput(t *testing.T) {
	t.Parallel()

	t.Run("literal --body", func(t *testing.T) {
		t.Parallel()
		const want = "a literal body"
		text, changed, err := resolveTrailBodyInput(want, "", true, false, nil)
		if err != nil || !changed || text != want {
			t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", text, changed, err, want)
		}
	})

	t.Run("--body-file reads file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "body.md")
		if err := os.WriteFile(path, []byte("# Title\n\nfrom file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		text, changed, err := resolveTrailBodyInput("", path, false, true, nil)
		if err != nil || !changed || text != "# Title\n\nfrom file\n" {
			t.Fatalf("got (%q, %v, %v), want file contents", text, changed, err)
		}
	})

	t.Run("--body - reads stdin", func(t *testing.T) {
		t.Parallel()
		text, changed, err := resolveTrailBodyInput("-", "", true, false, strings.NewReader("piped body"))
		if err != nil || !changed || text != "piped body" {
			t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", text, changed, err, "piped body")
		}
	})

	t.Run("--body-file - reads stdin", func(t *testing.T) {
		t.Parallel()
		text, changed, err := resolveTrailBodyInput("", "-", false, true, strings.NewReader("piped via file flag"))
		if err != nil || !changed || text != "piped via file flag" {
			t.Fatalf("got (%q, %v, %v), want stdin contents", text, changed, err)
		}
	})

	t.Run("--body and --body-file are mutually exclusive", func(t *testing.T) {
		t.Parallel()
		if _, _, err := resolveTrailBodyInput("x", "f.md", true, true, nil); err == nil {
			t.Fatal("expected error when both --body and --body-file are set")
		}
	})

	t.Run("nothing set means no change", func(t *testing.T) {
		t.Parallel()
		text, changed, err := resolveTrailBodyInput("", "", false, false, nil)
		if err != nil || changed || text != "" {
			t.Fatalf("got (%q, %v, %v), want (\"\", false, nil)", text, changed, err)
		}
	})

	t.Run("--body - with no stdin errors", func(t *testing.T) {
		t.Parallel()
		if _, _, err := resolveTrailBodyInput("-", "", true, false, nil); err == nil {
			t.Fatal("expected an error reading stdin when none is available")
		}
	})

	t.Run("--body-file with missing path errors", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "does-not-exist.md")
		if _, _, err := resolveTrailBodyInput("", missing, false, true, nil); err == nil {
			t.Fatal("expected an error reading a nonexistent body file")
		}
	})
}

// patchRecorder is an httptest server that records every PATCH it receives.
func patchRecorder(t *testing.T) (*httptest.Server, *[]api.TrailUpdateRequest) {
	t.Helper()
	var got []api.TrailUpdateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("unexpected method %s", r.Method)
		}
		var req api.TrailUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode patch body: %v", err)
		}
		got = append(got, req)
		if _, err := io.WriteString(w, `{"trail":{"number":42}}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestApplyTrailUpdateSplitsBodyAndMetadata(t *testing.T) {
	t.Parallel()
	srv, got := patchRecorder(t)
	client := api.NewClientWithBaseURL("tok", srv.URL)
	found := &api.TrailResource{Number: 42}

	err := applyTrailUpdate(t.Context(), client, "gh", "acme", "repo", found, trailUpdateInputs{
		Status: "open", StatusChanged: true,
		Body: "new body", BodyChanged: true,
	})
	if err != nil {
		t.Fatalf("applyTrailUpdate: %v", err)
	}
	// Body and metadata must go in SEPARATE PATCHes (the API rejects combining them).
	if len(*got) != 2 {
		t.Fatalf("got %d PATCH calls, want 2 (metadata + body)", len(*got))
	}
	for _, req := range *got {
		if req.Body != nil && req.Status != nil {
			t.Fatalf("a single PATCH carried both body and metadata: %+v", req)
		}
	}
}

func TestApplyTrailUpdateBodyOnlyIsSinglePatch(t *testing.T) {
	t.Parallel()
	srv, got := patchRecorder(t)
	client := api.NewClientWithBaseURL("tok", srv.URL)
	found := &api.TrailResource{Number: 1488} // branchless trail: Branch == "" but Number > 0

	err := applyTrailUpdate(t.Context(), client, "gh", "acme", "repo", found, trailUpdateInputs{
		Body: "branchless body", BodyChanged: true,
	})
	if err != nil {
		t.Fatalf("applyTrailUpdate: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("got %d PATCH calls, want 1", len(*got))
	}
	if (*got)[0].Body == nil || *(*got)[0].Body != "branchless body" {
		t.Fatalf("PATCH body = %v, want %q", (*got)[0].Body, "branchless body")
	}
}

func TestApplyTrailUpdateRejectsNumberlessTrail(t *testing.T) {
	t.Parallel()
	client := api.NewClientWithBaseURL("tok", "http://unused.invalid")
	found := &api.TrailResource{Number: 0, Title: "Draft"}
	if err := applyTrailUpdate(t.Context(), client, "gh", "acme", "repo", found, trailUpdateInputs{Body: "x", BodyChanged: true}); err == nil {
		t.Fatal("expected error updating a trail with no number")
	}
}

func TestApplyTrailUpdateReportsPartialFailureWhenBodyPatchFails(t *testing.T) {
	t.Parallel()
	// Metadata PATCH lands first and succeeds; the body PATCH (second) fails. The
	// error must surface that the metadata change already landed so the partial
	// outcome isn't hidden from the user.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			if _, err := io.WriteString(w, `{"trail":{"number":42}}`); err != nil {
				t.Errorf("write response: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, `{"error":"body write failed"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	client := api.NewClientWithBaseURL("tok", srv.URL)

	err := applyTrailUpdate(t.Context(), client, "gh", "acme", "repo", &api.TrailResource{Number: 42}, trailUpdateInputs{
		Status: string(trail.StatusOpen), StatusChanged: true,
		Body: "new body", BodyChanged: true,
	})
	if err == nil {
		t.Fatal("expected an error when the body PATCH fails")
	}
	if !strings.Contains(err.Error(), "metadata updated, but body update failed") {
		t.Fatalf("error = %q, want it to report the partial failure", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("got %d PATCH calls, want 2 (metadata succeeded, body attempted)", got)
	}
}

func trailBodyServer(t *testing.T, json string) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, json); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return api.NewClientWithBaseURL("tok", srv.URL)
}

func TestFetchTrailBodyPrefersMarkdown(t *testing.T) {
	t.Parallel()
	// A new server (PR #2574) returns markdown; the CLI must seed from it (not the
	// flattened text_snapshot) so formatting survives an edit round-trip.
	client := trailBodyServer(t, `{"trail":{"number":5,"body_document":{"text_snapshot":"H b","markdown":"# H\n\n**b**"}}}`)
	body, err := fetchTrailBody(t.Context(), client, "gh", "acme", "repo", 5)
	if err != nil || !body.exists || !body.editable || body.text != "# H\n\n**b**" {
		t.Fatalf("fetchTrailBody = %+v (err %v), want text markdown, exists+editable", body, err)
	}
}

func TestFetchTrailBodyFallsBackToSnapshotWhenMarkdownAbsent(t *testing.T) {
	t.Parallel()
	// An old server (pre-#2574) omits the markdown field entirely → fall back to the
	// raw, untrimmed text_snapshot, still editable (its prior behavior).
	client := trailBodyServer(t, `{"trail":{"number":5,"body_document":{"text_snapshot":"  spaced body \n"}}}`)
	body, err := fetchTrailBody(t.Context(), client, "gh", "acme", "repo", 5)
	if err != nil || !body.exists || !body.editable || body.text != "  spaced body \n" {
		t.Fatalf("fetchTrailBody = %+v (err %v), want raw snapshot, exists+editable", body, err)
	}

	// fetchTrailDescription (used by `show`) trims for display.
	desc, err := fetchTrailDescription(t.Context(), client, "gh", "acme", "repo", 5)
	if err != nil || desc != "spaced body" {
		t.Fatalf("fetchTrailDescription = (%q, %v), want (%q, nil)", desc, err, "spaced body")
	}
}

func TestFetchTrailBodyRefusesNullMarkdown(t *testing.T) {
	t.Parallel()
	// A new server that could NOT losslessly serialize the body sends markdown:null.
	// The body is marked non-editable so the CLI never flattens it on save; the
	// text_snapshot is still carried for display-only use.
	client := trailBodyServer(t, `{"trail":{"number":5,"body_document":{"text_snapshot":"flattened","markdown":null}}}`)
	body, err := fetchTrailBody(t.Context(), client, "gh", "acme", "repo", 5)
	if err != nil || !body.exists || body.editable || body.text != "flattened" {
		t.Fatalf("fetchTrailBody = %+v (err %v), want exists, NOT editable, text=snapshot", body, err)
	}
}

func TestFetchTrailBodyRendersMarkdownInShow(t *testing.T) {
	t.Parallel()
	// `show` renders the markdown (trimmed), not the flattened snapshot.
	client := trailBodyServer(t, `{"trail":{"number":5,"body_document":{"text_snapshot":"H","markdown":"\n# H\n\n**b**\n"}}}`)
	desc, err := fetchTrailDescription(t.Context(), client, "gh", "acme", "repo", 5)
	if err != nil || desc != "# H\n\n**b**" {
		t.Fatalf("fetchTrailDescription = (%q, %v), want trimmed markdown", desc, err)
	}
}

func TestFetchTrailBodyReportsMissingDocument(t *testing.T) {
	t.Parallel()
	client := trailBodyServer(t, `{"trail":{"number":5}}`)
	body, err := fetchTrailBody(t.Context(), client, "gh", "acme", "repo", 5)
	if err != nil || body.exists || body.text != "" {
		t.Fatalf("fetchTrailBody (no doc) = %+v (err %v), want zero value", body, err)
	}
}

func TestInteractiveUpdateInputsMarksOnlyEditedFields(t *testing.T) {
	t.Parallel()
	seed := trailUpdateEdits{status: "open", title: "Title", body: "hello"}

	// No edits → nothing marked changed.
	got := interactiveUpdateInputs(seed, seed)
	if got.StatusChanged || got.TitleChanged || got.BodyChanged {
		t.Fatalf("unedited form marked a field changed: %+v", got)
	}

	// Only the body edited.
	const editedBody = "an edited body"
	got = interactiveUpdateInputs(seed, trailUpdateEdits{status: "open", title: "Title", body: editedBody})
	if !got.BodyChanged || got.StatusChanged || got.TitleChanged {
		t.Fatalf("body-only edit flags wrong: %+v", got)
	}
	if got.Body != editedBody {
		t.Fatalf("Body = %q, want %q", got.Body, editedBody)
	}
}

func TestInteractiveUpdateInputsUneditedBodyIsNotResent(t *testing.T) {
	t.Parallel()
	// Body failed to load → seed is empty. The user edits status only and leaves
	// the body untouched. The empty body must NOT be marked changed, or a body
	// PATCH would erase the live collaborative document.
	seed := trailUpdateEdits{status: "open", title: "Title", body: ""}
	got := interactiveUpdateInputs(seed, trailUpdateEdits{status: "closed", title: "Title", body: ""})
	if got.BodyChanged {
		t.Fatal("unedited (empty-seed) body marked changed — would erase the document")
	}
	if !got.StatusChanged {
		t.Fatal("status edit should be marked changed")
	}
}

func TestResolveTrailUpdateTargetKeepsBranchSeparate(t *testing.T) {
	t.Parallel()

	// --branch must be a branch-only lookup, never a number/id selector — so
	// `--branch 123` targets the branch named "123", not trail #123.
	cmd := newTrailUpdateCmd()
	if err := cmd.Flags().Set("branch", "123"); err != nil {
		t.Fatal(err)
	}
	target, err := resolveTrailUpdateTarget(cmd, nil)
	if err != nil || target.branch != "123" || target.selector != "" {
		t.Fatalf("--branch 123 → %+v (err %v), want branch=123 selector empty", target, err)
	}

	// A positional arg is a generic selector (number/id/branch).
	cmd = newTrailUpdateCmd()
	target, err = resolveTrailUpdateTarget(cmd, []string{"123"})
	if err != nil || target.selector != "123" || target.branch != "" {
		t.Fatalf("positional 123 → %+v (err %v), want selector=123 branch empty", target, err)
	}

	// --trail is a generic selector.
	cmd = newTrailUpdateCmd()
	if err := cmd.Flags().Set("trail", "abc"); err != nil {
		t.Fatal(err)
	}
	target, err = resolveTrailUpdateTarget(cmd, nil)
	if err != nil || target.selector != "abc" || target.branch != "" {
		t.Fatalf("--trail abc → %+v (err %v), want selector=abc branch empty", target, err)
	}

	// Combining sources is rejected.
	cmd = newTrailUpdateCmd()
	if err := cmd.Flags().Set("branch", "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTrailUpdateTarget(cmd, []string{"1"}); err == nil {
		t.Fatal("combining a positional with --branch should error")
	}

	// --trail and a positional both feed target.selector; supplying both must
	// still error rather than letting the positional silently clobber --trail.
	cmd = newTrailUpdateCmd()
	if err := cmd.Flags().Set("trail", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveTrailUpdateTarget(cmd, []string{"b"}); err == nil {
		t.Fatal("combining a positional with --trail should error")
	}
}

func TestTrailUpdateAcceptsTrailSelectorArg(t *testing.T) {
	t.Parallel()
	cmd := newTrailUpdateCmd()
	if err := cmd.Args(cmd, []string{"1488"}); err != nil {
		t.Fatalf("update rejected a single trail selector arg: %v", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Fatal("update accepted two positional args")
	}
}
