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

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trail"
	"github.com/entireio/cli/internal/entireclient/clusterdiscovery"
	"github.com/entireio/cli/internal/entireclient/contexts"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

const (
	trailListTestAuthorAlice = "alice"
	trailListTestAuthorBob   = "bob"
	trailTestRepoID          = "repo-id"
	// trailTestBasePath is the trails endpoint for the gh/acme/repo fixture.
	trailTestBasePath = "/api/v1/trails/gh/acme/repo"
	// trailTestListBody is the stand-in list-resource body used across the
	// resolveTrailUpdateBody fallback tests.
	trailTestListBody = "list body"
)

func initTrailCleanupRepo(t *testing.T) (localDir, originDir string, repo *git.Repository) {
	t.Helper()

	testutil.IsolateGitConfigEnv(t)
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
	prevTrailClient := newTrailAPIClient
	newTrailAPIClient = func(ctx context.Context, insecureHTTP bool, _, _, _ string) (*api.Client, string, error) {
		client, err := NewAuthenticatedAPIClient(ctx, insecureHTTP)
		return client, trailTestRepoID, err
	}
	t.Cleanup(func() { newTrailAPIClient = prevTrailClient })
	//
	// tokenstore.UseFileBackendForTesting mutate package-level state.
	//
	// Discovery selects a context whose keyring slot holds nothing, so the
	// per-context provider reports ErrNotLoggedIn.
	t.Setenv(api.BaseURLEnvVar, "https://entire.io")
	t.Cleanup(tokenstore.UseFileBackendForTesting(filepath.Join(t.TempDir(), "tokens.json")))
	c := &contexts.Context{Name: "me@core", CoreURL: "https://core.example", Handle: "me", KeychainService: "kc:me"}
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			return c, nil
		}))

	var out, errOut bytes.Buffer
	err := runTrailListAll(t.Context(), &out, &errOut, trailListOptions{Status: defaultTrailListStatus, Limit: defaultTrailListLimit})
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
	t.Setenv(api.BaseURLEnvVar, "https://entire.io")
	t.Cleanup(auth.SetResolveContextForAPIForTest(t,
		func(context.Context, string, string, string, *http.Client, clusterdiscovery.DebugFunc) (*contexts.Context, error) {
			t.Fatal("discovery should not run for invalid local options")
			return nil, errors.New("unreachable")
		}))

	opts := trailListOptions{Status: defaultTrailListStatus, Limit: 0}

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

