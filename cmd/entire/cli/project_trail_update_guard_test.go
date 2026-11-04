package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

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
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails/gh/acme/widget", r.URL.Path)
		list := api.TrailListResponse{}
		if work != nil {
			list.Trails, list.Total = []api.TrailResource{*work}, 1
		}
		assert.NoError(t, json.NewEncoder(w).Encode(list))
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
		"To update this branch's trail: entire trail update (no selector).",
		"To update #42 anyway: entire trail update " + projectTrailTestID + " --project gh/acme",
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

	require.ErrorContains(t, err, "42 is this branch's change number, and a bare number names a project trail. For the change, run: entire trail update widget/42")
	require.Zero(t, patched)
}

func TestProjectTrailUpdateBareNumberPassesWhenItCannotMisfire(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		work      *api.TrailResource
		lookupErr error
		stderr    string
	}{
		{name: "branch work is on this trail", args: []string{"update", "42", "--body", "x"},
			work: branchWorkOn(42, 7, projectTrailTestID)},
		{name: "branch has no trail", args: []string{"update", "42", "--body", "x"}},
		{name: "branch work has no project trail", args: []string{"update", "42", "--body", "x"},
			work: &api.TrailResource{ID: "01ARZ3NDEKTSV4RRFFQ69G5FBB", Number: 7, Branch: "feature/mine", Status: "open"}},
		{name: "lookup fails", args: []string{"update", "42", "--body", "x"},
			lookupErr: errors.New("repository not onboarded"), stderr: "Warning: could not check which trail branch \"feature/mine\" belongs to"},
		{name: "ULID names the trail exactly", args: []string{"update", projectTrailTestID, "--project", "gh/acme", "--body", "x"},
			work: branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC")},
		{name: "another project", args: []string{"update", "42", "--project", "gh/other", "--body", "x"},
			work: branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			var patched int
			serveGuardProject(t, &patched)
			serveBranchWork(t, tt.work, tt.lookupErr)

			_, errOut, err := executeProjectTrailTest(t, tt.args...)

			require.NoError(t, err)
			require.Equal(t, 1, patched)
			if tt.stderr != "" {
				require.Contains(t, errOut, tt.stderr)
			}
		})
	}
}

func TestProjectTrailNumberMismatchSanitizesServerText(t *testing.T) {
	t.Parallel()
	current := api.ProjectTrail{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV\x1b]0;x\x07", Title: "T\x1b[2J"}
	msg := projectTrailNumberMismatch(42, current, &projectTrailTarget{Host: "gh", Project: "acme"}, "feature/mine", "widget",
		branchWorkOn(43, 7, "01ARZ3NDEKTSV4RRFFQ69G5FCC"))
	require.NotContains(t, msg, "\x1b")
	require.NotContains(t, msg, "\x07")
}
