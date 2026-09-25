package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	projectTrailTestID      = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	projectTrailTestProject = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	projectTrailTestChange  = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	projectTrailTestPath    = "/api/v1/gh/acme/trails/" + projectTrailTestID
)

type fakeProjectTrailCore struct {
	resolveCalls   int
	catalogCalls   int
	apiURL         string
	resolutionJSON string
	clusters       []coreapi.Cluster
}

func (f *fakeProjectTrailCore) ResolveProject(_ context.Context, params coreapi.ResolveProjectParams) (*coreapi.ResolveProjectOutputBody, error) {
	f.resolveCalls++
	resolved := struct {
		Project *struct {
			ID, Region, PrimaryProcessingCell, APIURL string
		} `json:"project"`
		Reference struct {
			Host, Project string
		} `json:"reference"`
	}{}
	body := f.resolutionJSON
	if body == "" {
		body = fmt.Sprintf(`{"project":{"id":%q,"region":"eu","primaryProcessingCell":"project-cell","apiUrl":%q},"reference":{"host":%q,"project":%q}}`, projectTrailTestProject, f.apiURL, params.Host, params.Project)
	}
	if err := json.Unmarshal([]byte(body), &resolved); err != nil {
		return nil, err
	}
	out := &coreapi.ResolveProjectOutputBody{
		Reference: coreapi.ProjectReference{Host: coreapi.ProjectReferenceHost(resolved.Reference.Host), Project: resolved.Reference.Project},
	}
	if resolved.Project != nil {
		out.Project = coreapi.Project{
			ID: resolved.Project.ID, Region: resolved.Project.Region,
			PrimaryProcessingCell: coreapi.NewOptString(resolved.Project.PrimaryProcessingCell),
			ApiUrl:                coreapi.NewOptString(resolved.Project.APIURL),
		}
	}
	return out, nil
}

func (f *fakeProjectTrailCore) ListClusters(context.Context) (*coreapi.ListClustersOutputBody, error) {
	f.catalogCalls++
	return &coreapi.ListClustersOutputBody{Clusters: f.clusters}, nil
}

// Tests using this fixture are not parallel: they replace client constructors.
// HTTP is confined to test servers, with no auth store or real API access.
func setupProjectTrailTest(t *testing.T, handler http.HandlerFunc) (*fakeProjectTrailCore, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	core := &fakeProjectTrailCore{apiURL: server.URL, clusters: []coreapi.Cluster{{Slug: "project-cell", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString(server.URL)}}}
	oldCore, oldCell := newProjectTrailCoreClient, newProjectTrailCellClient
	newProjectTrailCoreClient = func() (projectTrailCoreClient, error) { return core, nil }
	newProjectTrailCellClient = func(_ context.Context, _ bool, target *auth.CellTarget) (*api.Client, error) {
		require.Equal(t, "eu", target.Jurisdiction)
		require.Equal(t, server.URL, target.BaseURL)
		return api.NewClientWithBaseURL("project-token", target.BaseURL), nil
	}
	t.Cleanup(func() { newProjectTrailCoreClient, newProjectTrailCellClient = oldCore, oldCell })
	return core, server
}

func executeProjectTrailTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newTrailCmd()
	cmd.SilenceUsage = true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func projectTrailTestResource() api.ProjectTrail {
	return api.ProjectTrail{ID: projectTrailTestID, ProjectID: projectTrailTestProject, Number: 42,
		Title: "Project intent", Body: "Description", Status: "open", Assignees: []string{"alice"}, IsPossiblyPartial: true,
		Changes: []api.ChangeSummary{{ID: projectTrailTestChange, Number: 7, Branch: "feature/a", RepositoryID: "repo-a"}}}
}

func TestProjectTrailShowFollowsParentAcrossCells(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, projectTrailTestPath, r.URL.Path)
		assert.Equal(t, "Bearer project-token", r.Header.Get("Authorization"))
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	repoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails/gh/acme/widget", r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{
			ID: projectTrailTestChange, Number: 7, Branch: "feature/a", Title: "Code work",
			Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
				Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"},
		}}}))
	}))
	t.Cleanup(repoServer.Close)
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", repoServer.URL), "repo-id", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })
	out, _, err := executeProjectTrailTest(t, "show", "--repo", "gh/acme/widget", "--branch", "feature/a", "--json")
	require.NoError(t, err)
	var item api.ProjectTrail
	require.NoError(t, json.Unmarshal([]byte(out), &item))
	require.Equal(t, 42, item.Number)
	require.Equal(t, projectTrailTestID, item.ID)
	require.True(t, item.IsPossiblyPartial)
	require.Zero(t, core.resolveCalls, "repo-only parent reads must not resolve the project via project-view access")
}

func TestProjectTrailMissingParentDoesNotFallBack(t *testing.T) {
	_, server := setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{{ID: projectTrailTestChange, Number: 7, Branch: "feature/a"}}}))
	})
	old := newTrailAPIClient
	newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
		return api.NewClientWithBaseURL("repo-token", server.URL), "repo-id", nil
	}
	t.Cleanup(func() { newTrailAPIClient = old })
	out, _, err := executeProjectTrailTest(t, "show", "--repo", "gh/acme/widget", "--branch", "feature/a")
	require.ErrorContains(t, err, "no discoverable project trail")
	require.Empty(t, out)
}