func TestTrailRootPrintsHelp(t *testing.T) {
	t.Parallel()
	cmd := newTrailCmdForMode(true)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute trail root: %v", err)
	}
	text := out.String()
	for _, want := range []string{"A trail captures project intent", "--repo and --branch", "show", "list", "create", "link", "unlink"} {
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

func TestTrailRepoBasePath(t *testing.T) {
	t.Parallel()

	t.Run("GitHub keeps host-addressed route", func(t *testing.T) {
		t.Parallel()
		got, err := trailRepoBasePath("gh", "acme", "repo", "placement-id")
		require.NoError(t, err)
		require.Equal(t, "/api/v1/trails/gh/acme/repo", got)
	})

	t.Run("Entire-native uses repo ID route", func(t *testing.T) {
		t.Parallel()
		got, err := trailRepoBasePath("et", "acme", "repo", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
		require.NoError(t, err)
		require.Equal(t, "/api/v1/repos/01ARZ3NDEKTSV4RRFFQ69G5FAV/trails", got)
	})

	t.Run("Entire-native requires repo ID", func(t *testing.T) {
		t.Parallel()
		_, err := trailRepoBasePath("et", "acme", "repo", "")
		require.EqualError(t, err, "cannot access trails for an Entire-native repo without its repo ID")
	})
}

func TestTrailPathsFromNativeRepoBase(t *testing.T) {
	t.Parallel()
	const basePath = "/api/v1/repos/native-repo-id/trails"
	require.Equal(t, basePath+"/575", trailNumberPathForBase(basePath, 575))
}

func TestTrailNumberPath(t *testing.T) {
	t.Parallel()
	got := trailNumberPathForBase(trailsBasePath("gh", "acme", "repo"), 575)
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

func TestTrailDisplayURL(t *testing.T) {
	t.Setenv(api.BaseURLEnvVar, "https://entire.io")
	t.Setenv(api.WebBaseURLEnvVar, "")

	// Server URL wins, even when a number is present.
	got := trailDisplayURL(api.TrailResource{Number: 5, URL: "https://server/url"}, "gh", "acme", "repo")
	if got != "https://server/url" {
		t.Fatalf("expected server URL, got %q", got)
	}

	// Falls back to a constructed URL for older servers that omit it.
	// The web app lives on entire.com, not the entire.io API origin.
	got = trailDisplayURL(api.TrailResource{Number: 5}, "gh", "acme", "repo")
	if got != "https://entire.com/gh/acme/repo/trails/5" {
		t.Fatalf("expected constructed fallback URL on the web origin, got %q", got)
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

func TestDecodeTrailResourceReadsTheDirectResource(t *testing.T) {
	t.Parallel()
	// The detail route returns the resource itself; sibling keys are ignored.
	payload := `{"id":"trl_direct","number":7,"branch":"feat/direct","checkpoints":[],"has_write_permission":true}`
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(payload))}
	got, err := decodeTrailResource(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "trl_direct" || got.Number != 7 || got.Branch != "feat/direct" {
		t.Fatalf("decoded resource = %#v", got)
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

// Trail deletion was removed; the command must say so whichever legacy flags
// a script still passes.
func TestTrailDeleteRemoved(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{}, {"575"}, {"--force"}, {"--branch", "feat", "-f"}} {
		cmd := newTrailDeleteCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("trail delete %v: expected error, got nil", args)
		}
		if !errors.Is(err, errTrailDeleteRemoved) {
			t.Fatalf("trail delete %v: want the removal error, got: %v", args, err)
		}
	}
}

// TestParseTrailRepoShape_GitSuffixIsDroppedOnEveryForge pins that a bare
// triple drops `.git` whichever forge it names. Both forges reach a trails
// route — a mirror by forge/owner/repo, a native repo by ULID through
// trailRepoBasePath — so this is user-visible normalization: `--repo
// et/p/foo.git` and `--repo et/p/foo` name one repository.
func TestParseTrailRepoShape_GitSuffixIsDroppedOnEveryForge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		raw       string
		wantForge string
		wantOwner string
		wantRepo  string
	}{
		{name: "native drops the suffix", raw: "et/audit1/foo.git", wantForge: "et", wantOwner: "audit1", wantRepo: "foo"},
		{name: "native without a suffix", raw: "et/audit1/foo", wantForge: "et", wantOwner: "audit1", wantRepo: "foo"},
		{name: "mirror drops the suffix", raw: "gh/acme/app.git", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		// Case is not part of the suffix. This value is typically pasted
		// from a clone URL, and the server cuts the suffix with EqualFold,
		// so a case-sensitive drop here forwards "foo.GIT" as a repo
		// coordinate — a name the trails route cannot match.
		{name: "native drops an uppercase suffix", raw: "et/audit1/foo.GIT", wantForge: "et", wantOwner: "audit1", wantRepo: "foo"},
		{name: "mirror drops a mixed-case suffix", raw: "gh/acme/app.Git", wantForge: "gh", wantOwner: "acme", wantRepo: "app"},
		{name: "a longer dotted extension survives", raw: "gh/acme/app.gitignore", wantForge: "gh", wantOwner: "acme", wantRepo: "app.gitignore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forge, owner, repo, err := parseTrailRepoShape(tc.raw)
			require.NoError(t, err)
			require.Equal(t, tc.wantForge, forge)
			require.Equal(t, tc.wantOwner, owner)
			require.Equal(t, tc.wantRepo, repo)
		})
	}
}

// TestParseTrailRepoShape_RefusesNamesTheTrimManufactures pins that the segment
// check runs again AFTER the suffix is dropped. The emptiness check ahead of the
// trim sees the name as typed, so ".git" and "..git" both passed it and then
// became "" and "." — coordinates the trim invented, forwarded to a trails
// route. Neither forge is exempt.
func TestParseTrailRepoShape_RefusesNamesTheTrimManufactures(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"et/acme/.git",   // empties
		"et/acme/..git",  // becomes "."
		"et/acme/...git", // becomes ".."
		"gh/acme/.git",
		"gh/acme/..git",
		// The manufactured-name guard has to cover every case of the
		// suffix too, or the case-insensitive cut reopens exactly the hole
		// the case-sensitive one had closed.
		"et/acme/.GIT",
		"et/acme/..GIT",
		"gh/acme/..Git",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := parseTrailRepoShape(raw)
			require.Error(t, err)
		})
	}
}

