package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trail"
)

const (
	trailListTestAuthorAlice = "alice"
	trailListTestAuthorBob   = "bob"
)

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

func TestLimitTrailsKeepsMostRecentPrefix(t *testing.T) {
	t.Parallel()
	trails := []*trail.Metadata{
		{Branch: "newest"},
		{Branch: "middle"},
		{Branch: "oldest"},
	}

	got := limitTrails(trails, 2)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Branch != "newest" || got[1].Branch != "middle" {
		t.Fatalf("got branches %q, %q; want newest, middle", got[0].Branch, got[1].Branch)
	}

	if all := limitTrails(trails, 3); len(all) != len(trails) {
		t.Fatalf("limit 3 len = %d, want %d", len(all), len(trails))
	}
}

func TestFilterTrailsByAuthor(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	bob := trailListTestAuthorBob
	trails := []*trail.Metadata{
		{Branch: "mine-1", Author: &trail.Author{Login: &alice}},
		{Branch: "theirs", Author: &trail.Author{Login: &bob}},
		{Branch: "unknown"},
		{Branch: "mine-2", Author: &trail.Author{Login: &alice}},
	}

	got := filterTrailsByAuthor(trails, trailListTestAuthorAlice)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Branch != "mine-1" || got[1].Branch != "mine-2" {
		t.Fatalf("got branches %q, %q; want mine-1, mine-2", got[0].Branch, got[1].Branch)
	}
}

func TestFilterTrailsByAuthorIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	mixed := "Alice"
	trails := []*trail.Metadata{
		{Branch: "mine", Author: &trail.Author{Login: &mixed}},
	}

	got := filterTrailsByAuthor(trails, "alice")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (case-insensitive)", len(got))
	}
}

func TestParseTrailStatusFilterAcceptsCommaSeparatedStatuses(t *testing.T) {
	t.Parallel()
	got, err := parseTrailStatusFilter("in_progress, open,closed")
	if err != nil {
		t.Fatalf("parseTrailStatusFilter: %v", err)
	}
	want := []trail.Status{trail.StatusInProgress, trail.StatusOpen, trail.StatusClosed}
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
	if _, err := parseTrailStatusFilter("in_progress,nope"); err == nil {
		t.Fatal("expected invalid status error")
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
			Status:    trail.StatusInProgress,
			Author:    &trail.Author{Login: &alice},
			UpdatedAt: time.Now(),
		},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   []trail.Status{trail.StatusInProgress},
	})

	text := out.String()
	for _, want := range []string{"In progress · 1 trail", "feat/repo-wide", trailListTestAuthorAlice} {
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
			Status:    trail.StatusInProgress,
			Author:    &trail.Author{Login: &alice},
			UpdatedAt: time.Now().Add(-24 * time.Hour),
		},
	}, trailListDisplayOptions{
		RequestedAuthor: trailListTestAuthorAlice,
		StatusFilters:   []trail.Status{trail.StatusInProgress},
	})

	text := out.String()
	if !strings.Contains(text, "alice · 1 in progress") {
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
			Status:    trail.StatusInProgress,
			Author:    &trail.Author{Login: &mixedCase},
			UpdatedAt: time.Now(),
		},
	}, trailListDisplayOptions{
		RequestedAuthor: "alice",
		CurrentUser:     "alice",
		StatusFilters:   []trail.Status{trail.StatusInProgress},
	})

	text := out.String()
	if !strings.Contains(text, "Your trails (alice) · 1 in progress") {
		t.Fatalf("expected 'Your trails (alice)' header, got:\n%s", text)
	}
}

func TestPrintTrailListAnyAuthorAnyStatusGroupsByStatus(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	bob := trailListTestAuthorBob
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/a", Status: trail.StatusInProgress, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
		{Branch: "fix/b", Status: trail.StatusOpen, Author: &trail.Author{Login: &bob}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
	})

	text := out.String()
	if strings.Index(text, "In progress · 1") > strings.Index(text, "Open · 1") {
		t.Fatalf("expected in-progress group before open group, got:\n%s", text)
	}
	for _, want := range []string{"Recent trails · 2", "In progress · 1", "Open · 1", "feat/a", trailListTestAuthorAlice, "fix/b", trailListTestAuthorBob} {
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
		{Branch: "feat/a", Status: trail.StatusInProgress, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
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

func TestPrintTrailListUnknownStatusGroupedInOtherBucket(t *testing.T) {
	t.Parallel()
	alice := trailListTestAuthorAlice
	unknownStatus := trail.Status("experimental_review")
	var out bytes.Buffer
	printTrailList(&out, []*trail.Metadata{
		{Branch: "feat/known", Status: trail.StatusInProgress, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
		{Branch: "feat/odd", Status: unknownStatus, Author: &trail.Author{Login: &alice}, UpdatedAt: time.Now()},
	}, trailListDisplayOptions{
		RequestedAuthor: "",
		StatusFilters:   nil,
	})

	text := out.String()
	for _, want := range []string{"Recent trails · 2", "In progress · 1", "Other · 1", "feat/odd"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q, got:\n%s", want, text)
		}
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
