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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	change "github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const (
	changeListTestAuthorAlice = "alice"
	changeListTestAuthorBob   = "bob"
	// changeTestBasePath is the changes endpoint for the gh/acme/repo fixture.
	changeTestBasePath = "/api/v1/changes/gh/acme/repo"
	// changeTestListBody is the stand-in list-resource body used across the
	// resolveChangeUpdateBody fallback tests.
	changeTestListBody = "list body"
)

func TestNewChangeCreateRequestUsesLinkBranchAction(t *testing.T) {
	req := newChangeCreateRequest("title", "body", "feature/x", "main", "open", "", "", nil)

	require.Equal(t, api.ChangeCreateRequest{
		Title:        "title",
		Body:         "body",
		BranchName:   "feature/x",
		BranchAction: "link",
		Base:         "main",
		Status:       "open",
	}, req)
}

func TestNewChangeCreateRequestCanBeBranchless(t *testing.T) {
	req := newChangeCreateRequest("title", "body", "", "main", "open", "", "", nil)

	require.Equal(t, api.ChangeCreateRequest{
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

func TestPrepareChangeCreateBranchSkipsBranchlessChange(t *testing.T) {
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
			state, err := prepareChangeCreateBranch(context.Background(), io.Discard, io.Discard, nil, "origin", tc.branch, "main", tc.noBranch)

			require.NoError(t, err)
			require.False(t, state.NeedsCreation)
			require.False(t, state.LocalCreated)
			require.False(t, state.RemotePushed)
		})
	}
}

func TestValidateChangeCreateFlagCombosRejectsBranchlessConflicts(t *testing.T) {
	t.Parallel()

	t.Run("branch", func(t *testing.T) {
		t.Parallel()
		cmd := newChangeCreateCmd()
		require.NoError(t, cmd.Flags().Set("branch", "feature/x"))

		err := validateChangeCreateFlagCombos(cmd, false, true)

		require.EqualError(t, err, "cannot combine --no-branch with --branch")
	})

	t.Run("checkout", func(t *testing.T) {
		t.Parallel()
		cmd := newChangeCreateCmd()

		err := validateChangeCreateFlagCombos(cmd, true, true)

		require.EqualError(t, err, "cannot combine --no-branch with --checkout")
	})
}

func TestChangeCreateCommandRejectsBranchlessFlagConflictsBeforeRepoLookup(t *testing.T) {
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
			cmd := newChangeCreateCmd()
			cmd.SetContext(context.Background())
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()

			require.EqualError(t, err, tc.wantErr)
		})
	}
}

func TestResolveChangeCreateFieldsBranchlessNonInteractiveClearsBranchAndDefaultsStatus(t *testing.T) {
	t.Parallel()

	cmd := newChangeCreateCmd()
	require.NoError(t, cmd.Flags().Set("title", "  Branchless change  "))

	title, body, base, branch, status, err := resolveChangeCreateFields(cmd, io.Discard, "  Branchless change  ", "body", " main ", "", "", "feature/current", true)

	require.NoError(t, err)
	require.Equal(t, "Branchless change", title)
	require.Equal(t, "body", body)
	require.Equal(t, "main", base)
	require.Empty(t, branch)
	require.Equal(t, string(change.StatusOpen), status)
}

func TestValidateChangeCreateFieldsAllowsBranchlessEmptyBranch(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateChangeCreateFields(context.Background(), "Branchless", "", string(change.StatusOpen), true))
	require.EqualError(t,
		validateChangeCreateFields(context.Background(), "Branch backed", "", string(change.StatusOpen), false),
		"branch name is required")
}

func TestRunChangeCreateInteractiveBranchlessSkipsBranchPrompt(t *testing.T) {
	// No t.Parallel: runChangeCreateForm is package-global test seam.
	previous := runChangeCreateForm
	calls := 0
	runChangeCreateForm = func(*huh.Form) error {
		calls++
		return nil
	}
	t.Cleanup(func() { runChangeCreateForm = previous })

	title := "  Branchless change  "
	body := "body"
	branch := "must-be-cleared"
	status := ""

	err := runChangeCreateInteractive(&title, &body, &branch, &status, true)

	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, "Branchless change", title)
	require.Empty(t, branch)
	require.Equal(t, string(change.StatusOpen), status)
}

func TestRunChangeCreateBranchlessHappyPath(t *testing.T) {
	// No t.Parallel: uses t.Chdir plus auth/tokenstore package-level test seams.
	prevChangeClient := newChangeAPIClient
	newChangeAPIClient = func(ctx context.Context, insecureHTTP bool, _ string) (*api.Client, error) {
		return NewAuthenticatedAPIClient(ctx, insecureHTTP)
	}
	t.Cleanup(func() { newChangeAPIClient = prevChangeClient })
	var gotCreate map[string]any
	var gotCreateAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"exchanged-token","token_type":"Bearer","expires_in":3600}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/changes/gh/acme/repo":
			gotCreateAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotCreate); err != nil {
				t.Errorf("decode create request: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeCreateResponse{
				Change: api.ChangeResource{ID: "trl_branchless", Title: "Branchless full path"},
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
	runGitChangeTest(t, repoDir, "remote", "add", "origin", "https://github.com/acme/repo.git")
	t.Chdir(repoDir)

	cmd := newChangeCreateCmd()
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
	require.Equal(t, string(change.StatusOpen), gotCreate["status"])
	require.NotContains(t, gotCreate, "branchName")
	require.NotContains(t, gotCreate, "branchAction")
	require.Contains(t, out.String(), `Created change "Branchless full path" (ID: trl_branchless)`)
	require.NotContains(t, out.String(), "Pushed branch")
	require.Empty(t, errOut.String())
}

func TestCleanupCreatedChangeBranch(t *testing.T) {
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
			localDir, originDir, repo := initChangeCleanupRepo(t)
			defer repo.Close()
			t.Chdir(localDir)

			runGitChangeTest(t, localDir, "branch", branch)
			if tc.remotePushed {
				runGitChangeTest(t, localDir, "push", "origin", branch)
			}
			if tc.checkoutBranch {
				runGitChangeTest(t, localDir, "checkout", branch)
			}

			var errBuf bytes.Buffer
			cleanupCreatedChangeBranch(context.Background(), repo, "origin", branch, tc.localCreated, tc.remotePushed, &errBuf)

			require.Equal(t, tc.wantLocalBranch, gitBranchExistsChangeTest(t, localDir, branch), "local branch mismatch; stderr: %s", errBuf.String())
			require.Equal(t, tc.wantRemoteBranch, gitBranchExistsChangeTest(t, originDir, branch), "remote branch mismatch; stderr: %s", errBuf.String())
			if tc.checkoutBranch {
				require.Contains(t, errBuf.String(), "not deleting remote branch")
			}
		})
	}
}

func initChangeCleanupRepo(t *testing.T) (localDir, originDir string, repo *git.Repository) {
	t.Helper()

	testutil.IsolateGitConfigEnv(t)
	tmp := t.TempDir()
	localDir = filepath.Join(tmp, "local")
	originDir = filepath.Join(tmp, "origin.git")
	require.NoError(t, os.MkdirAll(localDir, 0o755))
	runGitChangeTest(t, tmp, "init", "--bare", originDir)
	repo = initOpenedTestRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "test\n")
	runGitChangeTest(t, localDir, "add", "README.md")
	runGitChangeTest(t, localDir, "commit", "-m", "initial")
	runGitChangeTest(t, localDir, "remote", "add", "origin", originDir)
	return localDir, originDir, repo
}

func runGitChangeTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
}

func gitBranchExistsChangeTest(t *testing.T, repoDir, branch string) bool {
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

func TestRunChangeListAll_PrintsLoginHintWhenNotLoggedIn(t *testing.T) {
	// No t.Parallel: SetResolveContextForAPIForTest and
	prevChangeClient := newChangeAPIClient
	newChangeAPIClient = func(ctx context.Context, insecureHTTP bool, _ string) (*api.Client, error) {
		return NewAuthenticatedAPIClient(ctx, insecureHTTP)
	}
	t.Cleanup(func() { newChangeAPIClient = prevChangeClient })
	//
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
	err := runChangeListAll(t.Context(), &out, &errOut, changeListOptions{Status: defaultChangeListStatus, Limit: defaultChangeListLimit})
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
	if strings.Contains(out.String(), "No changes found") {
		t.Errorf("stdout = %q, must not render logged-out state as an empty change list", out.String())
	}
	wantHint := "Not logged in. Run 'entire login' to authenticate."
	if got := errOut.String(); !strings.Contains(got, wantHint) {
		t.Errorf("errOut = %q, want hint %q", got, wantHint)
	}
}

func TestRunChangeListAll_ValidatesOptionsBeforeAuth(t *testing.T) {
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

	opts := changeListOptions{Status: defaultChangeListStatus, Limit: 0}

	var out, errOut bytes.Buffer
	err := runChangeListAll(t.Context(), &out, &errOut, opts)
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

func TestChangeRootPrintsHelp(t *testing.T) {
	t.Parallel()
	cmd := newChangeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute change root: %v", err)
	}
	text := out.String()
	for _, want := range []string{"A change ties together the context for a branch", "`entire change finding`", "show", "list", "create", "finding"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help output missing %q, got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Not logged in") {
		t.Fatalf("change root should not perform auth/API work, got:\n%s", text)
	}
}

func TestChangesBasePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		forge, owner, rp string
		want             string
	}{
		{"gh forge", "gh", "acme", "repo", "/api/v1/changes/gh/acme/repo"},
		{"et forge", "et", "acme", "repo", "/api/v1/changes/et/acme/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := changesBasePath(tt.forge, tt.owner, tt.rp)
			if got != tt.want {
				t.Fatalf("changesBasePath(%q, %q, %q) = %q, want %q", tt.forge, tt.owner, tt.rp, got, tt.want)
			}
		})
	}
}