// Not parallel: uses t.Chdir() to point ResolveRemoteRepo at a fake repo.
func TestResolveTrailRemote_RejectsUnsupportedForge(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@gitlab.com:acme/my-app.git")
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
func TestTrailEnablementCache_ReadsClonePreference(t *testing.T) {
	// Inline of the former trailsEnabledForRepo wrapper: resolves the current
	// repo's enablement scope and checks the cached enablement decision.
	trailsEnabledForCurrentRepo := func(ctx context.Context) bool {
		scope, err := currentTrailEnablementScope(ctx)
		if err != nil {
			return false
		}
		return cachedTrailsEnablementForScope(ctx, scope, time.Now()) == trailEnablementCacheEnabled
	}

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.RunGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/repo.git")
	t.Chdir(repoDir)
	ctx := context.Background()

	if trailsEnabledForCurrentRepo(ctx) {
		t.Fatal("expected trails disabled when cache is absent")
	}
	if err := saveTrailsEnabledForRepo(ctx, false); err != nil {
		t.Fatalf("save false cache: %v", err)
	}
	if trailsEnabledForCurrentRepo(ctx) {
		t.Fatal("expected trails disabled when cache is false")
	}
	if err := saveTrailsEnabledForRepo(ctx, true); err != nil {
		t.Fatalf("save true cache: %v", err)
	}
	if !trailsEnabledForCurrentRepo(ctx) {
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
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		*p = *prefs
		return nil
	}); err != nil {
		t.Fatalf("save auth-mismatched prefs: %v", err)
	}
	if trailsEnabledForCurrentRepo(ctx) {
		t.Fatal("expected trails disabled for mismatched auth cache scope")
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
	if trailsEnabledForCurrentRepo(ctx) {
		t.Fatal("expected trails disabled when cache is stale")
	}

	if err := saveTrailsEnabledForRemote(ctx, "gh", "other", "repo", true); err != nil {
		t.Fatalf("save mismatched cache: %v", err)
	}
	if trailsEnabledForCurrentRepo(ctx) {
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

func TestTrailListPageQueryEncodesFilters(t *testing.T) {
	t.Parallel()
	got := trailListPageQuery([]trail.Status{trail.StatusOpen, trail.StatusDraft}, 10, "")
	want := "?per_page=10&status%5Beq%5D=open%2Cdraft"
	if got != want {
		t.Fatalf("trailListPageQuery = %q, want %q", got, want)
	}
}

func TestTrailListPageQueryAnyStatusOmitsStatusParam(t *testing.T) {
	t.Parallel()
	got := trailListPageQuery(nil, 10, "")
	if got != "?per_page=10" {
		t.Fatalf("trailListPageQuery = %q, want %q", got, "?per_page=10")
	}
}

func TestTrailListPageQueryCapsPageSizeAtServerMax(t *testing.T) {
	t.Parallel()
	got := trailListPageQuery(nil, 5000, "")
	if !strings.Contains(got, "per_page=100") {
		t.Fatalf("expected per_page capped at 100, got %q", got)
	}
}

func TestListTrailResourcesRejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		_, _, err := listTrailResources(t.Context(), nil, trailsBasePath("gh", "acme", "repo"), nil, "", limit)
		if err == nil || err.Error() != "limit must be greater than 0" {
			t.Fatalf("limit %d error = %v, want limit validation error", limit, err)
		}
	}
}

// A --limit above entire-api's page cap is satisfied by pagination, so the list
// must not warn about a server-side cap the way the retired backend did.
func TestRunTrailListAllPrintsNoServerLimitNote(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"items":[{"id":"trl_1","number":1,"branch":"feature/x","original_branch":"feature/former","status":"open"}],"total_count":1033}`)
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	var out bytes.Buffer
	err := runTrailListAllWithClient(t.Context(), &out, client, "", trailListOptions{
		Repo: "gh/acme/repo", Limit: 500,
	}, []trail.Status{trail.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "feature/x") {
		t.Fatalf("trail missing from output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "exceeds the server maximum") {
		t.Fatalf("unexpected server-limit note:\n%s", out.String())
	}
	if strings.Contains(out.String(), "feature/former") {
		t.Fatalf("human output contains original branch:\n%s", out.String())
	}
}

func TestRunTrailListAllWithClientNativeUsesRepoIDRoute(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = fmt.Fprint(w, `{"items":[],"total_count":0}`)
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	err := runTrailListAllWithClient(t.Context(), io.Discard, client, "01ARZ3NDEKTSV4RRFFQ69G5FAV", trailListOptions{
		Repo: "entire://aws-us-east-2.entire.io/et/entirehq/marvin", Limit: 10,
	}, []trail.Status{trail.StatusOpen})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/repos/01ARZ3NDEKTSV4RRFFQ69G5FAV/trails", gotPath)
}

func TestListTrailResourcesStopsWhenAuthorLimitIsSatisfied(t *testing.T) {
	t.Parallel()
	requests := 0
	login := "alice"
	next := "should-not-be-requested"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 1 {
			t.Errorf("unexpected extra page request with token %q", r.URL.Query().Get("cursor"))
		}
		trails := make([]api.TrailResource, trailListServerMaxLimit)
		for i := range trails {
			trails[i] = api.TrailResource{
				ID:     "trl_" + strconv.Itoa(i),
				Number: i + 1,
				Author: &trail.Author{Login: &login},
			}
		}
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{
			Trails: trails, NextCursor: &next, Total: 10_000,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()
	client := api.NewClientWithBaseURL("tok", srv.URL)

	items, total, err := listTrailResources(t.Context(), client, trailsBasePath("gh", "acme", "repo"), nil, login, 5)
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

// A continuation carries only cursor+per_page: the opaque cursor holds the
// active filters (RFD-026 §8) and the server restores them each page, so
// repeating status here would be redundant (and a conflicting respelling is
// rejected with 400 by cursorFiltersMatch cell-side).
func TestTrailListPageQueryContinuationOmitsFiltersTheCursorCarries(t *testing.T) {
	t.Parallel()
	got := trailListPageQuery([]trail.Status{trail.StatusOpen}, 100, "next page")
	want := "?cursor=next+page&per_page=100"
	if got != want {
		t.Fatalf("trailListPageQuery = %q, want %q", got, want)
	}
}

func TestFindTrailByNumberUsesDirectEntireAPIRoute(t *testing.T) {
	t.Parallel()
	const trailNumber = 1201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/api/v1/trails/gh/acme/repo/1201"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf("query = %q, want empty", r.URL.RawQuery)
		}
		if err := json.NewEncoder(w).Encode(api.TrailResource{ID: "trl_old", Number: trailNumber, Branch: "old/trail"}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByNumberAtPath(t.Context(), client, trailsBasePath("gh", "acme", "repo"), trailNumber)
	if err != nil {
		t.Fatalf("findTrailByNumber: %v", err)
	}
	if found == nil || found.ID != "trl_old" {
		t.Fatalf("found = %#v, want trl_old", found)
	}
}

func TestFindTrailByNumberAtPathUsesNativeRepoIDRoute(t *testing.T) {
	t.Parallel()
	const (
		basePath    = "/api/v1/repos/native-repo-id/trails"
		trailNumber = 3
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != basePath+"/3" || r.URL.RawQuery != "" {
			t.Errorf("request = %s?%s, want %s/3 with no query", r.URL.Path, r.URL.RawQuery, basePath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(api.TrailResource{ID: "native-trail", Number: trailNumber}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	found, err := findTrailByNumberAtPath(t.Context(), api.NewClientWithBaseURL("tok", srv.URL), basePath, trailNumber)
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Equal(t, "native-trail", found.ID)
}

// A 2xx whose body carries no trail identity is "not found", not a trail whose
// every field is zero. Selector callers act on `found != nil`, so returning a
// phantom would make `trail show <number>` render an empty trail instead of
// reporting the miss.
func TestFindTrailByNumberTreatsIdentitylessBodyAsNotFound(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, body string }{
		{name: "empty object", body: `{}`},
		{name: "error body with a 200", body: `{"error":"not found"}`},
		{name: "list response on the number route", body: `{"items":[{"id":"trl_a","number":1}],"total_count":1}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(w, tt.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer srv.Close()

			found, err := findTrailByNumberAtPath(t.Context(), api.NewClientWithBaseURL("tok", srv.URL), trailsBasePath("gh", "acme", "repo"), 575)
			if err != nil {
				t.Fatalf("findTrailByNumber: %v", err)
			}
			if found != nil {
				t.Fatalf("found = %#v, want nil (not a zero-valued trail)", found)
			}
		})
	}
}

