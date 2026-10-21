package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

func TestProjectTrailCreateWithChange(t *testing.T) {
	for _, forge := range []string{"gh", "et"} {
		t.Run(forge, func(t *testing.T) {
			var methods []string
			handler := repositoryTrailCreationHandler(t, forge, func(change api.ChangeCreateRequest) {
				assert.Equal(t, "feature/new", change.BranchName)
				assert.Equal(t, "Intent", change.Title)
				assert.Equal(t, "link", change.BranchAction)
			})
			_, project := setupRepositoryTrailCreation(t, forge, func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/changes") {
					assert.Equal(t, "change-key", r.Header.Get("Idempotency-Key"))
				}
				handler(w, r)
			})
			_, _, err := executeProjectTrailTest(t, "create", "--project", forge+"/acme", "--repo", forge+"/acme/widget", "--title", "Intent", "--branch", "feature/new", "--idempotency-key", "change-key")
			require.NoError(t, err)
			require.Zero(t, project.resolveCalls)
			require.Equal(t, []string{http.MethodPost, http.MethodPost, http.MethodGet}, methods)
		})
	}
}

func TestProjectTrailCreateLinksCurrentBranchByDefault(t *testing.T) {
	// Not parallel: isolates git CWD and replaces project/repository constructors.
	dir, remote := setupProjectTrailPublication(t)
	testutil.RunGit(t, dir, "checkout", "-b", "feature/current")
	setupRepositoryTrailCreation(t, "gh", repositoryTrailCreationHandler(t, "gh", func(change api.ChangeCreateRequest) {
		assert.Equal(t, "feature/current", change.BranchName)
		assert.Equal(t, "link", change.BranchAction)
	}))
	_, _, err := executeProjectTrailTest(t, "create", "--title", "Intent")
	require.NoError(t, err)
	require.Equal(t, "feature/current\n", testutil.RunGit(t, dir, "branch", "--show-current"))
	require.True(t, gitBranchExistsTrailTest(t, remote, "feature/current"))
}

func TestProjectTrailLinkRequiresParentAndRetryHeaders(t *testing.T) {
	var methods []string
	_, project := setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, r *http.Request) {
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
		assert.Equal(t, projectTrailTestChange, request.RepositoryID)
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.NewEncoder(w).Encode(api.ChangeCreateResponse{ID: projectTrailTestChange, TrailID: projectTrailTestID, RepositoryID: projectTrailTestChange}))
	})
	out, _, err := executeProjectTrailTest(t, "link", projectTrailTestID, "--project", "gh/acme", "--repo", "gh/acme/widget",
		"--title", "Work", "--branch", "feature/existing", "--idempotency-key", "retry-link", "--json")
	require.NoError(t, err)
	require.Contains(t, out, projectTrailTestID)
	require.Contains(t, out, "feature/existing")
	require.NotContains(t, out, `"id":`)
	require.Equal(t, []string{http.MethodGet, http.MethodPost}, methods)
	require.Zero(t, project.resolveCalls)
}

func TestProjectTrailLinkSelectorGuidanceMatchesAcceptedForms(t *testing.T) {
	t.Parallel()
	_, _, err := executeProjectTrailTest(t, "link", "widget/7", "--project", "gh/acme", "--repo", "gh/acme/widget", "--branch", "feature/work")
	require.ErrorContains(t, err, "use a project trail ID or number")
	require.NotContains(t, err.Error(), "<repo>/<number>")
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