func TestChangeNumberPath(t *testing.T) {
	t.Parallel()
	got := changeNumberPath("gh", "acme", "repo", 575)
	want := "/api/v1/changes/gh/acme/repo/575"
	if got != want {
		t.Fatalf("changeNumberPath = %q, want %q", got, want)
	}
	// Regression guard: the single-change endpoint is keyed by the integer change
	// number, never the UUID id — the server's parseChangeNumber rejects a UUID
	// (it starts with a non-[1-9] char), which previously surfaced as a 400.
	if strings.Contains(got, "-") {
		t.Fatalf("changeNumberPath must use the integer number, got %q", got)
	}
}

func TestChangeWebURL(t *testing.T) {
	t.Parallel()
	want := "https://entire.io/gh/acme/repo/changes/575"
	if got := changeWebURL("https://entire.io", "gh", "acme", "repo", 575); got != want {
		t.Fatalf("changeWebURL = %q, want %q", got, want)
	}
	// A trailing slash on the base must not double up.
	if got := changeWebURL("https://entire.io/", "gh", "acme", "repo", 575); got != want {
		t.Fatalf("changeWebURL(trailing slash) = %q, want %q", got, want)
	}
}

func TestPrintCreatedChange(t *testing.T) {
	t.Parallel()

	// The server-provided URL is used verbatim.
	var out bytes.Buffer
	printCreatedChange(&out, api.ChangeResource{Title: "Fix it", Branch: "feat/x", ID: "abc123", Number: 575, URL: "https://entire.io/gh/acme/repo/changes/575/fix-it"}, "gh", "acme", "repo")
	text := out.String()
	if !strings.Contains(text, `Created change "Fix it" for branch feat/x (ID: abc123)`) {
		t.Fatalf("missing create summary line, got:\n%s", text)
	}
	if !strings.Contains(text, "URL: https://entire.io/gh/acme/repo/changes/575/fix-it") {
		t.Fatalf("expected the server-provided URL, got:\n%s", text)
	}

	// Without a number, omit the URL line.
	out.Reset()
	printCreatedChange(&out, api.ChangeResource{Title: "No num", Branch: "feat/y", ID: "def456"}, "gh", "acme", "repo")
	if text := out.String(); strings.Contains(text, "URL:") {
		t.Fatalf("expected URL omitted when number and URL are absent, got:\n%s", text)
	}
}

func TestChangeDisplayURL(t *testing.T) {
	t.Parallel()

	// Server URL wins, even when a number is present.
	got := changeDisplayURL(api.ChangeResource{Number: 5, URL: "https://server/url"}, "gh", "acme", "repo")
	if got != "https://server/url" {
		t.Fatalf("expected server URL, got %q", got)
	}

	// Falls back to a constructed URL for older servers that omit it.
	got = changeDisplayURL(api.ChangeResource{Number: 5}, "gh", "acme", "repo")
	if !strings.HasSuffix(got, "/gh/acme/repo/changes/5") {
		t.Fatalf("expected constructed fallback URL, got %q", got)
	}

	// Nothing to show when neither is available.
	if got := changeDisplayURL(api.ChangeResource{}, "gh", "acme", "repo"); got != "" {
		t.Fatalf("expected empty URL, got %q", got)
	}
}

func TestChangeDescriptionForDisplay(t *testing.T) {
	t.Parallel()
	if got := changeDescriptionForDisplay("the body", true); got != "the body" {
		t.Fatalf("non-empty body: got %q, want %q", got, "the body")
	}
	if got := changeDescriptionForDisplay("the body", false); got != "the body" {
		t.Fatalf("non-empty body (not loaded): got %q, want %q", got, "the body")
	}
	// Loaded but empty/whitespace → explicit placeholder.
	if got := changeDescriptionForDisplay("", true); got != noChangeDescription {
		t.Fatalf("loaded+empty: got %q, want %q", got, noChangeDescription)
	}
	if got := changeDescriptionForDisplay("   ", true); got != noChangeDescription {
		t.Fatalf("loaded+whitespace: got %q, want %q", got, noChangeDescription)
	}
	// Not loaded (fetch failed) → nothing (the caller already warned).
	if got := changeDescriptionForDisplay("", false); got != "" {
		t.Fatalf("not loaded+empty: got %q, want empty", got)
	}
}

func TestDecodeChangeResourceReadsTheDirectResource(t *testing.T) {
	t.Parallel()
	// The detail route returns the resource itself; sibling keys are ignored.
	payload := `{"id":"trl_direct","number":7,"branch":"feat/direct","checkpoints":[],"hasWritePermission":true}`
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(payload))}
	got, err := decodeChangeResource(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "trl_direct" || got.Number != 7 || got.Branch != "feat/direct" {
		t.Fatalf("decoded resource = %#v", got)
	}
}