func TestFindTrailPaginatesPastServerMax(t *testing.T) {
	t.Parallel()
	const nextPage = "next-page"
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("cursor")
		tokens = append(tokens, token)
		trails := []api.TrailResource{}
		var next *string
		switch token {
		case "":
			trails = make([]api.TrailResource, trailListServerMaxLimit)
			for i := range trails {
				trails[i] = api.TrailResource{ID: "trl_first_" + strconv.Itoa(i), Number: i + 1, Branch: "old/" + strconv.Itoa(i)}
			}
			n := nextPage
			next = &n
		case nextPage:
			trails = []api.TrailResource{{ID: "trl_target", Number: 201, Branch: "target"}}
		}
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: trails, Total: trailListServerMaxLimit + 1, NextCursor: next}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranchAtPath(context.Background(), client, trailsBasePath("gh", "acme", "repo"), "target")
	if err != nil {
		t.Fatalf("findTrailByBranch: %v", err)
	}
	if found == nil || found.ID != "trl_target" {
		t.Fatalf("found = %#v, want trl_target", found)
	}
	if len(tokens) != 2 || tokens[0] != "" || tokens[1] != nextPage {
		t.Fatalf("page tokens = %v, want [\"\" next-page]", tokens)
	}
}

// A trail on the very last page of the search budget must still be found — the
// page loop has to reach trailFindMaxPages, not stop one short of it.
func TestFindTrailPaginatesToTheEndOfItsBudget(t *testing.T) {
	t.Parallel()
	const targetPage = trailFindMaxPages
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page := int(atomic.AddInt32(&requests, 1))
		response := api.TrailListResponse{}
		if page == targetPage {
			response.Trails = []api.TrailResource{{ID: "trl_target", Number: 1001, Branch: "target"}}
		} else {
			response.Trails = make([]api.TrailResource, trailListServerMaxLimit)
			for i := range response.Trails {
				number := (page-1)*trailListServerMaxLimit + i + 1
				response.Trails[i] = api.TrailResource{ID: "trl_" + strconv.Itoa(number), Number: number, Branch: "old/" + strconv.Itoa(number)}
			}
			next := "page-" + strconv.Itoa(page+1)
			response.NextCursor = &next
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranchAtPath(t.Context(), client, trailsBasePath("gh", "acme", "repo"), "target")
	if err != nil {
		t.Fatalf("findTrailByBranch: %v", err)
	}
	if found == nil || found.ID != "trl_target" {
		t.Fatalf("found = %#v, want trl_target", found)
	}
	if got := atomic.LoadInt32(&requests); got != targetPage {
		t.Fatalf("requests = %d, want %d", got, targetPage)
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
		next := "same-page"
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: trails, NextCursor: &next}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranchAtPath(context.Background(), client, trailsBasePath("gh", "acme", "repo"), "target")
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
		next := "page-" + strconv.Itoa(requestNumber)
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: trails, NextCursor: &next}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := findTrailByBranchAtPath(context.Background(), client, trailsBasePath("gh", "acme", "repo"), "target")
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
	}, "", nil, "")

	if text := out.String(); strings.Contains(text, "Phase:") {
		t.Fatalf("expected whitespace phase to be omitted, got:\n%s", text)
	}
}

