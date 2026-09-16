package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/coreapi"
)

// Not parallel: replaces the repo-placement client constructor.
func setupProjectTrailRepoPlacement(t *testing.T) {
	t.Helper()
	fake := &fakeCellCore{
		clusters: []coreapi.Cluster{{Slug: "repo-cell", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://repo.example/api/v1")}},
		repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{{
			FullName: "acme/widget", Primaries: coreapi.NewOptRepoPrimaries(coreapi.RepoPrimaries{Processing: "repo-id"}),
			Placements: []coreapi.RepoPlacement{{ID: "repo-id", ClusterSlug: "repo-cell", Status: coreapi.RepoPlacementStatusReady}},
		}}},
	}
	old := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return fake, nil }
	t.Cleanup(func() { newCellCoreClient = old })
}

func TestProjectTrailCreateWithInitialChange(t *testing.T) {
	setupProjectTrailRepoPlacement(t)
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/gh/acme/trails", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("Idempotency-Key"))
		assert.Empty(t, r.Header.Get("If-Match"))
		var request api.ProjectTrailCreateRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		if !assert.Len(t, request.Changes, 1) {
			return
		}
		assert.Equal(t, "feature/new", request.Changes[0].BranchName)
		assert.Equal(t, "repo-id", request.Changes[0].RepositoryID)
		assert.Equal(t, "link", request.Changes[0].BranchAction)
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	_, _, err := executeProjectTrailTest(t, "create", "--project", "gh/acme", "--repo", "gh/acme/widget", "--title", "Intent", "--branch", "feature/new")
	require.NoError(t, err)
}

func TestProjectTrailCreateLinksCurrentBranchByDefault(t *testing.T) {
	// Not parallel: isolates git CWD and replaces project/repository constructors.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	testutil.RunGit(t, dir, "checkout", "-b", "feature/current")
	testutil.RunGit(t, dir, "remote", "add", "origin", "git@github.com:acme/widget.git")
	t.Chdir(dir)
	setupProjectTrailRepoPlacement(t)
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		var request api.ProjectTrailCreateRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		if !assert.Len(t, request.Changes, 1) {
			return
		}
		assert.Equal(t, "feature/current", request.Changes[0].BranchName)
		assert.Equal(t, "link", request.Changes[0].BranchAction)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	_, _, err := executeProjectTrailTest(t, "create", "--title", "Intent")
	require.NoError(t, err)
	require.Equal(t, "feature/current\n", testutil.RunGit(t, dir, "branch", "--show-current"))
}

func TestProjectTrailLinkRequiresParentAndRetryHeaders(t *testing.T) {
	setupProjectTrailRepoPlacement(t)
	var methods []string
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			assert.Equal(t, projectTrailTestPath, r.URL.Path)
			w.Header().Set("ETag", `W/"parent-version"`)
			assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			return
		}
		assert.Equal(t, projectTrailTestPath+"/changes", r.URL.Path)
		assert.Equal(t, `W/"parent-version"`, r.Header.Get("If-Match"))
		assert.Equal(t, "retry-link", r.Header.Get("Idempotency-Key"))
		var request api.ChangeCreateRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "link", request.BranchAction)
		assert.Equal(t, "feature/existing", request.BranchName)
		assert.Equal(t, "repo-id", request.RepositoryID)
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.NewEncoder(w).Encode(api.ChangeCreateResponse{ID: projectTrailTestChange, TrailID: projectTrailTestID, RepositoryID: "repo-id"}))
	})
	out, _, err := executeProjectTrailTest(t, "link", projectTrailTestID, "--project", "gh/acme", "--repo", "gh/acme/widget",
		"--title", "Work", "--branch", "feature/existing", "--idempotency-key", "retry-link", "--json")
	require.NoError(t, err)
	require.Contains(t, out, projectTrailTestID)
	require.Contains(t, out, "feature/existing")
	require.NotContains(t, out, projectTrailTestChange)
	require.Equal(t, []string{http.MethodGet, http.MethodPost}, methods)
}

func TestProjectTrailHasNoChangeEntityCommands(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"change", "attach", "detach", "delete"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			_, _, err := executeProjectTrailTest(t, verb)
			require.ErrorContains(t, err, "unknown command")
		})
	}
}

func TestProjectTrailParentPathValidation(t *testing.T) {
	t.Parallel()
	core := &fakeProjectTrailCore{}
	for _, path := range []string{"https://evil.example/trails/" + projectTrailTestID, "/api/v1/gh/other/trails/" + projectTrailTestID, ""} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			_, err := openProjectTrailTarget(context.Background(), core, api.TrailParentReference{
				ID: projectTrailTestID, ProjectID: projectTrailTestProject, Host: "gh", Project: "acme", Path: path,
			}, false)
			require.ErrorContains(t, err, "canonical path")
		})
	}
}