func TestFetchChangeDescription_ReadsNestedBodyDocument(t *testing.T) {
	t.Parallel()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// Regression guard: the text lives one level down in bodyDocument, and
		// `checkpoints` is a bare array the decode must ignore.
		if _, err := io.WriteString(w, `{"number":777,"branch":"feat/x","bodyDocument":{"textSnapshot":"the intent text"},"checkpoints":[],"hasWritePermission":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	bodyText, _, err := fetchChangeDescription(t.Context(), client, "gh", "acme", "repo", 777)
	if err != nil {
		t.Fatalf("fetchChangeDescription: %v", err)
	}
	if want := "/api/v1/changes/gh/acme/repo/777"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if bodyText != "the intent text" {
		t.Fatalf("bodyText = %q, want %q", bodyText, "the intent text")
	}
}

func TestResolveChangeUpdateBody_PrefersDetailSnapshot(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"number":42,"bodyDocument":{"textSnapshot":"the real body","etag":"W/\"etag-real\""},"checkpoints":[],"hasWritePermission":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	// The list resource omits the description, so found.Body is empty. The
	// seed must come from the detail endpoint, not the empty list body.
	found := &api.ChangeResource{Number: 42, Body: ""}
	body, etag, err := resolveChangeUpdateBody(t.Context(), client, "gh", "acme", "repo", found)
	if err != nil {
		t.Fatalf("resolveChangeUpdateBody: %v", err)
	}
	if body != "the real body" {
		t.Fatalf("body = %q, want %q", body, "the real body")
	}
	if etag != `W/"etag-real"` {
		t.Fatalf("etag = %q, want the detail's real etag to survive the round trip", etag)
	}
}

// TestResolveChangeUpdateBody_ReturnsETagEvenWhenSnapshotEmpty covers a real
// document whose description happens to be empty (already cleared): the etag
// describes the document that was read, not its text, so it must still come
// back — dropping it here would force the non-interactive best-effort refetch
// in runChangeUpdateWithClient to redo a read that already succeeded.
func TestResolveChangeUpdateBody_ReturnsETagEvenWhenSnapshotEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"number":42,"bodyDocument":{"textSnapshot":"","etag":"W/\"etag-empty-doc\""},"checkpoints":[],"hasWritePermission":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found := &api.ChangeResource{Number: 42, Body: changeTestListBody}
	body, etag, err := resolveChangeUpdateBody(t.Context(), client, "gh", "acme", "repo", found)
	if err != nil {
		t.Fatalf("resolveChangeUpdateBody: %v", err)
	}
	if body != changeTestListBody {
		t.Fatalf("body = %q, want fallback %q (empty snapshot doesn't override it)", body, changeTestListBody)
	}
	if etag != `W/"etag-empty-doc"` {
		t.Fatalf("etag = %q, want the real etag even though the snapshot was empty", etag)
	}
}

func TestResolveChangeUpdateBody_FallsBackToListBody(t *testing.T) {
	t.Parallel()
	// Older/partial server: detail omits bodyDocument (textSnapshot empty).
	// The seed must fall back to the list body rather than blanking it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, `{"number":42,"checkpoints":[],"hasWritePermission":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found := &api.ChangeResource{Number: 42, Body: changeTestListBody}
	body, etag, err := resolveChangeUpdateBody(t.Context(), client, "gh", "acme", "repo", found)
	if err != nil {
		t.Fatalf("resolveChangeUpdateBody: %v", err)
	}
	if body != changeTestListBody {
		t.Fatalf("body = %q, want %q", body, changeTestListBody)
	}
	if etag != "" {
		t.Fatalf("etag = %q, want empty when falling back to the list body", etag)
	}
}

func TestResolveChangeUpdateBody_ReturnsErrorOnFetchFailure(t *testing.T) {
	t.Parallel()
	// A detail-fetch failure must be surfaced (not swallowed) so the caller can
	// warn: a blank baseline could otherwise silently overwrite an unseen body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found := &api.ChangeResource{Number: 42, Body: changeTestListBody}
	body, etag, err := resolveChangeUpdateBody(t.Context(), client, "gh", "acme", "repo", found)
	if err == nil {
		t.Fatal("expected error on fetch failure, got nil")
	}
	if body != changeTestListBody {
		t.Fatalf("body = %q, want fallback %q", body, changeTestListBody)
	}
	if etag != "" {
		t.Fatalf("etag = %q, want empty on fetch failure", etag)
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

func TestParseChangeNumberArg(t *testing.T) {
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
			got, err := parseChangeNumberArg(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseChangeNumberArg(%v) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("parseChangeNumberArg(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestConfirmChangeDeletion(t *testing.T) {
	t.Parallel()

	// --force proceeds without prompting (no TTY needed).
	var buf bytes.Buffer
	proceed, err := confirmChangeDeletion(t.Context(), &buf, 575, "Some title", true, false)
	if err != nil || !proceed {
		t.Fatalf("force: got (proceed=%v, err=%v), want (true, nil)", proceed, err)
	}

	// Non-interactive without --force must refuse, not delete unprompted.
	buf.Reset()
	proceed, err = confirmChangeDeletion(t.Context(), &buf, 575, "Some title", false, false)
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
	proceed, err = confirmChangeDeletion(ctx, &buf, 575, "Some title", false, true)
	if err != nil || proceed {
		t.Fatalf("cancelled ctx: got (proceed=%v, err=%v), want (false, nil)", proceed, err)
	}
}

func TestDeleteChangeByNumber(t *testing.T) {
	t.Parallel()

	t.Run("deletes via the integer number path and accepts 204", func(t *testing.T) {
		t.Parallel()
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		if err := deleteChangeByNumber(t.Context(), client, "gh", "acme", "repo", 575); err != nil {
			t.Fatalf("deleteChangeByNumber: %v", err)
		}
		if gotMethod != http.MethodDelete {
			t.Fatalf("method = %q, want DELETE", gotMethod)
		}
		if want := "/api/v1/changes/gh/acme/repo/575"; gotPath != want {
			t.Fatalf("path = %q, want %q (integer number, not UUID)", gotPath, want)
		}
	})

	t.Run("surfaces a non-2xx status", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": "Change not found"}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		if err := deleteChangeByNumber(t.Context(), client, "gh", "acme", "repo", 999); err == nil {
			t.Fatal("expected error for 404, got nil")
		}
	})
}

// Not parallel: uses t.Chdir() to point ResolveRemoteRepo at a fake repo.
func TestResolveChangeRemote_RejectsUnsupportedForge(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", "origin", "git@gitlab.com:acme/my-app.git")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if err := cmd.Run(); err != nil {
		t.Fatalf("git remote add: %v", err)
	}
	t.Chdir(repoDir)

	_, _, _, err := resolveChangeRemote(context.Background())
	if err == nil {
		t.Fatal("expected error for gitlab.com origin, got nil")
	}
	if !strings.Contains(err.Error(), "not on a forge supported by Entire changes") {
		t.Fatalf("error message does not mention unsupported forge: %v", err)
	}
}

// TestChangesEnabledForRepo_ReadsClonePreference verifies the prompt-path gate
// is a local clone-preference read only. The API enablement decision itself
// (2xx => enabled) is covered by api.TestClient_TrailsEnabled.
//
// Not parallel: uses t.Chdir() to point clone preferences at a fake repo.
func TestChangeEnablementCache_ReadsClonePreference(t *testing.T) {
	// Inline of the former changesEnabledForRepo wrapper: resolves the current
	// repo's enablement scope and checks the cached enablement decision.
	changesEnabledForCurrentRepo := func(ctx context.Context) bool {
		scope, err := currentTrailEnablementScope(ctx)
		if err != nil {
			return false
		}
		return cachedTrailsEnablementForScope(ctx, scope, time.Now()) == trailEnablementCacheEnabled
	}

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

	if changesEnabledForCurrentRepo(ctx) {
		t.Fatal("expected changes disabled when cache is absent")
	}
	if err := saveTrailsEnabledForRepo(ctx, false); err != nil {
		t.Fatalf("save false cache: %v", err)
	}
	if changesEnabledForCurrentRepo(ctx) {
		t.Fatal("expected changes disabled when cache is false")
	}
	if err := saveTrailsEnabledForRepo(ctx, true); err != nil {
		t.Fatalf("save true cache: %v", err)
	}
	if !changesEnabledForCurrentRepo(ctx) {
		t.Fatal("expected changes enabled when cache is true")
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
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		*p = *prefs
		return nil
	}); err != nil {
		t.Fatalf("save auth-mismatched prefs: %v", err)
	}
	if changesEnabledForCurrentRepo(ctx) {
		t.Fatal("expected changes disabled for mismatched auth cache scope")
	}
	prefs.TrailsEnabledAuthKey = currentAuthKey
	fresh := time.Now()
	prefs.TrailsEnabledCheckedAt = &fresh
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		*p = *prefs
		return nil
	}); err != nil {
		t.Fatalf("restore auth-matched prefs: %v", err)
	}

	stale := time.Now().Add(-trailEnablementCacheTTL - time.Minute)
	prefs.TrailsEnabledCheckedAt = &stale
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		*p = *prefs
		return nil
	}); err != nil {
		t.Fatalf("save stale prefs: %v", err)
	}
	if changesEnabledForCurrentRepo(ctx) {
		t.Fatal("expected changes disabled when cache is stale")
	}

	if err := saveTrailsEnabledForRemote(ctx, "gh", "other", "repo", true); err != nil {
		t.Fatalf("save mismatched cache: %v", err)
	}
	if changesEnabledForCurrentRepo(ctx) {
		t.Fatal("expected changes disabled for mismatched cache scope")
	}
}

func TestChangeWatchDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		forge, owner, rp string
		number           int
		changeID, want   string
	}{
		{"with number", "gh", "acme", "repo", 5, "abc123", "change #5 (gh/acme/repo, id abc123)"},
		{"without number", "gh", "acme", "repo", 0, "abc123", "change abc123 (gh/acme/repo)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := changeWatchDescription(tt.forge, tt.owner, tt.rp, tt.number, tt.changeID)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChangeListPageQueryEncodesFilters(t *testing.T) {
	t.Parallel()
	got := changeListPageQuery([]change.Status{change.StatusOpen, change.StatusDraft}, 10, "")
	want := "?pageSize=10&status=open%2Cdraft"
	if got != want {
		t.Fatalf("changeListPageQuery = %q, want %q", got, want)
	}
}

func TestChangeListPageQueryAnyStatusOmitsStatusParam(t *testing.T) {
	t.Parallel()
	got := changeListPageQuery(nil, 10, "")
	if got != "?pageSize=10" {
		t.Fatalf("changeListPageQuery = %q, want %q", got, "?pageSize=10")
	}
}

func TestChangeListPageQueryCapsPageSizeAtServerMax(t *testing.T) {
	t.Parallel()
	got := changeListPageQuery(nil, 5000, "")
	if !strings.Contains(got, "pageSize=100") {
		t.Fatalf("expected pageSize capped at 100, got %q", got)
	}
}

func TestListChangeResourcesRejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		_, _, err := listChangeResources(t.Context(), nil, "gh", "acme", "repo", nil, "", limit)
		if err == nil || err.Error() != "limit must be greater than 0" {
			t.Fatalf("limit %d error = %v, want limit validation error", limit, err)
		}
	}
}

// A --limit above entire-api's page cap is satisfied by pagination, so the list
// must not warn about a server-side cap the way the retired backend did.
func TestRunChangeListAllPrintsNoServerLimitNote(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"items":[{"id":"trl_1","number":1,"branch":"feature/x","status":"open"}],"totalCount":1033}`)
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	var out bytes.Buffer
	err := runChangeListAllWithClient(t.Context(), &out, client, changeListOptions{
		Repo: "gh/acme/repo", Limit: 500,
	}, []change.Status{change.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "feature/x") {
		t.Fatalf("change missing from output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "exceeds the server maximum") {
		t.Fatalf("unexpected server-limit note:\n%s", out.String())
	}
}

func TestListChangeResourcesStopsWhenAuthorLimitIsSatisfied(t *testing.T) {
	t.Parallel()
	requests := 0
	login := "alice"
	next := "should-not-be-requested"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 1 {
			t.Errorf("unexpected extra page request with token %q", r.URL.Query().Get("pageToken"))
		}
		changes := make([]api.ChangeResource, changeListServerMaxLimit)
		for i := range changes {
			changes[i] = api.ChangeResource{
				ID:     "trl_" + strconv.Itoa(i),
				Number: i + 1,
				Author: &change.Author{Login: &login},
			}
		}
		if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
			Changes: changes, NextPageToken: &next, Total: 10_000,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()
	client := api.NewClientWithBaseURL("tok", srv.URL)

	items, total, err := listChangeResources(t.Context(), client, "gh", "acme", "repo", nil, login, 5)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if len(items) != 5 || total != 5 {
		t.Fatalf("items=%d total=%d, want 5/5", len(items), total)
	}
}

func TestChangeListPageQueryUsesEntireAPIPagination(t *testing.T) {
	t.Parallel()
	got := changeListPageQuery([]change.Status{change.StatusOpen}, 100, "next page")
	want := "?pageSize=100&pageToken=next+page&status=open"
	if got != want {
		t.Fatalf("changeListPageQuery = %q, want %q", got, want)
	}
}

func TestFindChangeByNumberUsesDirectEntireAPIRoute(t *testing.T) {
	t.Parallel()
	const changeNumber = 1201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/changes/gh/acme/repo/1201"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("query = %q, want empty", r.URL.RawQuery)
		}
		if err := json.NewEncoder(w).Encode(api.ChangeResource{ID: "trl_old", Number: changeNumber, Branch: "old/change"}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findChangeByNumber(t.Context(), client, "gh", "acme", "repo", changeNumber)
	if err != nil {
		t.Fatalf("findChangeByNumber: %v", err)
	}
	if found == nil || found.ID != "trl_old" {
		t.Fatalf("found = %#v, want trl_old", found)
	}
}

// A 2xx whose body carries no change identity is "not found", not a change whose
// every field is zero. Selector callers act on `found != nil`, so returning a
// phantom would make `change show <number>` render an empty change instead of
// reporting the miss.
func TestFindChangeByNumberTreatsIdentitylessBodyAsNotFound(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body string }{
		{name: "empty object", body: `{}`},
		{name: "error body with a 200", body: `{"error":"not found"}`},
		{name: "list response on the number route", body: `{"items":[{"id":"trl_a","number":1}],"totalCount":1}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(w, tt.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer srv.Close()

			found, err := findChangeByNumber(t.Context(), api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", 575)
			if err != nil {
				t.Fatalf("findChangeByNumber: %v", err)
			}
			if found != nil {
				t.Fatalf("found = %#v, want nil (not a zero-valued change)", found)
			}
		})
	}
}

