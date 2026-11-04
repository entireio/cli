package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// setupGuardRepo checks out feature/mine in a clone of gh/acme/widget.
func setupGuardRepo(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "x")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	testutil.RunGit(t, dir, "checkout", "-b", "feature/mine")
	t.Chdir(dir)
}

// serveGuardProject serves project trail #42 (someone else's: its only change
// is feature/a in repo-a) by number and ID, and records PATCHes.
func serveGuardProject(t *testing.T, patched *int) {
	t.Helper()
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			*patched++
		}
		w.Header().Set("ETag", `W/"v1"`)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
}

// serveBranchWork answers the branch lookup in gh/acme/widget with work (none
// when nil), or fails it with lookupErr.
func serveBranchWork(t *testing.T, work *api.TrailResource, lookupErr error) {
	t.Helper()
	if lookupErr != nil {
		newTrailAPIClient = func(_ context.Context, _ bool, _, _, _ string) (*api.Client, string, error) {
			return nil, "", lookupErr
		}
		return
	}
	serveBranchLookup(t, func(w http.ResponseWriter, _ *http.Request) {
		list := api.TrailListResponse{}
		if work != nil {
			list.Trails, list.Total = []api.TrailResource{*work}, 1
		}
		assert.NoError(t, json.NewEncoder(w).Encode(list))
	})
}

// serveBranchLookup answers the branch lookup in gh/acme/widget with handler.
func serveBranchLookup(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails/gh/acme/widget", r.URL.Path)
		handler(w, r)
	})
}

func branchWorkOn(parentNumber, changeNumber int, parentID string) *api.TrailResource {
	return &api.TrailResource{ID: "01ARZ3NDEKTSV4RRFFQ69G5FBB", Number: changeNumber, Branch: "feature/mine", Status: "open",
		Parent: &api.TrailParentReference{ID: parentID, Number: parentNumber, Host: "gh", Project: "acme"}}
}

// A bare number names a project trail, which can belong to someone else: a
// change number (cli/1744) or pull request number typed as `update 1744`
// reached unrelated project trail #1744 and replaced its description. The
// update now refuses when this branch's work is on a different trail.
func TestProjectTrailUpdateBareNumberRefusesAnotherTrail(t *testing.T) {
	setupGuardRepo(t)
	var patched int
	serveGuardProject(t, &patched)
	serveBranchWork(t, branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC"), nil)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "my change's description")

	require.Error(t, err)
	require.Zero(t, patched, "no PATCH may reach the other trail")
	for _, want := range []string{
		`refusing to update trail #42 "Project intent": branch "feature/mine" is work on trail #43, not on #42`,
		"To update this branch's trail, rerun without 42, keeping your other flags.",
		"To update #42 anyway, rerun with " + projectTrailTestID + " --project gh/acme in place of 42, keeping your other flags.",
	} {
		require.Contains(t, err.Error(), want)
	}
}

func TestProjectTrailUpdateBareNumberNamesTheChangeItLooksLike(t *testing.T) {
	setupGuardRepo(t)
	var patched int
	serveGuardProject(t, &patched)
	serveBranchWork(t, branchWorkOn(43, 42, "01ARZ3NDEKTSV4RRFFQ69G5FCC"), nil)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--title", "x")

	require.ErrorContains(t, err, "42 is this branch's change number, and a bare number names a project trail. For the change, rerun with widget/42 in place of 42")
	require.Zero(t, patched)
}

func TestProjectTrailUpdateBareNumberPassesWhenItCannotMisfire(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		work *api.TrailResource
	}{
		{name: "branch work is on this trail", args: []string{"update", "42", "--body", "x"},
			work: branchWorkOn(42, 7, projectTrailTestID)},
		{name: "branch has no trail", args: []string{"update", "42", "--body", "x"}},
		{name: "ULID names the trail exactly", args: []string{"update", projectTrailTestID, "--project", "gh/acme", "--body", "x"},
			work: branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC")},
		{name: "--repo names this clone's repository, work on this trail", args: []string{"update", "42", "--repo", "gh/acme/widget", "--body", "x"},
			work: branchWorkOn(42, 7, projectTrailTestID)},
		{name: "another project", args: []string{"update", "42", "--project", "gh/other", "--body", "x"},
			work: branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			var patched int
			serveGuardProject(t, &patched)
			serveBranchWork(t, tt.work, nil)

			_, _, err := executeProjectTrailTest(t, tt.args...)

			require.NoError(t, err)
			require.Equal(t, 1, patched)
		})
	}
}

