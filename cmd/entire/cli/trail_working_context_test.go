package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestSelectTrailWorkingBranch(t *testing.T) {
	t.Parallel()
	changes := []api.ChangeSummary{{ID: "a", RepositoryID: "one", Branch: "feature/a"}, {ID: "b", RepositoryID: "one", Branch: "feature/b"}, {ID: "c", RepositoryID: "two", Branch: "feature/a"}}
	for _, tt := range []struct{ name, repo, explicit, preferred, id, failure string }{
		{"explicit wins", "one", "feature/b", "feature/a", "b", ""},
		{"checkout match", "one", "", "feature/a", "a", ""},
		{"sole branch", "two", "", "unrelated", "c", ""},
		{"ambiguous", "one", "", "unrelated", "", "select --branch: feature/a, feature/b"},
		{"foreign repository", "three", "", "feature/a", "", "no visible branches"},
		{"explicit missing", "one", "missing", "feature/a", "", "no visible branch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectTrailWorkingBranch(changes, tt.repo, tt.explicit, tt.preferred)
			if tt.failure != "" {
				require.ErrorContains(t, err, tt.failure)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.id, got.ID)
		})
	}
}

// Not parallel: replaces the repository client constructor with an isolated cell.
func setupWorkingRepoClient(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", server.URL), "repo-id", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })
}

func workingProjectTestResource() api.ProjectTrail {
	parent := projectTrailTestResource()
	parent.Changes = []api.ChangeSummary{{ID: projectTrailTestChange, RepositoryID: "repo-id", Repository: "widget", Branch: "feature/work"}}
	return parent
}

func serveWorkingProjectRead(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/gh/acme/trails":
		if err := json.NewEncoder(w).Encode(api.ProjectTrailListResponse{Items: []api.ProjectTrail{workingProjectTestResource()}}); err != nil {
			t.Errorf("encode project list: %v", err)
		}
	case projectTrailTestPath:
		w.Header().Set("ETag", `W/"parent-version"`)
		assert.NoError(t, json.NewEncoder(w).Encode(workingProjectTestResource()))
	case projectTrailTestPath + "/changes/" + projectTrailTestChange:
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": projectTrailTestID, "repositoryId": "repo-id", "branch": "feature/work", "status": "open"}))
	default:
		return false
	}
	return true
}

func TestWorkingTrailCommandsKeepProjectAndRepoNumbersSeparate(t *testing.T) {
	// Replaces constructors; neither this test nor its subtests may run in parallel.
	for _, tt := range []struct {
		args           []string
		method, suffix string
	}{
		{[]string{"approve", "42"}, http.MethodPost, "/approvals"},
		{[]string{"request-changes", "42", "-m", "Fix it"}, http.MethodPost, "/approvals"},
		{[]string{"approvals", "42"}, http.MethodGet, "/approvals"},
		{[]string{"finding", "list", "42", "--json"}, http.MethodGet, "/reviews/comments"},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if !serveWorkingProjectRead(t, w, r) {
					t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			calls := 0
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, tt.method, r.Method)
				assert.Equal(t, "/api/v1/trails/gh/acme/widget/7"+tt.suffix, r.URL.Path)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"approvals": []any{}, "comments": []any{}}))
			})
			args := append(append([]string{}, tt.args...), "--repo", "gh/acme/widget", "--branch", "feature/work")
			out, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			if tt.args[0] != "finding" {
				require.Contains(t, out, "trail #42")
				require.Contains(t, out, "feature/work")
				require.NotContains(t, out, "trail #7")
			}
		})
	}
}

func TestWorkingTrailFindingResolveUsesReviewIDFromCellPayload(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	var paths []string
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/trails/gh/acme/widget/7/reviews/comments":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"comments": []map[string]any{{"id": "finding-one", "review_id": "review-one", "status": "open"}},
			}))
		case "PATCH /api/v1/trails/gh/acme/widget/7/reviews/review-one/comments/finding-one":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"id": "finding-one", "review_id": "review-one", "status": "resolved",
			}))
		default:
			t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	out, _, err := executeProjectTrailTest(t, "finding", "resolve", "42", "finding-one", "--repo", "gh/acme/widget", "--branch", "feature/work")
	require.NoError(t, err)
	require.Contains(t, out, "open → resolved")
	require.Equal(t, []string{
		"GET /api/v1/trails/gh/acme/widget/7/reviews/comments",
		"PATCH /api/v1/trails/gh/acme/widget/7/reviews/review-one/comments/finding-one",
	}, paths)
}

func TestWorkingTrailRejectsChangedOwnershipBeforeWriting(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == projectTrailTestPath {
			assert.NoError(t, json.NewEncoder(w).Encode(workingProjectTestResource()))
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": projectTrailTestChange, "number": 7, "trailId": "different-parent", "repositoryId": "repo-id", "branch": "feature/work"}))
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("must not write after ownership changed: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	_, _, err := executeProjectTrailTest(t, "approve", projectTrailTestID, "--repo", "gh/acme/widget")
	require.ErrorContains(t, err, "does not match")
}

func TestWorkingTrailBranchDiscoveryDoesNotRequireProjectEnumeration(t *testing.T) {
	// Replaces routing constructors, so subtests cannot run in parallel.
	for _, missingParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "addressed parent", true: "missing parent"}[missingParent], func(t *testing.T) {
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if missingParent {
					t.Error("missing parent must not fall back to project discovery")
				}
				if !serveWorkingProjectRead(t, w, r) {
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			writes := 0
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes++
					assert.Equal(t, "/api/v1/trails/gh/acme/widget/7/approvals", r.URL.Path)
					w.WriteHeader(http.StatusCreated)
					return
				}
				var parent *api.TrailParentReference
				if !missingParent {
					parent = &api.TrailParentReference{ID: projectTrailTestID, ProjectID: projectTrailTestProject, Number: 42,
						Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}
				}
				assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: projectTrailTestChange, Number: 7, Branch: "feature/work", Parent: parent}}}))
			})
			_, _, err := executeProjectTrailTest(t, "approve", "--repo", "gh/acme/widget", "--branch", "feature/work")
			require.Zero(t, core.resolveCalls)
			if missingParent {
				require.ErrorContains(t, err, "no discoverable trail")
				require.Zero(t, writes)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, writes)
			}
		})
	}
}

func TestWorkingTrailUnlinkPreservesBranch(t *testing.T) {
	var methods []string
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if serveWorkingProjectRead(t, w, r) {
			return
		}
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, projectTrailTestPath+"/changes/"+projectTrailTestChange, r.URL.Path)
		assert.Equal(t, `W/"parent-version"`, r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusNoContent)
	})
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unlink must not mutate repository work: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	out, _, err := executeProjectTrailTest(t, "unlink", projectTrailTestID, "--repo", "gh/acme/widget", "--branch", "feature/work")
	require.NoError(t, err)
	require.Contains(t, out, "trail #42")
	require.Equal(t, []string{http.MethodGet, http.MethodGet, http.MethodDelete}, methods)
}