func TestFindChangePaginatesPastServerMax(t *testing.T) {
	t.Parallel()
	const nextPage = "next-page"
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("pageToken")
		tokens = append(tokens, token)
		changes := []api.ChangeResource{}
		var next *string
		switch token {
		case "":
			changes = make([]api.ChangeResource, changeListServerMaxLimit)
			for i := range changes {
				changes[i] = api.ChangeResource{ID: "trl_first_" + strconv.Itoa(i), Number: i + 1, Branch: "old/" + strconv.Itoa(i)}
			}
			n := nextPage
			next = &n
		case nextPage:
			changes = []api.ChangeResource{{ID: "trl_target", Number: 201, Branch: "target"}}
		}
		if err := json.NewEncoder(w).Encode(api.ChangeListResponse{Changes: changes, Total: changeListServerMaxLimit + 1, NextPageToken: next}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findChangeByBranch(context.Background(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findChangeByBranch: %v", err)
	}
	if found == nil || found.ID != "trl_target" {
		t.Fatalf("found = %#v, want trl_target", found)
	}
	if len(tokens) != 2 || tokens[0] != "" || tokens[1] != nextPage {
		t.Fatalf("page tokens = %v, want [\"\" next-page]", tokens)
	}
}

// A change on the very last page of the search budget must still be found — the
// page loop has to reach changeFindMaxPages, not stop one short of it.
func TestFindChangePaginatesToTheEndOfItsBudget(t *testing.T) {
	t.Parallel()
	const targetPage = changeFindMaxPages
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page := int(atomic.AddInt32(&requests, 1))
		response := api.ChangeListResponse{}
		if page == targetPage {
			response.Changes = []api.ChangeResource{{ID: "trl_target", Number: 1001, Branch: "target"}}
		} else {
			response.Changes = make([]api.ChangeResource, changeListServerMaxLimit)
			for i := range response.Changes {
				number := (page-1)*changeListServerMaxLimit + i + 1
				response.Changes[i] = api.ChangeResource{ID: "trl_" + strconv.Itoa(number), Number: number, Branch: "old/" + strconv.Itoa(number)}
			}
			next := "page-" + strconv.Itoa(page+1)
			response.NextPageToken = &next
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findChangeByBranch(t.Context(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findChangeByBranch: %v", err)
	}
	if found == nil || found.ID != "trl_target" {
		t.Fatalf("found = %#v, want trl_target", found)
	}
	if got := atomic.LoadInt32(&requests); got != targetPage {
		t.Fatalf("requests = %d, want %d", got, targetPage)
	}
}

func TestFindChangeStopsWhenServerRepeatsUnpaginatedFullPage(t *testing.T) {
	t.Parallel()
	var requests int32
	changes := make([]api.ChangeResource, changeListServerMaxLimit)
	for i := range changes {
		changes[i] = api.ChangeResource{ID: "trl_repeat_" + strconv.Itoa(i), Number: i + 1, Branch: "old/" + strconv.Itoa(i)}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		next := "same-page"
		if err := json.NewEncoder(w).Encode(api.ChangeListResponse{Changes: changes, NextPageToken: &next}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findChangeByBranch(context.Background(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findChangeByBranch: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil", found)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestFindChangeStopsAtMaxPagesWithoutTotal(t *testing.T) {
	t.Parallel()
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestNumber := int(atomic.AddInt32(&requests, 1))
		changes := make([]api.ChangeResource, changeListServerMaxLimit)
		for i := range changes {
			changeNumber := (requestNumber-1)*changeListServerMaxLimit + i + 1
			changes[i] = api.ChangeResource{ID: "trl_" + strconv.Itoa(changeNumber), Number: changeNumber, Branch: "old/" + strconv.Itoa(changeNumber)}
		}
		next := "page-" + strconv.Itoa(requestNumber)
		if err := json.NewEncoder(w).Encode(api.ChangeListResponse{Changes: changes, NextPageToken: &next}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findChangeByBranch(context.Background(), client, "gh", "acme", "repo", "target")
	if err != nil {
		t.Fatalf("findChangeByBranch: %v", err)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil", found)
	}
	if got := atomic.LoadInt32(&requests); got != changeFindMaxPages {
		t.Fatalf("requests = %d, want %d", got, changeFindMaxPages)
	}
}

// TestRunChangeUpdateClearsDescriptionWithEmptyBody covers `--body=`: an empty
// description is a value to write, not an absence. Two things have to hold for
// that, and neither is visible from a passing update test — the write must be
// triggered by the flag having been set rather than by the text being non-empty,
// and markdown must reach the wire as an empty string (with omitempty it would
// drop out of the JSON and the server would reject the write as "exactly one of
// markdown/contentJson is required"). Both failures silently do nothing.
func TestRunChangeUpdateClearsDescriptionWithEmptyBody(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
				Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}},
				Total:   1,
			}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath+"/7":
			// No bodyDocument, so the etag fetch below comes back empty and
			// the write still falls back to Overwrite — the case this test
			// pins.
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeResource{ID: "trl_1", Number: 7, Branch: "feature/x"}); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		case r.Method == http.MethodPut && r.URL.Path == changeTestBasePath+"/7/body":
			mu.Lock()
			defer mu.Unlock()
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Errorf("decode body request: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeBodyDocument{}); err != nil {
				t.Errorf("encode body response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	err := runChangeUpdateWithClient(t.Context(), &out, io.Discard, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:      "feature/x",
		Body:        "",
		BodyChanged: true,
	})

	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[string]any{"markdown": "", "overwrite": true}, put)
	require.Contains(t, out.String(), "Updated change for branch feature/x")
}

func TestValidateChangeUpdateFieldsRejectsEmptyTitle(t *testing.T) {
	t.Parallel()
	if err := validateChangeUpdateFields(changeUpdateInputs{TitleChanged: true, Title: "   "}); err == nil {
		t.Fatal("expected empty title to be rejected")
	}
}

func TestChangeCreateAndUpdateRejectUnexpectedArgs(t *testing.T) {
	t.Parallel()
	for _, cmd := range []*cobra.Command{newChangeCreateCmd(), newChangeUpdateCmd()} {
		if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
			t.Fatalf("%s accepted an unexpected positional arg", cmd.Name())
		}
	}
}

func TestParseChangeStatusFilterAcceptsCommaSeparatedStatuses(t *testing.T) {
	t.Parallel()
	got, err := parseChangeStatusFilter("draft, open,closed")
	if err != nil {
		t.Fatalf("parseChangeStatusFilter: %v", err)
	}
	want := []change.Status{change.StatusDraft, change.StatusOpen, change.StatusClosed}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("status[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseChangeStatusFilterRejectsInvalidStatus(t *testing.T) {
	t.Parallel()
	if _, err := parseChangeStatusFilter("open,nope"); err == nil {
		t.Fatal("expected invalid status error")
	}
	// in_progress was retired server-side and must no longer parse.
	if _, err := parseChangeStatusFilter("in_progress"); err == nil {
		t.Fatal("expected invalid status error for retired in_progress")
	}
}

func TestParseChangeStatusFilterAnySentinelMeansNoFilter(t *testing.T) {
	t.Parallel()
	got, err := parseChangeStatusFilter(changeListStatusAny)
	if err != nil {
		t.Fatalf("parseChangeStatusFilter(%q): %v", changeListStatusAny, err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil (any disables the filter)", got)
	}
}

func TestPrintChangeListDefaultRepoShapeShowsAuthor(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{
			Branch:    "feat/repo-wide",
			Status:    change.StatusOpen,
			Author:    &change.Author{Login: &alice},
			UpdatedAt: time.Now(),
		},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []change.Status{change.StatusOpen},
	})

	text := out.String()
	for _, want := range []string{"Open · 1 change", "feat/repo-wide", changeListTestAuthorAlice} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintChangeListAuthorFilteredShapeHidesAuthor(t *testing.T) {
	t.Parallel()
	longBranch := "feature/very-long-branch-name-that-must-remain-visible"
	alice := changeListTestAuthorAlice

	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{
			Branch:    longBranch,
			Status:    change.StatusOpen,
			Author:    &change.Author{Login: &alice},
			UpdatedAt: time.Now().Add(-24 * time.Hour),
		},
	}, changeListDisplayOptions{
		RequestedAuthor: changeListTestAuthorAlice,
		StatusFilters:   []change.Status{change.StatusOpen},
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

func TestPrintChangeListYourChangesRelabelsAndSurfacesGhLogin(t *testing.T) {
	t.Parallel()
	mixedCase := "Alice" // gh returned a different case than the filter
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{
			Branch:    "feat/x",
			Status:    change.StatusOpen,
			Author:    &change.Author{Login: &mixedCase},
			UpdatedAt: time.Now(),
		},
	}, changeListDisplayOptions{
		RequestedAuthor: "alice",
		CurrentUser:     "alice",
		StatusFilters:   []change.Status{change.StatusOpen},
	})

	text := out.String()
	if !strings.Contains(text, "Your changes (alice) · 1 open") {
		t.Fatalf("expected 'Your changes (alice)' header, got:\n%s", text)
	}
}

func TestPrintChangeListShowsURLColumnWhenPresent(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Number: 5, Branch: "feat/a", Status: change.StatusOpen, URL: "https://entire.io/gh/acme/repo/changes/5", Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{StatusFilters: []change.Status{change.StatusOpen}})

	text := out.String()
	if !strings.Contains(text, "URL") || !strings.Contains(text, "https://entire.io/gh/acme/repo/changes/5") {
		t.Fatalf("expected a URL column with the change url, got:\n%s", text)
	}
}

func TestPrintChangeListOmitsURLColumnWhenAbsent(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Number: 5, Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{StatusFilters: []change.Status{change.StatusOpen}})

	// The column header must not appear when no change carries a URL (e.g. an
	// older server that omits the field and no local fallback was attached).
	if text := out.String(); strings.Contains(text, "URL") {
		t.Fatalf("expected URL column omitted when no change has a url, got:\n%s", text)
	}
}

func TestPrintChangeListAnyStatusShowsStatusColumn(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	bob := changeListTestAuthorBob
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
		{Branch: "fix/b", Status: change.StatusDraft, Author: &change.Author{Login: &bob}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    2,
	})

	text := out.String()
	for _, want := range []string{"Recent changes · 2", "STATUS", "open", "draft", "feat/a", changeListTestAuthorAlice, "fix/b", changeListTestAuthorBob} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintChangeListSingleStatusFilterOmitsStatusColumn(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []change.Status{change.StatusOpen},
		TotalMatched:    1,
	})

	if text := out.String(); strings.Contains(text, "STATUS") {
		t.Fatalf("single-status list should not repeat the status as a column, got:\n%s", text)
	}
}

