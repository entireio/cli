package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
	"github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"
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

func TestBuildTrailUpdateRequestCanClearBody(t *testing.T) {
	t.Parallel()
	req := buildTrailUpdateRequest(&api.TrailResource{Body: "old"}, trailUpdateInputs{BodyChanged: true, Body: ""})
	if req.Body == nil {
		t.Fatal("Body pointer is nil, want empty string pointer")
	}
	if *req.Body != "" {
		t.Fatalf("Body = %q, want empty string", *req.Body)
	}
}

func TestValidateTrailUpdateFieldsRejectsEmptyTitle(t *testing.T) {
	t.Parallel()
	if err := validateTrailUpdateFields(trailUpdateInputs{TitleChanged: true, Title: "   "}); err == nil {
		t.Fatal("expected empty title to be rejected")
	}
}

func TestTrailCreateAndUpdateRejectUnexpectedArgs(t *testing.T) {
	t.Parallel()
	for _, cmd := range []*cobra.Command{newTrailCreateCmd(), newTrailUpdateCmd()} {
		if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
			t.Fatalf("%s accepted an unexpected positional arg", cmd.Name())
		}
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