func TestPrintTrailDetailsRendersURLAndDescription(t *testing.T) {
	t.Parallel()
	m := &trail.Metadata{Title: "T", Branch: "feat/a", Base: "main", Status: trail.StatusOpen}

	var out bytes.Buffer
	printTrailDetails(&out, m, "https://entire.io/gh/acme/repo/trails/5", nil, "line one\nline two")
	text := out.String()
	if !strings.Contains(text, "URL:") || !strings.Contains(text, "https://entire.io/gh/acme/repo/trails/5") {
		t.Fatalf("expected a URL line, got:\n%s", text)
	}
	if !strings.Contains(text, "Description:") || !strings.Contains(text, "line one\nline two") {
		t.Fatalf("expected a Description block, got:\n%s", text)
	}

	// Empty URL and whitespace-only body are omitted.
	out.Reset()
	printTrailDetails(&out, m, "", nil, "   ")
	if text := out.String(); strings.Contains(text, "URL:") || strings.Contains(text, "Description:") || strings.Contains(text, "Project trail:") {
		t.Fatalf("expected URL/Description/Project trail omitted for empty values, got:\n%s", text)
	}

	// A Change prints its parent's page, the link agents otherwise guess.
	// Both links are server text, so control characters never reach the terminal.
	out.Reset()
	m.ProjectTrailURL = "https://entire.io/gh/acme/-/trails/42\x1b[31m"
	printTrailDetails(&out, m, "https://entire.io/gh/acme/repo/trails/5\x1b[0m", nil, "")
	if text := out.String(); !strings.Contains(text, "Project trail: https://entire.io/gh/acme/-/trails/42") || strings.Contains(text, "\x1b") {
		t.Fatalf("expected sanitized links, got:\n%q", text)
	}
}