func TestPrintChangeDetailsOmitsWhitespacePhase(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printChangeDetails(&out, &change.Metadata{
		Title:  "Whitespace phase",
		Branch: "feat/a",
		Base:   "main",
		Status: change.StatusOpen,
		Phase:  "   ",
	}, "", "")

	if text := out.String(); strings.Contains(text, "Phase:") {
		t.Fatalf("expected whitespace phase to be omitted, got:\n%s", text)
	}
}

func TestPrintChangeDetailsRendersURLAndDescription(t *testing.T) {
	t.Parallel()
	m := &change.Metadata{Title: "T", Branch: "feat/a", Base: "main", Status: change.StatusOpen}

	var out bytes.Buffer
	printChangeDetails(&out, m, "https://entire.io/gh/acme/repo/changes/5", "line one\nline two")
	text := out.String()
	if !strings.Contains(text, "URL:") || !strings.Contains(text, "https://entire.io/gh/acme/repo/changes/5") {
		t.Fatalf("expected a URL line, got:\n%s", text)
	}
	if !strings.Contains(text, "Description:") || !strings.Contains(text, "line one\nline two") {
		t.Fatalf("expected a Description block, got:\n%s", text)
	}

	// Empty URL and whitespace-only body are omitted.
	out.Reset()
	printChangeDetails(&out, m, "", "   ")
	if text := out.String(); strings.Contains(text, "URL:") || strings.Contains(text, "Description:") {
		t.Fatalf("expected URL/Description omitted for empty values, got:\n%s", text)
	}
}

// changeShowTestServer serves the two endpoints `change show` reads: the list
// (which resolves a non-numeric selector and omits the description) and the
// detail (which carries bodyDocument). detailStatus > 0 makes the detail fetch
// fail so the best-effort path can be exercised — pass a branch selector with
// it, because a numeric selector resolves through the detail route itself and
// would fail outright rather than degrade.
func changeShowTestServer(t *testing.T, resource api.ChangeResource, detailSnapshot string, detailStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{Changes: []api.ChangeResource{resource}, Total: 1}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath+"/"+strconv.Itoa(resource.Number):
			if detailStatus > 0 {
				w.WriteHeader(detailStatus)
				if err := json.NewEncoder(w).Encode(map[string]string{"error": "boom"}); err != nil {
					t.Errorf("encode detail error: %v", err)
				}
				return
			}
			detail := resource
			detail.BodyDocument = &api.ChangeBodyDocument{TextSnapshot: detailSnapshot}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(detail); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunChangeShowJSONEmitsOneChangeObject(t *testing.T) {
	t.Parallel()

	alice := changeListTestAuthorAlice
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	resource := api.ChangeResource{
		ID:        "trl_1",
		Number:    7,
		URL:       "https://entire.io/gh/acme/repo/changes/7",
		Branch:    "feature/x",
		Base:      "main",
		Title:     "Shown change",
		Status:    string(change.StatusOpen),
		Phase:     "building",
		Author:    &change.Author{ID: "u1", Login: &alice},
		Assignees: []string{"bob"},
		Labels:    []string{"cli"},
		Type:      string(change.TypeBug),
		Priority:  string(change.PriorityHigh),
		Reviewers: []change.Reviewer{{Login: "rev1", Status: change.ReviewerApproved}},
		CreatedAt: created,
		UpdatedAt: created,
	}
	srv := changeShowTestServer(t, resource, "detail body", 0)

	var out, errOut bytes.Buffer
	err := runChangeShowWithClient(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeShowOptions{Selector: "7", JSON: true})

	require.NoError(t, err)
	require.Empty(t, errOut.String())

	var got change.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got), "output must be a single JSON object: %s", out.String())
	require.Equal(t, 7, got.Number)
	require.Equal(t, change.ID("trl_1"), got.TrailID)
	require.Equal(t, "https://entire.io/gh/acme/repo/changes/7", got.URL)
	require.Equal(t, "feature/x", got.Branch)
	require.Equal(t, "main", got.Base)
	require.Equal(t, "Shown change", got.Title)
	require.Equal(t, change.StatusOpen, got.Status)
	require.Equal(t, "building", got.Phase)
	require.Equal(t, alice, got.AuthorLogin())
	require.Equal(t, []string{"bob"}, got.Assignees)
	require.Equal(t, []string{"cli"}, got.Labels)
	require.Equal(t, change.TypeBug, got.Type)
	require.Equal(t, change.PriorityHigh, got.Priority)
	require.Equal(t, []change.Reviewer{{Login: "rev1", Status: change.ReviewerApproved}}, got.Reviewers)
	// The description lives on the detail endpoint only; JSON must carry it, not
	// the empty list body.
	require.Equal(t, "detail body", got.Body)
	// Human-only decoration must not leak into the data.
	require.NotContains(t, out.String(), noChangeDescription)
	require.NotContains(t, out.String(), "Change: ")
}

// A numeric selector resolves through the detail route, which already returns
// bodyDocument — so `change show <number>` must hit that URL once, not twice.
func TestRunChangeShowNumericSelectorFetchesDetailOnce(t *testing.T) {
	t.Parallel()

	const number = 7
	detailPath := changeTestBasePath + "/" + strconv.Itoa(number)
	var detailHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != detailPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&detailHits, 1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(api.ChangeResource{
			ID: "trl_1", Number: number, Branch: "feature/x", Title: "T",
			Status:       string(change.StatusOpen),
			BodyDocument: &api.ChangeBodyDocument{TextSnapshot: "detail body"},
		}); err != nil {
			t.Errorf("encode detail response: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runChangeShowWithClient(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeShowOptions{Selector: "7", JSON: true})

	require.NoError(t, err)
	require.Empty(t, errOut.String())
	require.EqualValues(t, 1, atomic.LoadInt32(&detailHits), "the detail route must be requested exactly once")

	var got change.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Equal(t, "detail body", got.Body, "the description must come from the resolved resource")
}

func TestRunChangeShowJSONLeavesBodyEmptyWithoutDescription(t *testing.T) {
	t.Parallel()

	srv := changeShowTestServer(t, api.ChangeResource{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}, "", 0)

	var out, errOut bytes.Buffer
	err := runChangeShowWithClient(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeShowOptions{Selector: "7", JSON: true})

	require.NoError(t, err)
	require.Empty(t, errOut.String())

	var got change.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Empty(t, got.Body, "an empty description must stay empty in JSON, not become the display placeholder")
	require.NotContains(t, out.String(), noChangeDescription)
}

// A failed description fetch is best-effort: the warning belongs on stderr so
// stdout stays parseable.
func TestRunChangeShowJSONKeepsStdoutParseableWhenDescriptionFetchFails(t *testing.T) {
	t.Parallel()

	srv := changeShowTestServer(t, api.ChangeResource{ID: "trl_1", Number: 7, Branch: "feature/x", Title: "T", Body: changeTestListBody, Status: string(change.StatusOpen)}, "", http.StatusInternalServerError)

	var out, errOut bytes.Buffer
	err := runChangeShowWithClient(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeShowOptions{Selector: "feature/x", JSON: true})

	require.NoError(t, err)
	require.Contains(t, errOut.String(), "could not load change description")

	var got change.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got), "stdout must stay valid JSON: %s", out.String())
	require.Equal(t, changeTestListBody, got.Body, "the list body is the fallback when the detail fetch fails")
}