func TestProjectTrailUpdateAtomicConditionalPatch(t *testing.T) {
	var methods []string
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		assert.Equal(t, projectTrailTestPath, r.URL.Path)
		if r.Method == http.MethodPatch {
			assert.Equal(t, `W/"precise-parent-version"`, r.Header.Get("If-Match"))
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]any{"title": "New intent", "body": "", "assignees": []any{"bob"}}, body)
		}
		w.Header().Set("ETag", `W/"precise-parent-version"`)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	_, _, err := executeProjectTrailTest(t, "update", projectTrailTestID, "--project", "gh/acme", "--title", "New intent", "--body=", "--remove-assignee", "alice", "--add-assignee", "bob")
	require.NoError(t, err)
	require.Equal(t, []string{http.MethodGet, http.MethodPatch}, methods)
}

func TestProjectTrailUpdateRefusesMissingETagAndConflicts(t *testing.T) {
	for _, etag := range []string{"", `W/"version"`} {
		t.Run(etag, func(t *testing.T) {
			writes := 0
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					writes++
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				w.Header().Set("ETag", etag)
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			})
			_, _, err := executeProjectTrailTest(t, "update", projectTrailTestID, "--project", "gh/acme", "--body", "new")
			if etag == "" {
				require.ErrorContains(t, err, "no ETag")
				require.Zero(t, writes)
			} else {
				require.ErrorContains(t, err, "changed since it was read")
				require.Equal(t, 1, writes, "must not retry without precondition")
			}
		})
	}
}

func TestProjectTrailCreateWithoutRepo(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/gh/acme/trails", r.URL.Path)
		assert.Equal(t, "retry-me", r.Header.Get("Idempotency-Key"))
		assert.Empty(t, r.Header.Get("If-Match"))
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]any{"title": "Intent", "body": "Plan"}, body)
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
	})
	out, errOut, err := executeProjectTrailTest(t, "create", "--no-branch", "--project", "gh/acme", "--title", "Intent", "--body", "Plan", "--idempotency-key", "retry-me", "--json")
	require.NoError(t, err)
	require.Contains(t, errOut, "retry-me")
	require.Contains(t, out, projectTrailTestID)
}

// Numeric selectors remain project-scoped even though user-facing list is global.
func TestProjectTrailNumberSelector(t *testing.T) {
	pages := 0
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == projectTrailTestPath {
			assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			return
		}
		pages++
		assert.Equal(t, "/api/v1/gh/acme/trails", r.URL.Path)
		assert.Empty(t, r.URL.Query().Get("status"), "project API has no status filter")
		switch r.URL.Query().Get("pageToken") {
		case "":
			next := "page-two"
			assert.NoError(t, json.NewEncoder(w).Encode(api.ProjectTrailListResponse{Items: []api.ProjectTrail{}, NextPageToken: &next}))
		case "page-two":
			assert.NoError(t, json.NewEncoder(w).Encode(api.ProjectTrailListResponse{Items: []api.ProjectTrail{projectTrailTestResource()}}))
		default:
			t.Errorf("unexpected cursor: %s", r.URL.RawQuery)
		}
	})
	out, _, err := executeProjectTrailTest(t, "show", "42", "--project", "gh/acme", "--json")
	require.NoError(t, err)
	require.Contains(t, out, projectTrailTestID)
	require.Equal(t, 2, pages)
}

func TestProjectTrailCellRoutingFailsClosed(t *testing.T) {
	t.Parallel()
	clusters := []coreapi.Cluster{{Slug: "project-cell", Jurisdiction: "eu", ApiUrl: coreapi.NewOptString("https://project.example/api/v1")}}
	for _, tt := range []struct{ name, cell, jurisdiction string }{
		{"missing cell", "", "eu"}, {"missing jurisdiction", "project-cell", ""},
		{"wrong jurisdiction", "project-cell", "us"}, {"unknown cell", "other", "eu"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := projectTrailCellTarget(clusters, tt.cell, tt.jurisdiction)
			require.Error(t, err)
		})
	}
}

func TestProjectTrailValidationBeforeIO(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"create", "--title", ""}, {"create", "--title", "Intent", "--status", "merged"},
		{"update", "--status", "merged"}, {"update"}, {"show", "feature/a"},
		{"show", projectTrailTestID, "extra"},
		{"create", "--title", "Intent", "--no-branch", "--base", "main"},
		{"create", "--title", "Intent", "--no-branch", "--branch", "work"},
		{"create", "--title", "Intent", "--no-branch", "--branch-action", "create"},
		{"link"},
		{"unlink", projectTrailTestID, "7"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			t.Parallel()
			_, _, err := executeProjectTrailTest(t, args...)
			require.Error(t, err)
		})
	}
}