// trailShowTestServer serves the two endpoints `trail show` reads: the list
// (which resolves a non-numeric selector and omits the description) and the
// detail (which carries body_document). detailStatus > 0 makes the detail fetch
// fail so the best-effort path can be exercised — pass a branch selector with
// it, because a numeric selector resolves through the detail route itself and
// would fail outright rather than degrade.
func trailShowTestServer(t *testing.T, resource api.TrailResource, detailSnapshot string, detailStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == trailTestBasePath:
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{resource}, Total: 1}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case r.Method == http.MethodGet && r.URL.Path == trailTestBasePath+"/"+strconv.Itoa(resource.Number):
			if detailStatus > 0 {
				w.WriteHeader(detailStatus)
				if err := json.NewEncoder(w).Encode(map[string]string{"error": "boom"}); err != nil {
					t.Errorf("encode detail error: %v", err)
				}
				return
			}
			detail := resource
			detail.BodyDocument = &api.TrailBodyDocument{TextSnapshot: detailSnapshot}
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

func TestRunTrailListAndShowJSONPreserveBranchState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		branch         string
		originalBranch string
	}{
		{name: "linked", branch: "feature/current", originalBranch: "feature/original"},
		{name: "unlinked", originalBranch: "feature/former"},
		{name: "empty original branch", branch: "feature/current"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resource := api.TrailResource{
				ID:             "trl_1",
				Number:         7,
				Branch:         tt.branch,
				OriginalBranch: tt.originalBranch,
				Title:          "Branch state",
				Status:         string(trail.StatusOpen),
			}
			srv := trailShowTestServer(t, resource, "", 0)
			client := api.NewClientWithBaseURL("tok", srv.URL)

			var listOut bytes.Buffer
			err := runTrailListAllWithClient(t.Context(), &listOut, client, "", trailListOptions{
				Repo: "gh/acme/repo", Limit: 10, JSON: true,
			}, nil)
			require.NoError(t, err)

			var showOut, showErr bytes.Buffer
			err = runTrailShowWithClientAtPath(t.Context(), &showOut, &showErr, client, trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: "7", JSON: true})
			require.NoError(t, err)
			require.Empty(t, showErr.String())

			type branchState struct {
				Branch         string  `json:"branch"`
				OriginalBranch *string `json:"original_branch"`
			}
			var listed []branchState
			require.NoError(t, json.Unmarshal(listOut.Bytes(), &listed))
			require.Len(t, listed, 1)
			var shown branchState
			require.NoError(t, json.Unmarshal(showOut.Bytes(), &shown))

			for output, got := range map[string]branchState{"list": listed[0], "show": shown} {
				require.Equal(t, tt.branch, got.Branch, "%s branch", output)
				require.NotNil(t, got.OriginalBranch, "%s original_branch must be present", output)
				require.Equal(t, tt.originalBranch, *got.OriginalBranch, "%s original_branch", output)
			}
		})
	}
}