func TestRunChangeShowTextStillRendersTheHumanView(t *testing.T) {
	t.Parallel()

	srv := changeShowTestServer(t, api.ChangeResource{
		ID: "trl_1", Number: 7, URL: "https://entire.io/gh/acme/repo/changes/7",
		Branch: "feature/x", Base: "main", Title: "Shown change", Status: string(change.StatusOpen),
	}, "detail body", 0)

	var out, errOut bytes.Buffer
	err := runChangeShowWithClient(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeShowOptions{Selector: "7"})

	require.NoError(t, err)
	require.Empty(t, errOut.String())
	text := out.String()
	for _, want := range []string{"Change: Shown change", "Number:", "feature/x", "https://entire.io/gh/acme/repo/changes/7", "Description:", "detail body"} {
		require.Containsf(t, text, want, "text output missing %q:\n%s", want, text)
	}
}

func TestChangeShowCmdHasJSONFlag(t *testing.T) {
	t.Parallel()
	require.NotNil(t, newChangeShowCmd().Flags().Lookup("json"), "change show must offer --json so agents can read it non-interactively")
}

func TestPrintChangeListShowsPhaseWhenPresent(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Phase: "has_code", Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []change.Status{change.StatusOpen},
		TotalMatched:    1,
	})

	text := out.String()
	for _, want := range []string{"PHASE", "has code"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintChangeListSingularRecentChangeWhenOne(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
	})

	text := out.String()
	if !strings.Contains(text, "Recent change · 1") {
		t.Fatalf("expected singular 'Recent change · 1', got:\n%s", text)
	}
	if strings.Contains(text, "Recent changes · 1") {
		t.Fatalf("did not expect plural 'changes' for count 1, got:\n%s", text)
	}
}

func TestPrintChangeListUnknownStatusRendersInStatusColumn(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	unknownStatus := change.Status("experimental_review")
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/known", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
		{Branch: "feat/odd", Status: unknownStatus, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    2,
	})

	// A status the CLI doesn't know yet must not disappear; it renders
	// verbatim (underscores humanized) in the status column.
	text := out.String()
	for _, want := range []string{"Recent changes · 2", "experimental review", "feat/odd"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintChangeListTruncatedShowsShownOfTotal(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    5,
	})

	if text := out.String(); !strings.Contains(text, "Recent changes · 1/5") {
		t.Fatalf("expected truncated header 'Recent changes · 1/5', got:\n%s", text)
	}
}

func TestPrintChangeListTruncatedSingleStatusHeaderShowsShownOfTotal(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []change.Status{change.StatusOpen},
		TotalMatched:    3,
	})

	// Pluralized by the total match count, not the truncated page size.
	if text := out.String(); !strings.Contains(text, "Open · 1/3 changes") {
		t.Fatalf("expected truncated header 'Open · 1/3 changes', got:\n%s", text)
	}
}

func TestPrintChangeListFullPageKeepsPlainCounts(t *testing.T) {
	t.Parallel()
	alice := changeListTestAuthorAlice
	var out bytes.Buffer
	printChangeList(&out, []*change.Metadata{
		{Branch: "feat/a", Status: change.StatusOpen, Author: &change.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, changeListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
		TotalMatched:    1,
	})

	text := out.String()
	if !strings.Contains(text, "Recent change · 1") || strings.Contains(text, "1/1") {
		t.Fatalf("expected plain counts without slash when nothing was truncated, got:\n%s", text)
	}
}

func TestPrintChangeListEmptyDefaultStatusNamesFilterAndHints(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printChangeListEmpty(&out, "", []change.Status{change.StatusOpen})

	text := out.String()
	for _, want := range []string{
		"No open changes found.",
		"Use --status any to see changes in other statuses.",
		"entire change create",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
	}
}

func TestPrintChangeListEmptyAnyStatusOmitsHint(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printChangeListEmpty(&out, "", nil)

	text := out.String()
	if !strings.Contains(text, "No changes found.") {
		t.Fatalf("expected generic empty message, got:\n%s", text)
	}
	if strings.Contains(text, "--status any") {
		t.Fatalf("should not hint --status any when no status filter is active, got:\n%s", text)
	}
}

func TestPrintChangeListEmptyIncludesAuthor(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printChangeListEmpty(&out, changeListTestAuthorAlice, []change.Status{change.StatusOpen})

	text := out.String()
	if !strings.Contains(text, "No open changes found for alice.") {
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

func TestMergeStringSetAddsAndRemoves(t *testing.T) {
	t.Parallel()
	got := mergeStringSet([]string{"a", "b"}, []string{"c", "a"}, []string{"b"})
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestBuildChangeUpdateRequestAssigneesReviewersTypePriority(t *testing.T) {
	t.Parallel()
	current := &api.ChangeResource{
		Assignees:          []string{"alice"},
		RequestedReviewers: []string{"bob"},
	}
	req := buildChangeUpdateRequest(current, changeUpdateInputs{
		AssigneeAdd:     []string{"carol"},
		ReviewerRemove:  []string{"bob"},
		Type:            string(change.TypeBug),
		TypeChanged:     true,
		Priority:        string(change.PriorityHigh),
		PriorityChanged: true,
	})
	if req.Assignees == nil || len(*req.Assignees) != 2 {
		t.Fatalf("Assignees = %v, want [alice carol]", req.Assignees)
	}
	if req.RequestedReviewers == nil || len(*req.RequestedReviewers) != 0 {
		t.Fatalf("RequestedReviewers = %v, want []", req.RequestedReviewers)
	}
	if req.Type == nil || *req.Type != string(change.TypeBug) {
		t.Fatalf("Type = %v, want bug", req.Type)
	}
	if req.Priority == nil || *req.Priority != string(change.PriorityHigh) {
		t.Fatalf("Priority = %v, want high", req.Priority)
	}
}

func TestValidateChangeUpdateFieldsRejectsInvalidTypePriority(t *testing.T) {
	t.Parallel()
	if err := validateChangeUpdateFields(changeUpdateInputs{TypeChanged: true, Type: "epic"}); err == nil {
		t.Error("expected invalid type to be rejected")
	}
	if err := validateChangeUpdateFields(changeUpdateInputs{PriorityChanged: true, Priority: "critical"}); err == nil {
		t.Error("expected invalid priority to be rejected")
	}
	if err := validateChangeUpdateFields(changeUpdateInputs{TypeChanged: true, Type: "bug", PriorityChanged: true, Priority: "low"}); err != nil {
		t.Errorf("valid type/priority rejected: %v", err)
	}
}

// TestChangeUpdateRequestCountsEveryFieldAsMetadata pins the structural rule
// runChangeUpdateWithClient relies on: every field of api.ChangeUpdateRequest
// makes a metadata PATCH. Naming the fields by hand is how a later addition (a
// branch rename, say) gets dropped silently — hasMeta stays false, no PATCH is
// sent, and the command still reports success. This test grows with the struct
// instead. The description is not among them: it has no field here at all, and
// travels as its own PUT .../body.
func TestChangeUpdateRequestCountsEveryFieldAsMetadata(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(api.ChangeUpdateRequest{})
	require.NotZero(t, typ.NumField(), "api.ChangeUpdateRequest has no fields; this test would pass vacuously")
	for i := range typ.NumField() {
		field := typ.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			// Fail rather than skip: a skipped subtest passes quietly, so a
			// non-pointer field would silently retire the guarantee this test
			// exists to provide, exactly when it starts mattering.
			require.Equalf(t, reflect.Ptr, field.Type.Kind(),
				"field %s is not a pointer; decide its zero/set semantics and extend this test before adding it", field.Name)
			var req api.ChangeUpdateRequest
			// A pointer to the zero value is still "provided" — that is how a
			// field is cleared (e.g. an empty title, an emptied assignee list).
			reflect.ValueOf(&req).Elem().Field(i).Set(reflect.New(field.Type.Elem()))

			require.Truef(t, changeUpdateRequestHasFields(req),
				"field %s must count as metadata, otherwise the update drops it without sending a PATCH", field.Name)
		})
	}
}

// TestRunChangeUpdateSendsTitleAsPatchAndBodyAsPut drives the whole update path
// against a server shaped like production's: metadata on PATCH .../{number}, the
// description only on PUT .../{number}/body. The PATCH handler fails the test if
// a description ever appears on it, which is the invariant that matters and the
// one production enforces — it rejects that field naming the PUT route to use
// instead. The rejection's status code is deliberately not modeled: it has been
// a redacted 5xx and is moving to a 4xx, and the CLI must not care either way.
func TestRunChangeUpdateSendsTitleAsPatchAndBodyAsPut(t *testing.T) {
	t.Parallel()

	type changeWrite struct {
		method string
		path   string
		body   map[string]any
	}
	// writes is appended from the handler goroutine and read from the test
	// goroutine, so it is guarded — the counter-based tests in this file use
	// sync/atomic for the same reason; a slice needs a mutex instead.
	var mu sync.Mutex
	var writes []changeWrite
	// Returns nil on a decode failure, which the caller treats as "stop here";
	// the t.Errorf has already failed the test.
	record := func(r *http.Request) map[string]any {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode %s request: %v", r.Method, err)
			return nil
		}
		mu.Lock()
		writes = append(writes, changeWrite{method: r.Method, path: r.URL.Path, body: got})
		mu.Unlock()
		return got
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
				Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Title: "old title", Status: string(change.StatusOpen)}},
				Total:   1,
			}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath+"/7":
			// No bodyDocument, so the etag fetch below comes back empty and
			// the write falls back to Overwrite, as asserted below.
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeResource{ID: "trl_1", Number: 7, Branch: "feature/x", Title: "old title"}); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		case r.Method == http.MethodPatch && r.URL.Path == changeTestBasePath+"/7":
			got := record(r)
			if got == nil {
				return
			}
			if _, hasBody := got["body"]; hasBody {
				// The metadata route does not serve body writes at all, so a
				// description reaching it is the bug this test exists to catch.
				t.Errorf("metadata PATCH carried a description: %v", got)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeUpdateResponse{Change: api.ChangeResource{ID: "trl_1", Number: 7, Branch: "feature/x"}}); err != nil {
				t.Errorf("encode update response: %v", err)
			}
		case r.Method == http.MethodPut && r.URL.Path == changeTestBasePath+"/7/body":
			record(r)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `W/"2026-08-19T00:00:00.000Z"`)
			if err := json.NewEncoder(w).Encode(api.ChangeBodyDocument{TextSnapshot: "new body"}); err != nil {
				t.Errorf("encode body response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runChangeUpdateWithClient(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:       "feature/x",
		Title:        "new title",
		TitleChanged: true,
		Body:         "new body",
		BodyChanged:  true,
	})

	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []changeWrite{
		{method: http.MethodPatch, path: changeTestBasePath + "/7", body: map[string]any{"title": "new title"}},
		// overwrite is what makes editing an existing description work: without
		// it the route 409s on any non-empty body.
		{method: http.MethodPut, path: changeTestBasePath + "/7/body", body: map[string]any{"markdown": "new body", "overwrite": true}},
	}, writes, "title must go out as a metadata PATCH and the body as a PUT .../body")
	require.Contains(t, out.String(), "Updated change for branch feature/x")
	require.Empty(t, errOut.String())
}

