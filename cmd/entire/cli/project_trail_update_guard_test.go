package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const guardRepoID = "01ARZ3NDEKTSV4RRFFQ69G5FRP"

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

// serveGuardProject serves project trail #42, records each PATCH query, and
// answers PATCHes with patch.
func serveGuardProject(t *testing.T, patch http.HandlerFunc) *[]url.Values {
	t.Helper()
	var patches []url.Values
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches = append(patches, r.URL.Query())
			if patch != nil {
				patch(w, r)
				return
			}
		}
		w.Header().Set("ETag", `W/"v1"`)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
		return repoCellPlacement{RepoID: guardRepoID}, nil
	})
	return &patches
}

func stubGuardPlacement(t *testing.T, fn func(context.Context, string, string, string) (repoCellPlacement, error)) {
	t.Helper()
	old := resolveTrailUpdatePlacement
	resolveTrailUpdatePlacement = fn
	t.Cleanup(func() { resolveTrailUpdatePlacement = old })
}

// A bare number names a project trail, which can belong to someone else: a
// change number (cli/1744) or pull request number typed as `update 1744`
// reached unrelated project trail #1744 and replaced its description. The
// update now carries the branch, and the server refuses it atomically.
func TestProjectTrailUpdateBareNumberSendsTheBranch(t *testing.T) {
	setupGuardRepo(t)
	patches := serveGuardProject(t, nil)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.NoError(t, err)
	require.Equal(t, []url.Values{{"from_repo_id": {guardRepoID}, "from_branch": {"feature/mine"}, "branch_check": {"required"}}}, *patches)
}

// The server's refusal is shown on its own, sanitized to one line since it
// echoes the branch name, followed by the CLI's reruns.
func TestProjectTrailUpdateBareNumberShowsTheServerRefusal(t *testing.T) {
	setupGuardRepo(t)
	refusal := "Branch \"feature/mine\x1b]0;x\x07\" has work on another Trail, not on #42, so nothing was updated.\nTo update this branch's own Trail, leave out the number."
	serveGuardProject(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": refusal, "code": "trail_branch_mismatch"}))
	})

	_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.Error(t, err)
	require.Contains(t, err.Error(), "has work on another Trail, not on #42, so nothing was updated. To update this branch's own Trail, leave out the number.")
	require.NotContains(t, err.Error(), "\x1b")
	require.NotContains(t, err.Error(), "\x07")
	require.NotContains(t, err.Error(), "API error", "the refusal stands on its own")
	require.Contains(t, err.Error(), "\nTo update this branch's trail, rerun without 42. To update #42 anyway, rerun with "+projectTrailTestID+" --project gh/acme in place of 42.")
}

// Another 409 keeps its usual error.
func TestProjectTrailUpdateBareNumberPassesOtherConflictsThrough(t *testing.T) {
	setupGuardRepo(t)
	serveGuardProject(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": "refused", "code": "conflict"}))
	})

	_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

	require.ErrorContains(t, err, "update project trail: project trail request: API error: refused (status 409)")
}

// Where the branch does not decide which trail the number means, or no trail
// work can exist, nothing is sent.
func TestProjectTrailUpdateSendsNoBranchWhenItCannotMisfire(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		placement error
	}{
		{name: "ULID names the trail exactly", args: []string{"update", projectTrailTestID, "--project", "gh/acme", "--body", "x"}},
		{name: "another project", args: []string{"update", "42", "--project", "gh/other", "--body", "x"}},
		{name: "--repo names another repository", args: []string{"update", "42", "--repo", "gh/acme/other", "--body", "x"}},
		{name: "repository not onboarded", args: []string{"update", "42", "--body", "x"}, placement: &repoNotOnboardedError{inner: errors.New("not found")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			patches := serveGuardProject(t, nil)
			if tt.placement != nil {
				stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
					return repoCellPlacement{}, tt.placement
				})
			}

			_, _, err := executeProjectTrailTest(t, tt.args...)

			require.NoError(t, err)
			require.Len(t, *patches, 1)
			require.Empty(t, (*patches)[0])
		})
	}
}

// --repo naming this clone's repository still targets #42 by bare number.
func TestProjectTrailUpdateBareNumberWithThisRepoSendsTheBranch(t *testing.T) {
	setupGuardRepo(t)
	patches := serveGuardProject(t, nil)

	_, _, err := executeProjectTrailTest(t, "update", "42", "--repo", "gh/acme/widget", "--body", "x")

	require.NoError(t, err)
	require.Len(t, *patches, 1)
	require.Equal(t, "feature/mine", (*patches)[0].Get("from_branch"))
}

// Without a branch, or a repository id, nothing checkable can be sent, so the
// update is not attempted.
func TestProjectTrailUpdateBareNumberRefusesWhenNothingCanBeSent(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{name: "detached HEAD", setup: func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "checkout", "--detach")
		}, want: `refusing to update trail #42 "Project intent": the checked-out branch is unknown (not on a branch (detached HEAD)), so the server cannot check that this work belongs to #42; rerun with --branch <branch> added, or with ` + projectTrailTestID + " --project gh/acme in place of 42"},
		{name: "unborn branch", setup: func(t *testing.T) {
			t.Helper()
			testutil.RunGit(t, ".", "checkout", "--orphan", "fresh")
		}, want: "the checked-out branch is unknown (failed to get HEAD:"},
		{name: "repository does not resolve", setup: func(t *testing.T) {
			t.Helper()
			stubGuardPlacement(t, func(context.Context, string, string, string) (repoCellPlacement, error) {
				return repoCellPlacement{}, errors.New("core unavailable")
			})
		}, want: "resolve widget for the trail's branch check: core unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupGuardRepo(t)
			patches := serveGuardProject(t, nil)
			tt.setup(t)

			_, _, err := executeProjectTrailTest(t, "update", "42", "--body", "x")

			require.ErrorContains(t, err, tt.want)
			require.Empty(t, *patches)
		})
	}
}
