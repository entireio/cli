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
	"github.com/entireio/cli/internal/coreapi"
)

// These fixtures replace routing constructors; their callers cannot run in parallel.
func setupRepositoryTrailCreation(t *testing.T, forge string, handler http.HandlerFunc) (*fakeCellCore, *fakeProjectTrailCore) {
	t.Helper()
	project, server := setupProjectTrailTest(t, handler)
	core := &fakeCellCore{repo: &coreapi.Repo{ID: projectTrailTestChange, OwningProjectId: projectTrailTestProject}}
	oldCore, oldTrail := newCellCoreClient, newTrailAPIClient
	newCellCoreClient = func() (cellCoreClient, error) { return core, nil }
	newTrailAPIClient = func(_ context.Context, _ bool, host, owner, repo string) (*api.Client, string, error) {
		assert.Equal(t, forge, host)
		assert.Equal(t, "acme", owner)
		assert.Equal(t, "widget", repo)
		return api.NewClientWithBaseURL("repo-token", server.URL), projectTrailTestChange, nil
	}
	t.Cleanup(func() { newCellCoreClient, newTrailAPIClient = oldCore, oldTrail })
	return core, project
}

func TestProjectTrailCreateRepositoryScope(t *testing.T) {
	for _, tt := range []struct {
		name, forge string
		args        []string
	}{
		{"github", "gh", []string{"--repo", "gh/acme/widget"}},
		{"native", "et", []string{"--repo", "et/acme/widget"}},
		{"explicit project", "gh", []string{"--project", "gh/acme", "--repo", "gh/acme/widget"}},
		{"current repository", "gh", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.args == nil {
				dir, _ := setupProjectTrailPublication(t)
				writeTrailCreatePrePushHook(t, dir, "#!/bin/sh\necho unexpected-push >&2\nexit 1\n")
			}
			calls := 0
			_, project := setupRepositoryTrailCreation(t, tt.forge, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/"+tt.forge+"/acme/trails", r.URL.Path)
				assert.Equal(t, projectTrailTestProject, r.URL.Query().Get("project_id"))
				assert.Equal(t, "Bearer repo-token", r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("Idempotency-Key"))
				var request api.ProjectTrailCreateRequest
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				assert.Equal(t, api.ProjectTrailCreateRequest{
					Title: "Scoped intent", Body: "Plan", Status: "open", Type: "task", Priority: "high",
					Assignees: []string{"alice"}, RepositoryIDs: []string{projectTrailTestChange},
				}, request)
				out := projectTrailTestResource()
				out.RepositoryIDs, out.Changes = request.RepositoryIDs, nil
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.NewEncoder(w).Encode(out))
			})
			args := append([]string{"create", "--no-branch", "--title", "Scoped intent", "--body", "Plan", "--status", "open", "--type", "task", "--priority", "high", "--assignee", "alice", "--json"}, tt.args...)
			out, errOut, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Zero(t, project.resolveCalls, "repository-only users must not require project inspection")
			require.Zero(t, project.catalogCalls)
			require.NotContains(t, errOut, "Idempotency")
			require.NotContains(t, errOut, "unexpected-push")
			require.Contains(t, out, projectTrailTestID)
		})
	}
}

func TestProjectTrailCreateRepositoryScopeRejectsOptions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, want string
		args       []string
	}{
		{"foreign project", "must match", []string{"--project", "gh/other"}},
		{"different forge", "must match", []string{"--project", "et/acme"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"create", "--no-branch", "--title", "Intent", "--repo", "gh/acme/widget"}, tt.args...)
			_, _, err := executeProjectTrailTest(t, args...)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestProjectTrailCreateRepositoryScopeExplicitIdempotency(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "retry-me", r.Header.Get("Idempotency-Key"))
				var request api.ProjectTrailCreateRequest
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				assert.Equal(t, []string{projectTrailTestChange}, request.RepositoryIDs)
				assert.Empty(t, request.Changes)
				if status == http.StatusForbidden {
					http.Error(w, "Project management is required for initial Changes or idempotent batch creation", status)
					return
				}
				w.WriteHeader(status)
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			})
			_, errOut, err := executeProjectTrailTest(t, "create", "--no-branch", "--repo", "gh/acme/widget", "--title", "Intent", "--idempotency-key", "retry-me")
			if status == http.StatusForbidden {
				require.ErrorContains(t, err, "Project management is required")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, calls, "never downgrade a denied idempotent request")
			require.Contains(t, errOut, "Idempotency key: retry-me")
		})
	}
}

func TestProjectTrailCreateRepositoryScopeRefusesUnknownOwnership(t *testing.T) {
	for _, tt := range []struct {
		name string
		repo *coreapi.Repo
		err  error
		want string
	}{
		{"denied", nil, errors.New("repository access denied"), "repository access denied"},
		{"missing response", nil, nil, "identity does not match"},
		{"wrong repository", &coreapi.Repo{ID: projectTrailTestID, OwningProjectId: projectTrailTestProject}, nil, "identity does not match"},
		{"missing owner", &coreapi.Repo{ID: projectTrailTestChange}, nil, "valid project ID"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, project := setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected creation: %s %s", r.Method, r.URL)
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			})
			core.repo, core.repoErr = tt.repo, tt.err
			_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--repo", "gh/acme/widget", "--title", "Intent")
			require.ErrorContains(t, err, tt.want)
			require.Zero(t, project.resolveCalls)
		})
	}
}

func TestProjectTrailCreateRepositoryScopeDoesNotRetry(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, _ *http.Request) {
				calls++
				http.Error(w, "creation refused", status)
			})
			_, errOut, err := executeProjectTrailTest(t, "create", "--no-branch", "--repo", "gh/acme/widget", "--title", "Intent")
			require.Error(t, err)
			require.Equal(t, 1, calls)
			require.NotContains(t, err.Error(), "retry with --idempotency-key")
			require.NotContains(t, errOut, "Idempotency")
		})
	}
}

func TestProjectTrailCreateRepositoryScopeChecksResponseOwner(t *testing.T) {
	setupRepositoryTrailCreation(t, "gh", func(w http.ResponseWriter, _ *http.Request) {
		out := projectTrailTestResource()
		out.ProjectID = projectTrailTestChange
		assert.NoError(t, json.NewEncoder(w).Encode(out))
	})
	_, _, err := executeProjectTrailTest(t, "create", "--no-branch", "--repo", "gh/acme/widget", "--title", "Intent")
	require.ErrorContains(t, err, "identity does not match")
}