// TestRunChangeUpdateReportsNoChangesWhenNothingWasSent pins that an update
// which sends no PATCH does not claim success — agents read the success line as
// confirmation the write landed.
func TestRunChangeUpdateReportsNoChangesWhenNothingWasSent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != changeTestBasePath {
			t.Errorf("no PATCH should be sent, got: %s %s", r.Method, r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
			Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}},
			Total:   1,
		}); err != nil {
			t.Errorf("encode list response: %v", err)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	// The real source of an empty update is the interactive form closing
	// untouched, which leaves every *Changed flag false. That exact input would
	// re-open the form here (noFlags), so stand in with a non-nil but empty
	// assignee slice: it clears noFlags, and buildChangeUpdateRequest still
	// returns an empty request — the same state the split has to refuse to
	// report as a success.
	err := runChangeUpdateWithClient(t.Context(), &out, io.Discard, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:      "feature/x",
		AssigneeAdd: []string{},
	})

	require.NoError(t, err)
	require.Contains(t, out.String(), "No changes to apply")
	require.NotContains(t, out.String(), "Updated change")
}

// TestRunChangeUpdateReportsAppliedMetadataWhenBodyWriteFails covers the
// non-atomic half of the two-request update: the metadata PATCH already landed,
// so the error has to say so rather than reading as "nothing changed".
func TestRunChangeUpdateReportsAppliedMetadataWhenBodyWriteFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
				Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}},
				Total:   1,
			}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath+"/7":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeResource{ID: "trl_1", Number: 7, Branch: "feature/x"}); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/body"):
			w.WriteHeader(http.StatusServiceUnavailable)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": "yjs engine is not configured"}); err != nil {
				t.Errorf("encode rejection: %v", err)
			}
		case r.Method == http.MethodPatch:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeUpdateResponse{Change: api.ChangeResource{ID: "trl_1", Number: 7}}); err != nil {
				t.Errorf("encode update response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	err := runChangeUpdateWithClient(t.Context(), io.Discard, io.Discard, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:       "feature/x",
		Title:        "new title",
		TitleChanged: true,
		Body:         "new body",
		BodyChanged:  true,
	})

	require.ErrorContains(t, err, "change metadata was updated, but the body update failed")
}

// overwriteFieldOrFalse reads the "overwrite" field from a decoded JSON map,
// treating a missing (omitempty-dropped) key the same as an explicit false.
func overwriteFieldOrFalse(m map[string]any) bool {
	v, ok := m["overwrite"].(bool)
	if !ok {
		return false
	}
	return v
}

// TestSendChangeBody_DispatchModes drives sendChangeBody's three-way dispatch
// directly against a fake body route, asserting on the exact request the
// server received: which of Overwrite/If-Match went out, and which didn't.
// Flipping the mode logic to always send Overwrite, or dropping the If-Match
// header, or refusing the no-etag case, each fails a case here.
func TestSendChangeBody_DispatchModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		ifMatch       string
		overwrite     bool
		wantIfMatch   string
		wantOverwrite bool
	}{
		{
			name:          "overwrite flag wins even with an etag in hand",
			ifMatch:       "W/\"etag-1\"",
			overwrite:     true,
			wantIfMatch:   "",
			wantOverwrite: true,
		},
		{
			name:          "etag available, no overwrite: If-Match, no Overwrite",
			ifMatch:       "W/\"etag-1\"",
			overwrite:     false,
			wantIfMatch:   "W/\"etag-1\"",
			wantOverwrite: false,
		},
		{
			name:          "no etag, no overwrite: falls back to Overwrite",
			ifMatch:       "",
			overwrite:     false,
			wantIfMatch:   "",
			wantOverwrite: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotIfMatch string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotIfMatch = r.Header.Get("If-Match")
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Errorf("decode request body: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(api.ChangeBodyDocument{TextSnapshot: "new body"}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer srv.Close()

			client := api.NewClientWithBaseURL("tok", srv.URL)
			err := sendChangeBody(t.Context(), client, "/api/v1/changes/gh/acme/repo/7/body", "new body", tc.ifMatch, tc.overwrite)
			require.NoError(t, err)

			require.Equal(t, tc.wantIfMatch, gotIfMatch, "If-Match header")
			require.Equal(t, "new body", gotBody["markdown"])
			require.Equal(t, tc.wantOverwrite, overwriteFieldOrFalse(gotBody), "overwrite field")
		})
	}
}

// TestSendChangeBody_PreconditionFailedMapsToRerunOrOverwriteHint covers the
// 412 the route returns when the If-Match etag is stale: the CLI must not
// retry with Overwrite on its own, only explain the two ways out.
func TestSendChangeBody_PreconditionFailedMapsToRerunOrOverwriteHint(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "etag mismatch"}); err != nil {
			t.Errorf("encode rejection: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	err := sendChangeBody(t.Context(), client, "/api/v1/changes/gh/acme/repo/7/body", "new body", "W/\"stale\"", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "re-run")
	require.ErrorContains(t, err, "--overwrite")
	require.True(t, api.IsHTTPErrorStatus(err, http.StatusPreconditionFailed))
}

// TestSendChangeBody_ConflictMapsToOverwriteHint covers the 409 the route
// returns when writing without Overwrite against a non-empty description that
// carried no etag (e.g. an older server) — the caller must be told the flag
// that unblocks it, not just "conflict". Note this pins the message-mapping
// logic in isolation rather than a request shape sendChangeBody's own dispatch
// would produce today: with ifMatch=="" it always sets Overwrite:true, so the
// route (which only 409s when Overwrite is absent) would not actually reject
// this exact request. Kept as a direct unit test of the mapping regardless,
// since a future dispatch change (or a server behavior change) could make it
// reachable again.
func TestSendChangeBody_ConflictMapsToOverwriteHint(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "document_not_empty"}); err != nil {
			t.Errorf("encode rejection: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	err := sendChangeBody(t.Context(), client, "/api/v1/changes/gh/acme/repo/7/body", "new body", "", false)
	require.Error(t, err)
	require.ErrorContains(t, err, "--overwrite")
	require.True(t, api.IsHTTPErrorStatus(err, http.StatusConflict))
}

// TestRunChangeUpdateWithClient_EtagThreadsToIfMatch is the end-to-end
// non-interactive case: --body with no --overwrite must fetch the change
// detail purely for its etag (the interactive path already reads the body
// itself; --body skips straight to sendChangeBody, so runChangeUpdateWithClient
// has to do that read itself) and carry it through as If-Match.
func TestRunChangeUpdateWithClient_EtagThreadsToIfMatch(t *testing.T) {
	t.Parallel()

	var gotIfMatch string
	var gotOverwrite bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
				Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}},
				Total:   1,
			}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath+"/7":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeResource{
				ID: "trl_1", Number: 7, Branch: "feature/x",
				BodyDocument: &api.ChangeBodyDocument{TextSnapshot: "old body", ETag: `W/"etag-current"`},
			}); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		case r.Method == http.MethodPut && r.URL.Path == changeTestBasePath+"/7/body":
			gotIfMatch = r.Header.Get("If-Match")
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body request: %v", err)
				return
			}
			gotOverwrite = overwriteFieldOrFalse(got)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeBodyDocument{TextSnapshot: "new body"}); err != nil {
				t.Errorf("encode body response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	err := runChangeUpdateWithClient(t.Context(), io.Discard, io.Discard, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:      "feature/x",
		Body:        "new body",
		BodyChanged: true,
	})

	require.NoError(t, err)
	require.Equal(t, `W/"etag-current"`, gotIfMatch, "the etag read from the detail fetch must reach the wire as If-Match")
	require.False(t, gotOverwrite, "an etag in hand must not also send Overwrite")
}