// The guard fails closed: when it cannot establish which trail the branch's
// work is on, a bare number could still name someone else's trail, so the
// update refuses and names the ULID rerun.
func TestProjectTrailUpdateBareNumberRefusesWhenItCannotCheck(t *testing.T) {
	anyway := "To update #42 anyway, rerun with " + projectTrailTestID + " --project gh/acme in place of 42, keeping your other flags."
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T)
		want  []string
	}{
		{name: "lookup fails", setup: func(t *testing.T) {
			t.Helper()
			serveBranchWork(t, nil, errors.New("repository not onboarded"))
		}, want: []string{`branch "feature/mine" could not check which trail it belongs to (repository not onboarded)`}},
		{name: "lookup error text is sanitized to one line", setup: func(t *testing.T) {
			t.Helper()
			serveBranchWork(t, nil, errors.New("boom\x1b]0;title\x07\x1b[2J\nsecond\tline"))
		}, want: []string{"(boom second line)"}},
		{name: "work has no project parent", setup: func(t *testing.T) {
			t.Helper()
			serveBranchWork(t, &api.TrailResource{ID: "01ARZ3NDEKTSV4RRFFQ69G5FBB", Number: 7, Branch: "feature/mine", Status: "open"}, nil)
		}, want: []string{`branch "feature/mine" has trail work whose project trail the API did not return`}},
		{name: "lookup times out", setup: func(t *testing.T) {
			t.Helper()
			old := projectTrailUpdateGuardTimeout
			projectTrailUpdateGuardTimeout = 50 * time.Millisecond
			t.Cleanup(func() { projectTrailUpdateGuardTimeout = old })
			serveBranchLookup(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		}, want: []string{"could not check which trail it belongs to", "deadline exceeded"}},
		{name: "detached HEAD", setup: func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "checkout", "--detach")
			serveBranchLookup(t, func(http.ResponseWriter, *http.Request) { t.Error("detached HEAD must not look up branch work") })
		}, want: []string{"HEAD is not on a branch", "rerun with --branch <branch> added"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			var patched int
			serveGuardProject(t, &patched)
			tt.setup(t)

			_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

			require.Error(t, err)
			require.Zero(t, patched, "no PATCH may reach a trail the guard could not clear")
			msg := err.Error()
			require.Contains(t, msg, `refusing to update trail #42 "Project intent"`)
			require.Contains(t, msg, anyway)
			require.NotContains(t, msg, "rerun without 42", "the no-selector rerun depends on the same unknown")
			require.NotContains(t, msg, "\x1b")
			require.NotContains(t, msg, "\x07")
			for _, want := range tt.want {
				require.Contains(t, msg, want)
			}
		})
	}
}

// --repo naming this clone's repository still targets #42 by bare number, so
// the guard runs: --repo does not switch it off.
func TestProjectTrailUpdateBareNumberWithThisRepoKeepsTheGuard(t *testing.T) {
	setupGuardRepo(t)
	var patched int
	serveGuardProject(t, &patched)
	serveBranchWork(t, branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC"), nil)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--repo", "gh/acme/widget", "--body", "x")

	require.ErrorContains(t, err, `branch "feature/mine" is work on trail #43, not on #42`)
	require.Zero(t, patched)
}

// --repo naming another repository addresses work unrelated to this clone's
// branch, so the branch is not consulted.
func TestProjectTrailUpdateBareNumberWithAnotherRepoSkipsTheGuard(t *testing.T) {
	setupGuardRepo(t)
	var patched int
	serveGuardProject(t, &patched)
	serveBranchLookup(t, func(http.ResponseWriter, *http.Request) {
		t.Error("--repo for another repository must not look up this branch")
	})

	_, _, err := executeProjectTrailTest(t, "update", "42", "--repo", "gh/acme/other", "--body", "x")

	require.NoError(t, err)
	require.Equal(t, 1, patched)
}

func TestScanTrailPagesReportsTruncation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		pages        int
		wantComplete bool
	}{
		{name: "last page reached", pages: 2, wantComplete: true},
		{name: "stopped at the page budget", pages: trailFindMaxPages + 5, wantComplete: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var page int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				page++
				resp := api.TrailListResponse{Trails: []api.TrailResource{{ID: fmt.Sprintf("t%d", page), Branch: "other"}}}
				if page < tt.pages {
					next := fmt.Sprintf("cursor-%d", page)
					resp.NextCursor = &next
				}
				assert.NoError(t, json.NewEncoder(w).Encode(resp))
			}))
			t.Cleanup(server.Close)
			client := api.NewClientWithBaseURL("token", server.URL)

			found, complete, err := scanTrailPages(t.Context(), client, trailsBasePath("gh", "acme", "repo"),
				func(tr api.TrailResource) bool { return tr.Branch == "target" })

			require.NoError(t, err)
			require.Nil(t, found)
			require.Equal(t, tt.wantComplete, complete)
		})
	}
}

func TestProjectTrailNumberMismatchSanitizesServerText(t *testing.T) {
	t.Parallel()
	current := api.ProjectTrail{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV\x1b]0;x\x07", Title: "T\x1b[2J"}
	g := projectTrailNumberGuard{number: 42, current: current, target: &projectTrailTarget{Host: "gh", Project: "acme"}, repo: "widget", branch: "feature/mine"}
	msg := g.mismatch(branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC"))
	require.NotContains(t, msg, "\x1b")
	require.NotContains(t, msg, "\x07")
}

// A branch with no trail is never found, so on a repository with more trails
// than the scan budget it reaches the budget without a match. That must pass
// (with a warning), or every bare-number update from main would be refused.
func TestProjectTrailUpdateBareNumberPassesWhenScanReachesBudget(t *testing.T) {
	setupGuardRepo(t)
	var patched int
	serveGuardProject(t, &patched)
	var page int
	serveBranchLookup(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		next := fmt.Sprintf("cursor-%d", page)
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{NextCursor: &next}))
	})

	_, errOut, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.NoError(t, err)
	require.Equal(t, 1, patched)
	require.Equal(t, trailFindMaxPages, page)
	require.Contains(t, errOut, `Warning: branch "feature/mine" is not among the 2000 most recently active trails in acme/widget`)
}
