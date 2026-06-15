package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/auth-go/sts"
	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trail"
)

const (
	trailListTestAuthorAlice = "alice"
	trailListTestAuthorBob   = "bob"
)

func TestRunTrailListAll_PrintsLoginHintWhenNotLoggedIn(t *testing.T) {
	// No t.Parallel: SetManagerForTest mutates package-level auth state.
	store := newAuthMemStore()
	mgr := newResolveTestManager(t, store, func(context.Context, sts.ExchangeRequest) (*tokens.TokenSet, error) {
		t.Fatal("exchange should not run when no core token is stored")
		return nil, errors.New("unreachable")
	})
	t.Cleanup(auth.SetManagerForTest(t, mgr))
	t.Cleanup(auth.SetResolveContextForAPIForTest(t, auth.DiscoveryUnavailableForTest))

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
	// No t.Parallel: SetManagerForTest mutates package-level auth state.
	store := newAuthMemStore()
	mgr := newResolveTestManager(t, store, func(context.Context, sts.ExchangeRequest) (*tokens.TokenSet, error) {
		t.Fatal("exchange should not run for invalid local options")
		return nil, errors.New("unreachable")
	})
	t.Cleanup(auth.SetManagerForTest(t, mgr))
	t.Cleanup(auth.SetResolveContextForAPIForTest(t, auth.DiscoveryUnavailableForTest))

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