// TestRunChangeUpdateWithClient_OverwriteFlagSkipsEtagFetch covers --overwrite:
// no etag needed, so the extra detail fetch must not happen at all — a
// request to the detail route here would fail the test via the switch's
// default case.
func TestRunChangeUpdateWithClient_OverwriteFlagSkipsEtagFetch(t *testing.T) {
	t.Parallel()

	var gotOverwrite bool
	var gotIfMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
				Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}},
				Total:   1,
			}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodPut && r.URL.Path == changeTestBasePath+"/7/body":
			gotIfMatch = r.Header.Get("If-Match")
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body request: %v", err)
				return
			}
			gotOverwrite = overwriteFieldOrFalse(got)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeBodyDocument{TextSnapshot: "new body"}); err != nil {
				t.Errorf("encode body response: %v", err)
			}
		default:
			t.Errorf("unexpected request (etag fetch must be skipped under --overwrite): %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	err := runChangeUpdateWithClient(t.Context(), io.Discard, io.Discard, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:      "feature/x",
		Body:        "new body",
		BodyChanged: true,
		Overwrite:   true,
	})

	require.NoError(t, err)
	require.True(t, gotOverwrite)
	require.Empty(t, gotIfMatch)
}

// TestRunChangeUpdateWithClient_EtagFetchFailureWarnsAndFallsBackToOverwrite
// covers the non-interactive best-effort etag fetch actually failing (a hard
// server error, not just an absent bodyDocument): the command must still
// succeed by falling back to Overwrite, but — unlike the graceful
// no-bodyDocument case — this is a real failure, so the caller must be warned
// that the conflict check was skipped rather than have it disappear silently.
func TestRunChangeUpdateWithClient_EtagFetchFailureWarnsAndFallsBackToOverwrite(t *testing.T) {
	t.Parallel()

	var gotOverwrite bool
	var gotIfMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeListResponse{
				Changes: []api.ChangeResource{{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(change.StatusOpen)}},
				Total:   1,
			}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == changeTestBasePath+"/7":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPut && r.URL.Path == changeTestBasePath+"/7/body":
			gotIfMatch = r.Header.Get("If-Match")
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode body request: %v", err)
				return
			}
			gotOverwrite = overwriteFieldOrFalse(got)
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.ChangeBodyDocument{TextSnapshot: "new body"}); err != nil {
				t.Errorf("encode body response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	var errBuf bytes.Buffer
	err := runChangeUpdateWithClient(t.Context(), io.Discard, &errBuf, api.NewClientWithBaseURL("tok", srv.URL), "gh", "acme", "repo", changeUpdateInputs{
		Branch:      "feature/x",
		Body:        "new body",
		BodyChanged: true,
	})

	require.NoError(t, err, "a failed best-effort etag read must not fail the whole command")
	require.True(t, gotOverwrite, "no etag in hand must fall back to Overwrite")
	require.Empty(t, gotIfMatch)
	require.Contains(t, errBuf.String(), "Warning", "the caller must be told the conflict check was skipped, not have it disappear silently")
}

func TestChangeUpdateCmdHasCollaborationFlags(t *testing.T) {
	t.Parallel()
	cmd := newChangeUpdateCmd()
	for _, name := range []string{"add-assignee", "remove-assignee", "add-reviewer", "remove-reviewer", "type", "priority", "overwrite"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("change update missing --%s flag", name)
		}
	}
}

func TestPrintChangeDetailsShowsTypePriorityReviewers(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printChangeDetails(&out, &change.Metadata{
		Title:     "T",
		Branch:    "b",
		Base:      "main",
		Status:    change.StatusOpen,
		Type:      change.TypeBug,
		Priority:  change.PriorityHigh,
		Reviewers: []change.Reviewer{{Login: "rev1", Status: change.ReviewerApproved}},
	}, "", "")
	s := out.String()
	for _, want := range []string{"Type:", "bug", "Priority:", "high", "Reviewers:", "rev1", "approved"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestChangeCreateCmdHasMetadataFlags(t *testing.T) {
	t.Parallel()
	cmd := newChangeCreateCmd()
	for _, name := range []string{"type", "priority", "add-assignee"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("change create missing --%s flag", name)
		}
	}
}

func TestNewChangeCreateRequestCarriesMetadata(t *testing.T) {
	t.Parallel()
	req := newChangeCreateRequest("Title", "body", "b", "main", "open", string(change.TypeBug), string(change.PriorityHigh), []string{"alice"})
	if req.Type != string(change.TypeBug) || req.Priority != string(change.PriorityHigh) {
		t.Fatalf("type/priority = %q/%q, want bug/high", req.Type, req.Priority)
	}
	if len(req.Assignees) != 1 || req.Assignees[0] != "alice" {
		t.Fatalf("assignees = %v, want [alice]", req.Assignees)
	}
}

func TestBuildChangeUpdateRequestTrimsTypeAndPriority(t *testing.T) {
	t.Parallel()
	req := buildChangeUpdateRequest(&api.ChangeResource{}, changeUpdateInputs{
		Type:            "  bug  ",
		TypeChanged:     true,
		Priority:        "  high ",
		PriorityChanged: true,
	})
	if req.Type == nil || *req.Type != string(change.TypeBug) {
		t.Fatalf("Type on wire = %v, want trimmed bug", req.Type)
	}
	if req.Priority == nil || *req.Priority != string(change.PriorityHigh) {
		t.Fatalf("Priority on wire = %v, want trimmed high", req.Priority)
	}
}

// TestResolveChangePushRemote covers the precedence a change branch's delivery
// follows. The tiers are git's own, so a repo whose branch pushes somewhere other
// than "origin" (a fork workflow, remote.pushDefault) gets its branch delivered
// there instead of silently to whatever "origin" happens to be.
func TestResolveChangePushRemote(t *testing.T) {
	cases := []struct {
		name    string
		config  [][]string
		branch  string
		want    string
		wantErr string
	}{
		{
			name:   "nothing declared falls back to origin",
			branch: "feature/x",
			want:   "origin",
		},
		{
			name:   "remote.pushDefault wins over nothing",
			config: [][]string{{"remote.pushDefault", "fork"}},
			branch: "feature/x",
			want:   "fork",
		},
		{
			name:   "branch.<name>.remote is used when it is all that is set",
			config: [][]string{{"branch.feature/x.remote", "base"}},
			branch: "feature/x",
			want:   "base",
		},
		{
			name: "branch.<name>.pushRemote outranks both",
			config: [][]string{
				{"remote.pushDefault", "fork"},
				{"branch.feature/x.remote", "base"},
				{"branch.feature/x.pushRemote", "mine"},
			},
			branch: "feature/x",
			want:   "mine",
		},
		{
			name: "remote.pushDefault outranks branch.<name>.remote",
			config: [][]string{
				{"remote.pushDefault", "fork"},
				{"branch.feature/x.remote", "base"},
			},
			branch: "feature/x",
			want:   "fork",
		},
		{
			// The declaration is read for the branch being created, not for HEAD:
			// `change create --branch other` while on feature/x must not inherit
			// feature/x's push remote.
			name:   "declaration is read for the target branch, not HEAD",
			config: [][]string{{"branch.feature/x.pushRemote", "mine"}},
			branch: "other",
			want:   "origin",
		},
		{
			// Loud, not silently retargeted at origin: silent retargeting is the
			// failure this resolver replaced.
			name:    "a declared name git would read as a flag fails loudly",
			config:  [][]string{{"remote.pushDefault", "--upload-pack=touched"}},
			branch:  "feature/x",
			wantErr: "would read as a command-line flag",
		},
		{
			// A legal git value that the stricter write-direction validator in
			// repo_mirror_use.go would reject. "." means the local repo.
			name:   "a legal dot remote is accepted",
			config: [][]string{{"branch.feature/x.remote", "."}},
			branch: "feature/x",
			want:   ".",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Without this, a developer or CI machine carrying a global
			// remote.pushDefault resolves that instead of the repo-local config
			// under test — and the "falls back to origin" case fails.
			testutil.IsolateGitConfigEnv(t)
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			for _, kv := range tc.config {
				runGitChangeTest(t, dir, "config", kv[0], kv[1])
			}
			t.Chdir(dir)

			got, err := resolveChangePushRemote(context.Background(), tc.branch)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestPrepareChangeCreateBranchPushesToDeclaredRemote is the end-to-end shape of
// the bug: origin exists and is a perfectly good remote, but the branch's
// declared push destination is a different one. The branch must arrive there and
// nowhere else, and cleanup must undo it on that same remote.
func TestPrepareChangeCreateBranchPushesToDeclaredRemote(t *testing.T) {
	localDir, originDir, repo := initChangeCleanupRepo(t)
	defer repo.Close()
	forkDir := filepath.Join(filepath.Dir(localDir), "fork.git")
	runGitChangeTest(t, localDir, "init", "--bare", forkDir)
	runGitChangeTest(t, localDir, "remote", "add", "fork", forkDir)
	runGitChangeTest(t, localDir, "config", "remote.pushDefault", "fork")
	t.Chdir(localDir)

	const branch = "feature/declared"
	ctx := context.Background()
	remote, err := resolveChangePushRemote(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, "fork", remote)

	var out, errOut bytes.Buffer
	state, err := prepareChangeCreateBranch(ctx, &out, &errOut, repo, remote, branch, "main", false)

	require.NoError(t, err, "stderr: %s", errOut.String())
	require.True(t, state.NeedsCreation)
	require.True(t, state.RemotePushed)
	require.True(t, gitBranchExistsChangeTest(t, forkDir, branch), "branch missing from declared remote")
	require.False(t, gitBranchExistsChangeTest(t, originDir, branch), "branch leaked to origin")
	require.Contains(t, out.String(), "Pushed branch "+branch+" to fork")

	cleanupCreatedChangeBranch(ctx, repo, remote, branch, state.LocalCreated, state.RemotePushed, &errOut)
	require.False(t, gitBranchExistsChangeTest(t, forkDir, branch), "cleanup left the branch on the declared remote; stderr: %s", errOut.String())
}