func TestRunTrailShowJSONEmitsOneTrailObject(t *testing.T) {
	t.Parallel()

	alice := trailListTestAuthorAlice
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	resource := api.TrailResource{
		ID:        "trl_1",
		Number:    7,
		URL:       "https://entire.io/gh/acme/repo/trails/7",
		Branch:    "feature/x",
		Base:      "main",
		Title:     "Shown trail",
		Status:    string(trail.StatusOpen),
		Phase:     "building",
		Author:    &trail.Author{ID: "u1", Login: &alice},
		Labels:    []string{"cli"},
		Type:      string(trail.TypeBug),
		Priority:  string(trail.PriorityHigh),
		Reviewers: []trail.Reviewer{{Login: "rev1", Status: trail.ReviewerApproved}},
		CreatedAt: created,
		UpdatedAt: created,
	}
	srv := trailShowTestServer(t, resource, "detail body", 0)

	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: "7", JSON: true})

	require.NoError(t, err)
	require.Empty(t, errOut.String())

	var got trail.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got), "output must be a single JSON object: %s", out.String())
	require.Equal(t, 7, got.Number)
	require.Equal(t, trail.ID("trl_1"), got.TrailID)
	require.Equal(t, "https://entire.io/gh/acme/repo/trails/7", got.URL)
	require.Equal(t, "feature/x", got.Branch)
	require.Equal(t, "main", got.Base)
	require.Equal(t, "Shown trail", got.Title)
	require.Equal(t, trail.StatusOpen, got.Status)
	require.Equal(t, "building", got.Phase)
	require.Equal(t, alice, got.AuthorLogin())
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out.Bytes(), &fields))
	for _, retired := range []string{"assignees", "comment_count", "unresolved_count"} {
		require.NotContains(t, fields, retired)
	}
	require.Equal(t, []string{"cli"}, got.Labels)
	require.Equal(t, trail.TypeBug, got.Type)
	require.Equal(t, trail.PriorityHigh, got.Priority)
	require.Equal(t, []trail.Reviewer{{Login: "rev1", Status: trail.ReviewerApproved}}, got.Reviewers)
	// The description lives on the detail endpoint only; JSON must carry it, not
	// the empty list body.
	require.Equal(t, "detail body", got.Body)
	// Human-only decoration must not leak into the data.
	require.NotContains(t, out.String(), noTrailDescription)
	require.NotContains(t, out.String(), "Trail: ")
}

func TestRunTrailShowWithClientAtPathUsesNativeRepoIDRoutes(t *testing.T) {
	t.Parallel()
	const basePath = "/api/v1/repos/native-repo-id/trails"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case basePath:
			if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{
				ID: "native-trail", Number: 3, Branch: "feature/native", Title: "Native trail",
			}}}); err != nil {
				t.Errorf("encode list response: %v", err)
			}
		case basePath + "/3":
			if err := json.NewEncoder(w).Encode(api.TrailResource{
				ID: "native-trail", Number: 3, Branch: "feature/native",
				BodyDocument: &api.TrailBodyDocument{TextSnapshot: "native body"},
			}); err != nil {
				t.Errorf("encode detail response: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), basePath,
		"et", "entirehq", "marvin", trailShowOptions{Selector: "feature/native", JSON: true})
	require.NoError(t, err)
	require.Empty(t, errOut.String())
	require.Equal(t, []string{basePath, basePath + "/3"}, paths)
}

// A numeric selector resolves through the detail route, which already returns
// body_document — so `trail show <number>` must hit that URL once, not twice.
func TestRunTrailShowNumericSelectorFetchesDetailOnce(t *testing.T) {
	t.Parallel()

	const number = 7
	detailPath := trailTestBasePath + "/" + strconv.Itoa(number)
	var detailHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != detailPath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&detailHits, 1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(api.TrailResource{
			ID: "trl_1", Number: number, Branch: "feature/x", Title: "T",
			Status:       string(trail.StatusOpen),
			BodyDocument: &api.TrailBodyDocument{TextSnapshot: "detail body"},
		}); err != nil {
			t.Errorf("encode detail response: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: "7", JSON: true})

	require.NoError(t, err)
	require.Empty(t, errOut.String())
	require.EqualValues(t, 1, atomic.LoadInt32(&detailHits), "the detail route must be requested exactly once")

	var got trail.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Equal(t, "detail body", got.Body, "the description must come from the resolved resource")
}

func TestRunTrailShowJSONLeavesBodyEmptyWithoutDescription(t *testing.T) {
	t.Parallel()

	srv := trailShowTestServer(t, api.TrailResource{ID: "trl_1", Number: 7, Branch: "feature/x", Status: string(trail.StatusOpen)}, "", 0)

	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: "7", JSON: true})

	require.NoError(t, err)
	require.Empty(t, errOut.String())

	var got trail.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	require.Empty(t, got.Body, "an empty description must stay empty in JSON, not become the display placeholder")
	require.NotContains(t, out.String(), noTrailDescription)
}

// A failed description fetch is best-effort: the warning belongs on stderr so
// stdout stays parseable.
func TestRunTrailShowJSONKeepsStdoutParseableWhenDescriptionFetchFails(t *testing.T) {
	t.Parallel()

	srv := trailShowTestServer(t, api.TrailResource{ID: "trl_1", Number: 7, Branch: "feature/x", Title: "T", Body: trailTestListBody, Status: string(trail.StatusOpen)}, "", http.StatusInternalServerError)

	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: "feature/x", JSON: true})

	require.NoError(t, err)
	require.Contains(t, errOut.String(), "could not load trail detail")

	var got trail.Metadata
	require.NoError(t, json.Unmarshal(out.Bytes(), &got), "stdout must stay valid JSON: %s", out.String())
	require.Equal(t, trailTestListBody, got.Body, "the list body is the fallback when the detail fetch fails")
}

func TestRunTrailShowTextStillRendersTheHumanView(t *testing.T) {
	t.Parallel()

	srv := trailShowTestServer(t, api.TrailResource{
		ID: "trl_1", Number: 7, URL: "https://entire.io/gh/acme/repo/trails/7",
		Branch: "feature/x", OriginalBranch: "feature/former", Base: "main", Title: "Shown trail", Status: string(trail.StatusOpen),
	}, "detail body", 0)

	var out, errOut bytes.Buffer
	err := runTrailShowWithClientAtPath(t.Context(), &out, &errOut, api.NewClientWithBaseURL("tok", srv.URL), trailTestBasePath, "gh", "acme", "repo", trailShowOptions{Selector: "7"})

	require.NoError(t, err)
	require.Empty(t, errOut.String())
	text := out.String()
	for _, want := range []string{"Trail: Shown trail", "Number:", "feature/x", "https://entire.io/gh/acme/repo/trails/7", "Description:", "detail body"} {
		require.Containsf(t, text, want, "text output missing %q:\n%s", want, text)
	}
	require.NotContains(t, text, "feature/former")
}

func TestTrailShowCmdHasJSONFlag(t *testing.T) {
	t.Parallel()
	require.NotNil(t, newTrailShowCmd().Flags().Lookup("json"), "trail show must offer --json so agents can read it non-interactively")
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

func TestPrintTrailDetailsShowsTypePriorityReviewers(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTrailDetails(&out, &trail.Metadata{
		Title:     "T",
		Branch:    "b",
		Base:      "main",
		Status:    trail.StatusOpen,
		Type:      trail.TypeBug,
		Priority:  trail.PriorityHigh,
		Reviewers: []trail.Reviewer{{Login: "rev1", Status: trail.ReviewerApproved}},
	}, "", nil, "")
	s := out.String()
	for _, want := range []string{"Type:", "bug", "Priority:", "high", "Reviewers:", "rev1", "approved"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

// TestResolveTrailPushRemote covers the precedence a trail branch's delivery
// follows. The tiers are git's own, so a repo whose branch pushes somewhere other
// than "origin" (a fork workflow, remote.pushDefault) gets its branch delivered
// there instead of silently to whatever "origin" happens to be.
func TestResolveTrailPushRemote(t *testing.T) {
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
			// `trail create --branch other` while on feature/x must not inherit
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
				runGitTrailTest(t, dir, "config", kv[0], kv[1])
			}
			t.Chdir(dir)

			got, err := resolveTrailPushRemote(context.Background(), tc.branch)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// writeTrailCreatePrePushHook installs script as localDir's pre-push hook. The
// two hook tests differ only in whether the hook accepts the push.
func writeTrailCreatePrePushHook(t *testing.T, localDir, script string) {
	t.Helper()
	hooksDir := filepath.Join(localDir, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "pre-push"), []byte(script), 0o755))
}
